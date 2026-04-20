package probe

import (
	"context"
	"errors"
	"testing"
	"time"

	"netsonar/internal/config"
)

// TestICMPProber_ResolutionFailure verifies that probing an unresolvable
// address reports Success=false, PacketLoss=1.0, and a descriptive error.
func TestICMPProber_ResolutionFailure(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-icmp-resolve-fail",
		Address:   "this.host.does.not.exist.invalid",
		ProbeType: config.ProbeTypeICMP,
		Timeout:   2 * time.Second,
		ProbeOpts: config.ProbeOptions{
			PingCount:       3,
			PingIntervalSec: 0.1,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &ICMPProber{}
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false for unresolvable address")
	}
	if result.Error == "" {
		t.Fatal("expected non-empty Error for unresolvable address")
	}
	if result.PacketLoss != 1.0 {
		t.Fatalf("expected PacketLoss=1.0 on resolution failure, got %f", result.PacketLoss)
	}
}

// TestICMPProber_SocketError verifies that when the unprivileged ICMP
// socket cannot be opened (e.g. net.ipv4.ping_group_range does not
// include the process effective or supplementary GID), the prober reports a clear error,
// PacketLoss=1.0, and Success=false.
//
// This test is skipped if the socket opens successfully.
func TestICMPProber_SocketError(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-icmp-socket-error",
		Address:   "127.0.0.1",
		ProbeType: config.ProbeTypeICMP,
		Timeout:   2 * time.Second,
		ProbeOpts: config.ProbeOptions{
			PingCount: 1,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &ICMPProber{}
	result := prober.Probe(ctx, target)

	// If the probe succeeded, the socket opened fine — skip.
	if result.Success {
		t.Skip("unprivileged ICMP socket works; skipping socket error test")
	}

	// If it failed for a non-socket reason (e.g. timeout, no reply), also skip.
	if result.Error == "" || result.Error == "all ICMP echo requests timed out or failed" {
		t.Skipf("failure is not socket-related: %s", result.Error)
	}

	if result.PacketLoss != 1.0 {
		t.Fatalf("expected PacketLoss=1.0 on socket error, got %f", result.PacketLoss)
	}
}

// TestICMPProber_DefaultPingCount verifies that when PingCount is zero,
// the prober defaults to 1 ping (does not panic or loop forever).
func TestICMPProber_DefaultPingCount(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-icmp-default-count",
		Address:   "127.0.0.1",
		ProbeType: config.ProbeTypeICMP,
		Timeout:   2 * time.Second,
		ProbeOpts: config.ProbeOptions{
			PingCount: 0, // should default to 1
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &ICMPProber{}
	result := prober.Probe(ctx, target)

	// We can't guarantee success (depends on ping_group_range), but the probe
	// must complete without hanging and PacketLoss must be valid.
	if result.PacketLoss < 0.0 || result.PacketLoss > 1.0 {
		t.Fatalf("expected PacketLoss in [0.0, 1.0], got %f", result.PacketLoss)
	}
}

// TestICMPProber_NegativePingCount verifies that a negative PingCount is
// treated the same as zero (defaults to 1).
func TestICMPProber_NegativePingCount(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-icmp-negative-count",
		Address:   "127.0.0.1",
		ProbeType: config.ProbeTypeICMP,
		Timeout:   2 * time.Second,
		ProbeOpts: config.ProbeOptions{
			PingCount: -5, // should default to 1
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &ICMPProber{}
	result := prober.Probe(ctx, target)

	// Must complete without panic; PacketLoss must be valid.
	if result.PacketLoss < 0.0 || result.PacketLoss > 1.0 {
		t.Fatalf("expected PacketLoss in [0.0, 1.0], got %f", result.PacketLoss)
	}
}

// TestICMPProber_ContextCancelled verifies that a pre-cancelled context
// causes the probe to return quickly with failure and valid invariants.
func TestICMPProber_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	target := config.TargetConfig{
		Name:      "test-icmp-ctx-cancel",
		Address:   "127.0.0.1",
		ProbeType: config.ProbeTypeICMP,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			PingCount:       5,
			PingIntervalSec: 1.0,
		},
	}

	start := time.Now()
	prober := &ICMPProber{}
	result := prober.Probe(ctx, target)
	elapsed := time.Since(start)

	// With a cancelled context and 5 pings at 1s interval, the probe must
	// return almost immediately — not wait for all 5 pings.
	if elapsed > 2*time.Second {
		t.Fatalf("probe took %v with cancelled context; expected fast return", elapsed)
	}

	// PacketLoss must be valid regardless of how the probe ended.
	if result.PacketLoss < 0.0 || result.PacketLoss > 1.0 {
		t.Fatalf("expected PacketLoss in [0.0, 1.0], got %f", result.PacketLoss)
	}
}

func TestICMPEchoSequencePacketLossUsesActualSentOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var sends int
	result := runICMPEchoSequence(ctx, 5, 0, func(ctx context.Context, seq int) (time.Duration, bool, error) {
		sends++
		switch seq {
		case 0:
			return 10 * time.Millisecond, true, nil
		case 1:
			cancel()
			return 0, true, errors.New("read icmp: timeout")
		default:
			t.Fatalf("sendEcho called after context cancellation for seq=%d", seq)
			return 0, false, nil
		}
	})

	if sends != 2 {
		t.Fatalf("sendEcho calls = %d, want 2", sends)
	}
	if !result.Success {
		t.Fatalf("Success = false, want true; error=%q", result.Error)
	}
	if result.PacketLoss != 0.5 {
		t.Fatalf("PacketLoss = %f, want 0.5", result.PacketLoss)
	}
	if result.ICMPAvgRTT != 10*time.Millisecond {
		t.Fatalf("ICMPAvgRTT = %v, want 10ms", result.ICMPAvgRTT)
	}
}

// TestICMPProber_ResultInvariant_SuccessImpliesEmptyError verifies that
// when Success=true, Error is always empty. This is tested via localhost
// if ping sockets are allowed.
func TestICMPProber_ResultInvariant_SuccessImpliesEmptyError(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-icmp-invariant-success",
		Address:   "127.0.0.1",
		ProbeType: config.ProbeTypeICMP,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			PingCount:       3,
			PingIntervalSec: 0.1,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &ICMPProber{}
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Skipf("probe did not succeed (check net.ipv4.ping_group_range): %s", result.Error)
	}

	if result.Error != "" {
		t.Fatalf("Success=true but Error is non-empty: %q", result.Error)
	}
}

// TestICMPProber_ResultInvariant_FailureImpliesNonEmptyError verifies that
// when Success=false, Error is always non-empty.
func TestICMPProber_ResultInvariant_FailureImpliesNonEmptyError(t *testing.T) {
	// Use an unresolvable address to guarantee failure.
	target := config.TargetConfig{
		Name:      "test-icmp-invariant-failure",
		Address:   "this.host.does.not.exist.invalid",
		ProbeType: config.ProbeTypeICMP,
		Timeout:   2 * time.Second,
		ProbeOpts: config.ProbeOptions{
			PingCount: 1,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &ICMPProber{}
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false for unresolvable address")
	}
	if result.Error == "" {
		t.Fatal("Success=false but Error is empty")
	}
}

// TestICMPProber_ResultInvariant_PacketLossRange verifies that PacketLoss
// is always in [0.0, 1.0] regardless of the probe outcome.
func TestICMPProber_ResultInvariant_PacketLossRange(t *testing.T) {
	cases := []struct {
		name    string
		address string
	}{
		{"resolvable", "127.0.0.1"},
		{"unresolvable", "this.host.does.not.exist.invalid"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := config.TargetConfig{
				Name:      "test-icmp-pktloss-" + tc.name,
				Address:   tc.address,
				ProbeType: config.ProbeTypeICMP,
				Timeout:   2 * time.Second,
				ProbeOpts: config.ProbeOptions{
					PingCount:       3,
					PingIntervalSec: 0.1,
				},
			}

			ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
			defer cancel()

			prober := &ICMPProber{}
			result := prober.Probe(ctx, target)

			if result.PacketLoss < 0.0 || result.PacketLoss > 1.0 {
				t.Fatalf("PacketLoss=%f is outside [0.0, 1.0]", result.PacketLoss)
			}
		})
	}
}

// TestICMPProber_ResultInvariant_SuccessImpliesPositiveDurations verifies
// that when Success=true, Duration is wall-clock positive and ICMPAvgRTT is positive.
func TestICMPProber_ResultInvariant_SuccessImpliesPositiveDurations(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-icmp-invariant-duration",
		Address:   "127.0.0.1",
		ProbeType: config.ProbeTypeICMP,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			PingCount:       3,
			PingIntervalSec: 0.1,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &ICMPProber{}
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Skipf("probe did not succeed (check net.ipv4.ping_group_range): %s", result.Error)
	}

	if result.Duration <= 0 {
		t.Fatalf("Success=true but Duration=%v (expected > 0)", result.Duration)
	}
	if result.ICMPAvgRTT <= 0 {
		t.Fatalf("Success=true but ICMPAvgRTT=%v (expected > 0)", result.ICMPAvgRTT)
	}
	if result.Duration < result.ICMPAvgRTT {
		t.Fatalf("Duration=%v should be >= ICMPAvgRTT=%v", result.Duration, result.ICMPAvgRTT)
	}
}

func TestICMPProber_FailureSetsWallClockDurationAndZeroAvgRTT(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-icmp-failure-duration",
		Address:   "this.host.does.not.exist.invalid",
		ProbeType: config.ProbeTypeICMP,
		Timeout:   2 * time.Second,
		ProbeOpts: config.ProbeOptions{
			PingCount: 1,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &ICMPProber{}
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false for unresolvable address")
	}
	if result.Duration <= 0 {
		t.Fatalf("Failure Duration=%v, want wall-clock duration > 0", result.Duration)
	}
	if result.ICMPAvgRTT != 0 {
		t.Fatalf("Failure ICMPAvgRTT=%v, want 0", result.ICMPAvgRTT)
	}
}

// TestICMPProber_ResultInvariant_SuccessImpliesPacketLossLessThanOne
// verifies that when Success=true, PacketLoss < 1.0 (at least one reply).
func TestICMPProber_ResultInvariant_SuccessImpliesPacketLossLessThanOne(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-icmp-invariant-pktloss-success",
		Address:   "127.0.0.1",
		ProbeType: config.ProbeTypeICMP,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			PingCount:       3,
			PingIntervalSec: 0.1,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &ICMPProber{}
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Skipf("probe did not succeed (check net.ipv4.ping_group_range): %s", result.Error)
	}

	if result.PacketLoss >= 1.0 {
		t.Fatalf("Success=true but PacketLoss=%f (expected < 1.0)", result.PacketLoss)
	}
}

// TestICMPProber_ResultInvariant_FailureImpliesFullPacketLoss verifies
// that when all pings fail (resolution error), PacketLoss=1.0.
func TestICMPProber_ResultInvariant_FailureImpliesFullPacketLoss(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-icmp-invariant-full-loss",
		Address:   "this.host.does.not.exist.invalid",
		ProbeType: config.ProbeTypeICMP,
		Timeout:   2 * time.Second,
		ProbeOpts: config.ProbeOptions{
			PingCount: 3,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &ICMPProber{}
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected failure for unresolvable address")
	}
	if result.PacketLoss != 1.0 {
		t.Fatalf("expected PacketLoss=1.0 on total failure, got %f", result.PacketLoss)
	}
}

// TestICMPProber_DefaultPingInterval verifies that when PingIntervalSec
// is zero, the prober defaults to 1 second and does not panic.
func TestICMPProber_DefaultPingInterval(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-icmp-default-interval",
		Address:   "127.0.0.1",
		ProbeType: config.ProbeTypeICMP,
		Timeout:   3 * time.Second,
		ProbeOpts: config.ProbeOptions{
			PingCount:       1,
			PingIntervalSec: 0, // should default to 1s
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &ICMPProber{}
	result := prober.Probe(ctx, target)

	// Must complete without panic; PacketLoss must be valid.
	if result.PacketLoss < 0.0 || result.PacketLoss > 1.0 {
		t.Fatalf("expected PacketLoss in [0.0, 1.0], got %f", result.PacketLoss)
	}
}
