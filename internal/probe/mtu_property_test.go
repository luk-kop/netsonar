package probe

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"netsonar/internal/config"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
)

// mtuScenario describes a generated MTU probe scenario used to exercise
// the MTUProber under varying conditions.
type mtuScenario struct {
	Address          string
	ICMPPayloadSizes []int
	TimeoutMs        int
	Description      string
}

// genICMPPayloadSizes generates valid descending-sorted MTU payload size slices.
// Sizes are realistic ICMP payload sizes in the range [100, 1472].
// The slice contains 1–6 elements, is sorted descending, and has no
// duplicates (as required by config validation).
func genICMPPayloadSizes() gopter.Gen {
	return gen.IntRange(1, 6).FlatMap(func(v interface{}) gopter.Gen {
		count := v.(int)
		return gen.SliceOfN(count, gen.IntRange(100, 1472)).Map(func(raw []int) []int {
			// Deduplicate.
			seen := make(map[int]bool, len(raw))
			unique := make([]int, 0, len(raw))
			for _, s := range raw {
				if !seen[s] {
					seen[s] = true
					unique = append(unique, s)
				}
			}
			// Sort descending.
			sort.Sort(sort.Reverse(sort.IntSlice(unique)))
			// Guarantee at least 1 element.
			if len(unique) == 0 {
				return []int{1472}
			}
			return unique
		})
	}, reflect.TypeOf([]int{}))
}

// genMTUAddress generates addresses that exercise different code paths:
// localhost (likely succeeds with CAP_NET_RAW), unresolvable hostnames,
// TEST-NET addresses (unlikely to respond), and 0.0.0.0.
func genMTUAddress() gopter.Gen {
	addresses := []string{
		"127.0.0.1",
		"this.host.does.not.exist.invalid",
		"192.0.2.1", // TEST-NET-1, unlikely to respond
		"0.0.0.0",
	}
	return gen.IntRange(0, len(addresses)-1).Map(func(i int) string {
		return addresses[i]
	})
}

// genMTUTimeoutMs generates a timeout in milliseconds (100ms–3000ms).
// Short timeouts exercise the timeout/context-cancellation paths.
func genMTUTimeoutMs() gopter.Gen {
	return gen.IntRange(100, 3000)
}

// genMTUScenario generates a random MTU probe scenario by combining
// address, MTU sizes, and timeout generators.
func genMTUScenario() gopter.Gen {
	return gopter.CombineGens(
		genMTUAddress(),
		genICMPPayloadSizes(),
		genMTUTimeoutMs(),
	).Map(func(vals []interface{}) mtuScenario {
		return mtuScenario{
			Address:          vals[0].(string),
			ICMPPayloadSizes: vals[1].([]int),
			TimeoutMs:        vals[2].(int),
			Description: fmt.Sprintf("addr=%s sizes=%v timeout=%dms",
				vals[0].(string), vals[1].([]int), vals[2].(int)),
		}
	})
}

// TestPropertyMTUPathMTUDomain verifies Property 11:
// For all MTU probe executions, the PathMTU field SHALL be either -1
// or equal to one of the configured icmp_payload_sizes plus 28 (IP + ICMP
// header overhead).
//
// The test exercises the prober with varying generated inputs (addresses,
// MTU size lists, timeouts) and verifies the PathMTU domain invariant
// holds in all cases. Additionally it checks related invariants:
//   - If Success == true, then PathMTU must be one of sizes[i]+28 (not -1)
//   - If Success == false, then PathMTU == -1
//   - If Success == true, then Error == ""
//
// **Validates: Requirements 9.2, 15.4**
func TestPropertyMTUPathMTUDomain(t *testing.T) {
	parameters := gopter.DefaultTestParameters()
	parameters.MinSuccessfulTests = 200
	parameters.MaxSize = 50
	properties := gopter.NewProperties(parameters)

	properties.Property("MTU PathMTU is always -1 or sizes[i]+28 with correct invariants", prop.ForAll(
		func(sc mtuScenario) (bool, error) {
			timeout := time.Duration(sc.TimeoutMs) * time.Millisecond

			target := config.TargetConfig{
				Name:      "pbt-mtu-pathmtu",
				Address:   sc.Address,
				ProbeType: config.ProbeTypeMTU,
				Timeout:   timeout,
				ProbeOpts: config.ProbeOptions{
					ICMPPayloadSizes: sc.ICMPPayloadSizes,
				},
			}

			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()

			prober := &MTUProber{}
			result := prober.Probe(ctx, target)

			// Build the set of valid PathMTU values: -1 or any configured size + 28.
			validMTUs := make(map[int]bool, len(sc.ICMPPayloadSizes)+1)
			validMTUs[-1] = true
			for _, s := range sc.ICMPPayloadSizes {
				validMTUs[s+28] = true
			}

			// --- Property 11a: PathMTU domain ---
			if !validMTUs[result.PathMTU] {
				return false, fmt.Errorf(
					"PathMTU=%d is not -1 and not in {sizes+28} for scenario: %s (valid: %v)",
					result.PathMTU, sc.Description, validMTUs)
			}

			// --- Property 11b: Success implies valid PathMTU (not -1) ---
			if result.Success && result.PathMTU == -1 {
				return false, fmt.Errorf(
					"Success=true but PathMTU=-1 for scenario: %s",
					sc.Description)
			}

			// --- Property 11c: Failure implies PathMTU == -1 ---
			if !result.Success && result.PathMTU != -1 {
				return false, fmt.Errorf(
					"Success=false but PathMTU=%d (expected -1) for scenario: %s",
					result.PathMTU, sc.Description)
			}

			// --- Property 11d: Success implies empty error ---
			if result.Success && result.Error != "" {
				return false, fmt.Errorf(
					"Success=true but Error=%q for scenario: %s",
					result.Error, sc.Description)
			}

			return true, nil
		},
		genMTUScenario(),
	))

	properties.TestingRun(t)
}
