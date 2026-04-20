//go:build linux

package probe

import (
	"encoding/binary"
	"errors"
	"syscall"
	"testing"

	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

func TestClassifyMTUErrorQueueMessage(t *testing.T) {
	const seq = 1472
	tests := []struct {
		name           string
		ee             sockExtendedErr
		quoted         []byte
		quotedComplete bool
		wantOK         bool
		wantStatus     mtuPayloadStatus
		wantErr        error
	}{
		{
			name: "fragmentation_needed",
			ee: sockExtendedErr{
				errno:  uint32(syscall.EMSGSIZE),
				origin: unix.SO_EE_ORIGIN_ICMP,
				typ:    3,
				code:   4,
			},
			quoted:         quotedEcho(seq),
			quotedComplete: true,
			wantOK:         true,
			wantStatus:     mtuPayloadTooLarge,
			wantErr:        syscall.EMSGSIZE,
		},
		{
			name: "local_emsgsize_no_offender",
			ee: sockExtendedErr{
				errno:  uint32(syscall.EMSGSIZE),
				origin: unix.SO_EE_ORIGIN_LOCAL,
			},
			quotedComplete: false,
			wantOK:         true,
			wantStatus:     mtuPayloadLocalTooLarge,
			wantErr:        syscall.EMSGSIZE,
		},
		{
			name: "destination_unreachable_other_code",
			ee: sockExtendedErr{
				errno:  uint32(syscall.EHOSTUNREACH),
				origin: unix.SO_EE_ORIGIN_ICMP,
				typ:    3,
				code:   1,
			},
			quoted:         quotedEcho(seq),
			quotedComplete: true,
			wantOK:         true,
			wantStatus:     mtuPayloadUnreachable,
			wantErr:        syscall.EHOSTUNREACH,
		},
		{
			name: "unknown_event",
			ee: sockExtendedErr{
				errno:  uint32(syscall.EINVAL),
				origin: 99,
				typ:    99,
				code:   99,
			},
			quotedComplete: false,
			wantOK:         true,
			wantStatus:     mtuPayloadError,
		},
		{
			name: "wrong_quoted_sequence_complete",
			ee: sockExtendedErr{
				errno:  uint32(syscall.EMSGSIZE),
				origin: unix.SO_EE_ORIGIN_ICMP,
				typ:    3,
				code:   4,
			},
			quoted:         quotedEcho(seq + 1),
			quotedComplete: true,
			wantOK:         false,
		},
		{
			name: "wrong_quoted_sequence_truncated",
			ee: sockExtendedErr{
				errno:  uint32(syscall.EMSGSIZE),
				origin: unix.SO_EE_ORIGIN_ICMP,
				typ:    3,
				code:   4,
			},
			quoted:         quotedEcho(seq + 1),
			quotedComplete: false,
			wantOK:         true,
			wantStatus:     mtuPayloadTooLarge,
			wantErr:        syscall.EMSGSIZE,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event, ok := classifyMTUErrorQueueMessage(ipRecvErrOOB(t, encodeSockExtendedErr(tt.ee)), tt.quoted, tt.quotedComplete, seq)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if event.status != tt.wantStatus {
				t.Fatalf("status = %q, want %q (err=%v)", event.status, tt.wantStatus, event.err)
			}
			if tt.wantErr != nil && !errors.Is(event.err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", event.err, tt.wantErr)
			}
		})
	}
}

func TestClassifyMTUErrorQueueMessageIgnoresShortExtendedErr(t *testing.T) {
	event, ok := classifyMTUErrorQueueMessage(ipRecvErrOOB(t, make([]byte, sockExtendedErrLen-1)), nil, false, 1472)
	if ok {
		t.Fatalf("ok = true, want false; event=%+v", event)
	}
}

func TestClassifyMTUErrorQueueMessageIgnoresUnrelatedControlMessage(t *testing.T) {
	event, ok := classifyMTUErrorQueueMessage(socketControlOOB(t, unix.SOL_SOCKET, unix.SO_ERROR, []byte{1, 2, 3, 4}), nil, false, 1472)
	if ok {
		t.Fatalf("ok = true, want false; event=%+v", event)
	}
}

func TestParseSockExtendedErr(t *testing.T) {
	data := make([]byte, 24)
	binary.NativeEndian.PutUint32(data[0:4], uint32(syscall.EMSGSIZE))
	data[4] = unix.SO_EE_ORIGIN_ICMP
	data[5] = 3
	data[6] = 4
	binary.NativeEndian.PutUint32(data[8:12], 1490)
	binary.NativeEndian.PutUint32(data[12:16], 7)

	got, ok := parseSockExtendedErr(data)
	if !ok {
		t.Fatal("parseSockExtendedErr returned ok=false")
	}
	if got.errno != uint32(syscall.EMSGSIZE) || got.origin != unix.SO_EE_ORIGIN_ICMP || got.typ != 3 || got.code != 4 || got.info != 1490 || got.data != 7 {
		t.Fatalf("parsed extended err = %+v", got)
	}
}

func TestParseSockExtendedErrTooShort(t *testing.T) {
	if _, ok := parseSockExtendedErr(make([]byte, sockExtendedErrLen-1)); ok {
		t.Fatal("parseSockExtendedErr returned ok=true for short data")
	}
}

func TestQuotedPacketHasWrongSequence(t *testing.T) {
	const seq = 1472
	quoted := make([]byte, 8)
	quoted[0] = byte(ipv4.ICMPTypeEcho)
	quoted[1] = 0
	binary.BigEndian.PutUint16(quoted[6:8], seq)

	if quotedPacketHasWrongSequence(quoted, seq) {
		t.Fatal("matching sequence reported as wrong")
	}
	if !quotedPacketHasWrongSequence(quoted, seq+1) {
		t.Fatal("wrong sequence was not detected")
	}
	if quotedPacketHasWrongSequence(quoted[:7], seq) {
		t.Fatal("truncated quoted packet should not force wrong-sequence classification")
	}
}

func TestEchoReplyMatchesSequence(t *testing.T) {
	reply := []byte{
		0, 0, 0, 0, // type, code, checksum
		0, 0, 0x12, 0x34, // id, seq
	}
	if !echoReplyMatchesSequence(reply, 0x1234) {
		t.Fatal("expected echo reply sequence to match")
	}
	if echoReplyMatchesSequence(reply, 0x1235) {
		t.Fatal("expected different echo reply sequence not to match")
	}
}

func encodeSockExtendedErr(ee sockExtendedErr) []byte {
	data := make([]byte, sockExtendedErrLen)
	binary.NativeEndian.PutUint32(data[0:4], ee.errno)
	data[4] = ee.origin
	data[5] = ee.typ
	data[6] = ee.code
	binary.NativeEndian.PutUint32(data[8:12], ee.info)
	binary.NativeEndian.PutUint32(data[12:16], ee.data)
	return data
}

func quotedEcho(seq int) []byte {
	data := make([]byte, 8)
	data[0] = byte(ipv4.ICMPTypeEcho)
	data[1] = 0
	binary.BigEndian.PutUint16(data[6:8], uint16(seq))
	return data
}

func ipRecvErrOOB(t *testing.T, data []byte) []byte {
	t.Helper()
	return socketControlOOB(t, unix.IPPROTO_IP, unix.IP_RECVERR, data)
}

func socketControlOOB(t *testing.T, level int, typ int, data []byte) []byte {
	t.Helper()
	space := unix.CmsgSpace(len(data))
	oob := make([]byte, space)
	cmsgLen := unix.CmsgLen(len(data))
	binary.NativeEndian.PutUint64(oob[0:8], uint64(cmsgLen))
	binary.NativeEndian.PutUint32(oob[8:12], uint32(level))
	binary.NativeEndian.PutUint32(oob[12:16], uint32(typ))
	copy(oob[unix.CmsgLen(0):], data)
	return oob
}
