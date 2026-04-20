//go:build linux

package probe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

const (
	mtuPollMaxTimeout       = 200 * time.Millisecond
	mtuErrorQueuePayloadLen = 2048
	mtuErrorQueueOOBLen     = 1024
	mtuReadBufferLen        = 2048
	sockExtendedErrLen      = 16
)

type pingSocketMTUBackend struct{}

type mtuErrQueueEvent struct {
	status mtuPayloadStatus
	err    error
}

type sockExtendedErr struct {
	errno  uint32
	origin uint8
	typ    uint8
	code   uint8
	info   uint32
	data   uint32
}

func defaultMTUBackend() mtuProbeBackend {
	return pingSocketMTUBackend{}
}

func (pingSocketMTUBackend) probeSmallEcho(
	ctx context.Context,
	dst *net.IPAddr,
	payloadSize int,
	seq int,
) mtuPayloadResult {
	return probePingSocket(ctx, dst, payloadSize, seq)
}

func (pingSocketMTUBackend) probePayloadWithDF(
	ctx context.Context,
	dst *net.IPAddr,
	payloadSize int,
	seq int,
) mtuPayloadResult {
	return probePingSocket(ctx, dst, payloadSize, seq)
}

func probePingSocket(ctx context.Context, dst *net.IPAddr, payloadSize int, seq int) mtuPayloadResult {
	result := mtuPayloadResult{
		status:      mtuPayloadError,
		payloadSize: payloadSize,
	}

	fd, err := openMTUPingSocket(dst)
	if err != nil {
		result.err = err
		return result
	}
	defer func() { _ = unix.Close(fd) }()

	msg := &icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{
			ID:   0,
			Seq:  seq,
			Data: make([]byte, payloadSize),
		},
	}

	wire, err := msg.Marshal(nil)
	if err != nil {
		result.err = fmt.Errorf("marshal icmp: %w", err)
		return result
	}

	if _, err := unix.Write(fd, wire); err != nil {
		if errors.Is(err, syscall.EMSGSIZE) {
			_, _ = drainMTUErrorQueue(fd, seq)
			result.status = mtuPayloadLocalTooLarge
			result.err = err
			return result
		}
		result.err = err
		return result
	}

	readBuf := make([]byte, mtuReadBufferLen)
	for {
		if ctx.Err() != nil {
			result.status = mtuPayloadTimeout
			result.err = ctx.Err()
			return result
		}

		timeout, expired := pollTimeout(ctx)
		if expired {
			result.status = mtuPayloadTimeout
			result.err = context.DeadlineExceeded
			return result
		}

		pollFDs := []unix.PollFd{{
			Fd:     int32(fd),
			Events: int16(unix.POLLIN | unix.POLLERR),
		}}
		n, err := unix.Poll(pollFDs, timeout)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			result.err = fmt.Errorf("poll: %w", err)
			return result
		}
		if ctx.Err() != nil {
			result.status = mtuPayloadTimeout
			result.err = ctx.Err()
			return result
		}
		if n == 0 {
			continue
		}

		revents := pollFDs[0].Revents
		if revents&int16(unix.POLLERR) != 0 {
			event, ok := drainMTUErrorQueue(fd, seq)
			if ok {
				result.status = event.status
				result.err = event.err
				return result
			}
		}

		if revents&int16(unix.POLLIN) == 0 {
			continue
		}

		readN, err := unix.Read(fd, readBuf)
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
				continue
			}
			result.err = fmt.Errorf("read icmp: %w", err)
			return result
		}
		if echoReplyMatchesSequence(readBuf[:readN], seq) {
			result.status = mtuPayloadSuccess
			return result
		}
	}
}

func openMTUPingSocket(dst *net.IPAddr) (int, error) {
	ip4 := dst.IP.To4()
	if ip4 == nil {
		return -1, fmt.Errorf("destination is not IPv4")
	}

	var addr [4]byte
	copy(addr[:], ip4)

	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.IPPROTO_ICMP)
	if err != nil {
		return -1, err
	}
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_RECVERR, 1); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("set IP_RECVERR: %w", err)
	}
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_MTU_DISCOVER, unix.IP_PMTUDISC_PROBE); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("set IP_MTU_DISCOVER: %w", err)
	}
	if err := unix.Connect(fd, &unix.SockaddrInet4{Addr: addr}); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("connect ping socket: %w", err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("set nonblock: %w", err)
	}
	return fd, nil
}

func pollTimeout(ctx context.Context) (int, bool) {
	timeout := mtuPollMaxTimeout
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, true
		}
		if remaining < timeout {
			timeout = remaining
		}
	}
	return int(timeout.Milliseconds()), false
}

func drainMTUErrorQueue(fd int, seq int) (mtuErrQueueEvent, bool) {
	payloadBuf := make([]byte, mtuErrorQueuePayloadLen)
	oobBuf := make([]byte, mtuErrorQueueOOBLen)

	for {
		n, oobn, flags, _, err := unix.Recvmsg(fd, payloadBuf, oobBuf, unix.MSG_ERRQUEUE)
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
				return mtuErrQueueEvent{}, false
			}
			return mtuErrQueueEvent{status: mtuPayloadError, err: fmt.Errorf("recvmsg error queue: %w", err)}, true
		}
		if flags&unix.MSG_CTRUNC != 0 {
			continue
		}

		quotedComplete := flags&unix.MSG_TRUNC == 0
		event, ok := classifyMTUErrorQueueMessage(oobBuf[:oobn], payloadBuf[:n], quotedComplete, seq)
		if ok {
			return event, true
		}
	}
}

func classifyMTUErrorQueueMessage(oob []byte, quoted []byte, quotedComplete bool, seq int) (mtuErrQueueEvent, bool) {
	cmsgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return mtuErrQueueEvent{status: mtuPayloadError, err: fmt.Errorf("parse socket control message: %w", err)}, true
	}

	for _, cmsg := range cmsgs {
		if cmsg.Header.Level != unix.IPPROTO_IP || cmsg.Header.Type != unix.IP_RECVERR {
			continue
		}
		ee, ok := parseSockExtendedErr(cmsg.Data)
		if !ok {
			return mtuErrQueueEvent{}, false
		}
		if quotedComplete && quotedPacketHasWrongSequence(quoted, seq) {
			return mtuErrQueueEvent{}, false
		}

		switch {
		case ee.origin == unix.SO_EE_ORIGIN_ICMP && ee.typ == 3 && ee.code == 4 && errors.Is(syscall.Errno(ee.errno), syscall.EMSGSIZE):
			return mtuErrQueueEvent{status: mtuPayloadTooLarge, err: syscall.EMSGSIZE}, true
		case ee.origin == unix.SO_EE_ORIGIN_LOCAL && errors.Is(syscall.Errno(ee.errno), syscall.EMSGSIZE):
			return mtuErrQueueEvent{status: mtuPayloadLocalTooLarge, err: syscall.EMSGSIZE}, true
		case ee.origin == unix.SO_EE_ORIGIN_ICMP && ee.typ == 3:
			return mtuErrQueueEvent{status: mtuPayloadUnreachable, err: syscall.Errno(ee.errno)}, true
		default:
			return mtuErrQueueEvent{status: mtuPayloadError, err: fmt.Errorf("unexpected error queue event: origin=%d type=%d code=%d errno=%d", ee.origin, ee.typ, ee.code, ee.errno)}, true
		}
	}
	return mtuErrQueueEvent{}, false
}

func parseSockExtendedErr(data []byte) (sockExtendedErr, bool) {
	if len(data) < sockExtendedErrLen {
		return sockExtendedErr{}, false
	}
	return sockExtendedErr{
		errno:  binary.NativeEndian.Uint32(data[0:4]),
		origin: data[4],
		typ:    data[5],
		code:   data[6],
		info:   binary.NativeEndian.Uint32(data[8:12]),
		data:   binary.NativeEndian.Uint32(data[12:16]),
	}, true
}

func echoReplyMatchesSequence(data []byte, seq int) bool {
	reply, err := icmp.ParseMessage(1, data)
	if err != nil || reply.Type != ipv4.ICMPTypeEchoReply {
		return false
	}
	echo, ok := reply.Body.(*icmp.Echo)
	return ok && echo.Seq == seq
}

func quotedPacketHasWrongSequence(data []byte, seq int) bool {
	if len(data) < 8 || data[0] != byte(ipv4.ICMPTypeEcho) || data[1] != 0 {
		return false
	}
	quotedSeq := int(binary.BigEndian.Uint16(data[6:8]))
	return quotedSeq != seq
}
