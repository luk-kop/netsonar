// Package probe — ICMPProber implementation.
package probe

import (
	"context"
	"fmt"
	"net"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"

	"netsonar/internal/config"
)

// ICMPProber probes ICMP reachability by sending echo requests and measuring
// round-trip time, packet loss, and hop count from the TTL field.
type ICMPProber struct{}

// Probe executes an ICMP ping sequence against target.Address.
//
// Preconditions:
//   - target.Address is a valid hostname or IP address (no port)
//   - The kernel allows unprivileged ICMP (net.ipv4.ping_group_range includes process GID)
//   - target.ProbeOpts.PingCount >= 1 (defaults to 1 if zero)
//
// Postconditions:
//   - result.Duration is the wall-clock duration of the full probe
//   - result.ICMPAvgRTT is the average RTT across all successful pings
//   - result.PacketLoss = (sent - received) / sent, range [0.0, 1.0]
//   - result.HopCount is the TTL from the last received ICMP echo reply
//   - result.Success is true if at least one echo reply was received
//   - The ICMP socket is always closed before returning
//   - result.Error is non-empty when Success is false
func (p *ICMPProber) Probe(ctx context.Context, target config.TargetConfig) (result ProbeResult) {
	start := time.Now()
	defer func() { result.Duration = time.Since(start) }()

	pingCount := target.ProbeOpts.PingCount
	if pingCount <= 0 {
		pingCount = 1
	}

	pingInterval := time.Duration(target.ProbeOpts.PingIntervalSec * float64(time.Second))
	if pingInterval <= 0 {
		pingInterval = time.Second
	}

	// Resolve the target address to an IPv4 address.
	dst, err := net.ResolveIPAddr("ip4", target.Address)
	if err != nil {
		result.Error = fmt.Sprintf("resolve IPv4 address: %s", err)
		result.PacketLoss = 1.0
		return result
	}

	// Open an unprivileged ICMP connection (SOCK_DGRAM). This does not
	// require CAP_NET_RAW — the kernel handles ICMP ID assignment and
	// filters responses per socket. Requires net.ipv4.ping_group_range
	// to include the process GID (default on most Linux distributions).
	conn, err := icmp.ListenPacket("udp4", "0.0.0.0")
	if err != nil {
		result.Error = fmt.Sprintf("listen icmp: %s (check net.ipv4.ping_group_range)", err)
		result.PacketLoss = 1.0
		return result
	}
	defer func() { _ = conn.Close() }()

	// Enable TTL reception so we can read hop count from control messages.
	if err := conn.IPv4PacketConn().SetControlMessage(ipv4.FlagTTL, true); err != nil {
		// Non-fatal: TTL extraction will return 0 but probing still works.
		_ = err
	}

	var (
		totalRTT time.Duration
		received int
		hopCount int
	)

	// With udp4 the kernel assigns ICMP IDs per socket. We set icmpID=0
	// in the outgoing Echo struct; the kernel overwrites it. Matching is
	// done by Seq + peer — the kernel already filters replies per socket.
	icmpID := 0

	for seq := 0; seq < pingCount; seq++ {
		// Check context before each ping.
		if ctx.Err() != nil {
			break
		}

		// Wait for the configured interval between pings (skip before first).
		if seq > 0 {
			select {
			case <-ctx.Done():
				break
			case <-time.After(pingInterval):
			}
			if ctx.Err() != nil {
				break
			}
		}

		rtt, ttl, err := p.sendEcho(ctx, conn, dst, icmpID, seq)
		if err != nil {
			continue
		}

		received++
		totalRTT += rtt
		hopCount = ttl
	}

	sent := pingCount
	result.PacketLoss = float64(sent-received) / float64(sent)

	if received == 0 {
		result.Error = "all ICMP echo requests timed out or failed"
		return result
	}

	result.Success = true
	result.ICMPAvgRTT = totalRTT / time.Duration(received)
	result.HopCount = hopCount

	return result
}

// sendEcho sends a single ICMP echo request and waits for the reply.
// It returns the round-trip time, the TTL from the reply, or an error.
//
// The per-ping deadline is derived from the context. If the context has a
// deadline, that is used; otherwise a 5-second fallback is applied.
//
// With udp4 sockets the kernel manages ICMP IDs and filters replies per
// socket, so crosstalk with other ICMP traffic on the host is eliminated.
func (p *ICMPProber) sendEcho(
	ctx context.Context,
	conn *icmp.PacketConn,
	dst *net.IPAddr,
	id, seq int,
) (rtt time.Duration, ttl int, err error) {
	// Build the ICMP echo request message.
	msg := &icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{
			ID:   id,
			Seq:  seq,
			Data: make([]byte, 56), // standard 64-byte ping (8 header + 56 payload)
		},
	}

	msgBytes, err := msg.Marshal(nil)
	if err != nil {
		return 0, 0, fmt.Errorf("marshal icmp: %w", err)
	}

	// Derive a per-ping deadline from the context.
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return 0, 0, fmt.Errorf("set deadline: %w", err)
	}

	// udp4 sockets require *net.UDPAddr as the destination.
	udpDst := &net.UDPAddr{IP: dst.IP}

	start := time.Now()
	if _, err := conn.WriteTo(msgBytes, udpDst); err != nil {
		return 0, 0, fmt.Errorf("write icmp: %w", err)
	}

	// Read replies until we find our echo reply or the deadline expires.
	// With udp4 the kernel filters replies per socket, so we only receive
	// responses to our own echo requests. We still verify Seq and peer
	// as a sanity check.
	readBuf := make([]byte, 1500)
	for {
		if ctx.Err() != nil {
			return 0, 0, ctx.Err()
		}

		n, cm, peer, err := conn.IPv4PacketConn().ReadFrom(readBuf)
		if err != nil {
			return 0, 0, fmt.Errorf("read icmp: %w", err)
		}
		elapsed := time.Since(start)

		// Parse the ICMP message (protocol 1 = ICMPv4).
		reply, err := icmp.ParseMessage(1, readBuf[:n])
		if err != nil {
			continue
		}

		// Only accept echo replies matching our sequence number.
		if reply.Type != ipv4.ICMPTypeEchoReply {
			continue
		}
		echo, ok := reply.Body.(*icmp.Echo)
		if !ok {
			continue
		}
		if echo.Seq != seq {
			continue
		}

		// Verify the peer matches our destination. With udp4 sockets,
		// ReadFrom returns a *net.UDPAddr ("ip:port") while dst is a
		// *net.IPAddr ("ip"). Compare only the IP portion so the check
		// works regardless of the net.Addr concrete type.
		var peerIP net.IP
		switch a := peer.(type) {
		case *net.UDPAddr:
			peerIP = a.IP
		case *net.IPAddr:
			peerIP = a.IP
		}
		if peerIP == nil || !peerIP.Equal(dst.IP) {
			continue
		}

		// Extract TTL from the control message if available.
		replyTTL := 0
		if cm != nil {
			replyTTL = cm.TTL
		}

		return elapsed, replyTTL, nil
	}
}
