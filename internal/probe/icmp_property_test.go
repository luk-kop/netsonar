package probe

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"netsonar/internal/config"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
)

// icmpScenario describes a generated ICMP probe scenario used to exercise
// the ICMPProber under varying conditions.
type icmpScenario struct {
	Address         string
	PingCount       int
	PingIntervalSec float64
	TimeoutMs       int
	Description     string
}

// genAddress generates an address that will exercise different code paths:
// unresolvable hostnames, localhost, and random invalid domains.
func genAddress() gopter.Gen {
	addresses := []string{
		"127.0.0.1",
		"localhost",
		"this.host.does.not.exist.invalid",
		"nxdomain.invalid",
		"unresolvable.test.invalid",
		"0.0.0.0",
		"192.0.2.1", // TEST-NET-1, unlikely to respond
	}
	return gen.IntRange(0, len(addresses)-1).Map(func(i int) string {
		return addresses[i]
	})
}

// genPingCount generates ping counts including edge cases: negative, zero,
// and small positive values. The prober defaults ≤0 to 1.
func genPingCount() gopter.Gen {
	return gen.IntRange(-3, 10)
}

// genPingIntervalSec generates ping intervals including edge cases: negative,
// zero, and small positive values. The prober defaults ≤0 to 1s.
func genPingIntervalSec() gopter.Gen {
	return gen.Float64Range(-1.0, 2.0)
}

// genTimeoutMs generates a timeout in milliseconds (50ms–2000ms).
// Short timeouts exercise the timeout/context-cancellation paths.
func genTimeoutMs() gopter.Gen {
	return gen.IntRange(50, 2000)
}

// genICMPScenario generates a random ICMP probe scenario by combining
// address, ping count, ping interval, and timeout generators.
func genICMPScenario() gopter.Gen {
	return gopter.CombineGens(
		genAddress(),
		genPingCount(),
		genPingIntervalSec(),
		genTimeoutMs(),
	).Map(func(vals []interface{}) icmpScenario {
		return icmpScenario{
			Address:         vals[0].(string),
			PingCount:       vals[1].(int),
			PingIntervalSec: vals[2].(float64),
			TimeoutMs:       vals[3].(int),
			Description: fmt.Sprintf("addr=%s count=%d interval=%.2f timeout=%dms",
				vals[0].(string), vals[1].(int), vals[2].(float64), vals[3].(int)),
		}
	})
}

// TestPropertyICMPPacketLossRange verifies Property 10:
// For all ICMP probe executions, the PacketLoss field SHALL be a value
// between 0.0 and 1.0 inclusive, regardless of whether the probe succeeds
// or fails.
//
// The test exercises the prober with varying generated inputs (ping counts,
// addresses, intervals, timeouts) and verifies the PacketLoss invariant
// holds in all cases. Additionally it checks related invariants:
//   - If Success == true, then PacketLoss < 1.0 (at least one reply received)
//   - If Success == false and error is resolution/permission related,
//     then PacketLoss == 1.0
//
// **Validates: Requirements 8.2, 15.3**
func TestPropertyICMPPacketLossRange(t *testing.T) {
	parameters := gopter.DefaultTestParameters()
	parameters.MinSuccessfulTests = 200
	parameters.MaxSize = 50
	properties := gopter.NewProperties(parameters)

	properties.Property("ICMP PacketLoss is always in [0.0, 1.0] with correct invariants", prop.ForAll(
		func(sc icmpScenario) (bool, error) {
			timeout := time.Duration(sc.TimeoutMs) * time.Millisecond

			target := config.TargetConfig{
				Name:      "pbt-icmp-pktloss",
				Address:   sc.Address,
				ProbeType: config.ProbeTypeICMP,
				Timeout:   timeout,
				ProbeOpts: config.ProbeOptions{
					PingCount:       sc.PingCount,
					PingIntervalSec: sc.PingIntervalSec,
				},
			}

			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()

			prober := &ICMPProber{}
			result := prober.Probe(ctx, target)

			// --- Property 10a: PacketLoss in [0.0, 1.0] ---
			if result.PacketLoss < 0.0 {
				return false, fmt.Errorf("PacketLoss=%f < 0.0 for scenario: %s",
					result.PacketLoss, sc.Description)
			}
			if result.PacketLoss > 1.0 {
				return false, fmt.Errorf("PacketLoss=%f > 1.0 for scenario: %s",
					result.PacketLoss, sc.Description)
			}

			// --- Property 10b: Success implies PacketLoss < 1.0 ---
			if result.Success && result.PacketLoss >= 1.0 {
				return false, fmt.Errorf(
					"Success=true but PacketLoss=%f (expected < 1.0) for scenario: %s",
					result.PacketLoss, sc.Description)
			}

			// --- Property 10c: Resolution/permission failure implies PacketLoss == 1.0 ---
			if !result.Success && isResolutionOrPermissionError(result.Error) {
				if result.PacketLoss != 1.0 {
					return false, fmt.Errorf(
						"resolution/permission error but PacketLoss=%f (expected 1.0) for scenario: %s, error=%q",
						result.PacketLoss, sc.Description, result.Error)
				}
			}

			return true, nil
		},
		genICMPScenario(),
	))

	properties.TestingRun(t)
}

// isResolutionOrPermissionError returns true if the error string indicates
// a DNS resolution failure or a socket error (e.g. ping_group_range).
func isResolutionOrPermissionError(errMsg string) bool {
	return strings.Contains(errMsg, "resolve address") ||
		strings.Contains(errMsg, "permission denied") ||
		strings.Contains(errMsg, "listen icmp") ||
		strings.Contains(errMsg, "ping_group_range")
}
