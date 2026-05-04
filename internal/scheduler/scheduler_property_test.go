package scheduler

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"netsonar/internal/config"
	"netsonar/internal/metrics"
	"netsonar/internal/probe"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
)

// **Validates: Requirements 5.1, 5.2**
// Property 3: diffTargets set correctness
// For all pairs of valid target lists (old, new), diffTargets SHALL produce
// three sets (toStop, toStart, unchanged) such that:
//   1. Disjoint: unchanged names don't overlap with toStop or toStart names
//   2. Coverage: union of all names = union of old and new names
//   3. Unchanged correctness: every unchanged target exists in both old and new
//      with identical configuration

// targetNamePool is the shared pool of target names used by both old and new
// lists, ensuring overlap between them.
var targetNamePool = []string{"t0", "t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8", "t9"}

// genSimpleTarget generates a TargetConfig with the given name and random
// configuration fields (Address, ProbeType, Interval, Timeout).
func genSimpleTarget(name string) gopter.Gen {
	return gopter.CombineGens(
		gen.IntRange(1, 255),  // address octet
		gen.IntRange(0, 1),    // probe type index (TCP or HTTP)
		gen.IntRange(10, 120), // interval seconds
	).Map(func(vals []interface{}) config.TargetConfig {
		octet := vals[0].(int)
		ptIdx := vals[1].(int)
		intervalSec := vals[2].(int)

		probeType := config.ProbeTypeTCP
		if ptIdx == 1 {
			probeType = config.ProbeTypeHTTP
		}

		interval := time.Duration(intervalSec) * time.Second
		timeout := time.Duration(intervalSec/2) * time.Second
		if timeout == 0 {
			timeout = 1 * time.Second
		}

		return config.TargetConfig{
			Name:      name,
			Address:   fmt.Sprintf("10.0.0.%d:443", octet),
			ProbeType: probeType,
			Interval:  interval,
			Timeout:   timeout,
		}
	})
}

// targetListPair holds a generated pair of old and new target lists.
type targetListPair struct {
	Old []config.TargetConfig
	New []config.TargetConfig
}

// genTargetListPair generates a pair of target lists (old, new) where:
//   - Each list has 0–8 targets with unique names within that list
//   - Names are drawn from the shared pool so there's overlap
//   - For shared names, targets are sometimes identical and sometimes different
func genTargetListPair() gopter.Gen {
	return gopter.CombineGens(
		// Pick how many names go to old-only, new-only, shared-identical, shared-changed
		gen.IntRange(0, 3), // old-only count
		gen.IntRange(0, 3), // new-only count
		gen.IntRange(0, 3), // shared-identical count
		gen.IntRange(0, 2), // shared-changed count
	).FlatMap(func(v interface{}) gopter.Gen {
		vals := v.([]interface{})
		oldOnlyCount := vals[0].(int)
		newOnlyCount := vals[1].(int)
		sharedIdenticalCount := vals[2].(int)
		sharedChangedCount := vals[3].(int)

		totalNeeded := oldOnlyCount + newOnlyCount + sharedIdenticalCount + sharedChangedCount
		if totalNeeded > len(targetNamePool) {
			// Clamp to pool size by reducing proportionally.
			totalNeeded = len(targetNamePool)
			sharedChangedCount = min(sharedChangedCount, totalNeeded)
			totalNeeded -= sharedChangedCount
			sharedIdenticalCount = min(sharedIdenticalCount, totalNeeded)
			totalNeeded -= sharedIdenticalCount
			newOnlyCount = min(newOnlyCount, totalNeeded)
			totalNeeded -= newOnlyCount
			oldOnlyCount = totalNeeded
		}

		// Generate a permutation of the name pool to assign names.
		return gen.SliceOfN(len(targetNamePool), gen.IntRange(0, 999999)).FlatMap(func(v interface{}) gopter.Gen {
			// Use the random ints to create a shuffled index.
			randVals := v.([]int)
			indices := make([]int, len(targetNamePool))
			for i := range indices {
				indices[i] = i
			}
			// Fisher-Yates-ish shuffle using generated random values.
			for i := len(indices) - 1; i > 0; i-- {
				j := randVals[i] % (i + 1)
				if j < 0 {
					j = -j
				}
				indices[i], indices[j] = indices[j], indices[i]
			}

			// Assign names from shuffled pool.
			nameIdx := 0
			oldOnlyNames := make([]string, oldOnlyCount)
			for i := 0; i < oldOnlyCount; i++ {
				oldOnlyNames[i] = targetNamePool[indices[nameIdx]]
				nameIdx++
			}
			newOnlyNames := make([]string, newOnlyCount)
			for i := 0; i < newOnlyCount; i++ {
				newOnlyNames[i] = targetNamePool[indices[nameIdx]]
				nameIdx++
			}
			sharedIdenticalNames := make([]string, sharedIdenticalCount)
			for i := 0; i < sharedIdenticalCount; i++ {
				sharedIdenticalNames[i] = targetNamePool[indices[nameIdx]]
				nameIdx++
			}
			sharedChangedNames := make([]string, sharedChangedCount)
			for i := 0; i < sharedChangedCount; i++ {
				sharedChangedNames[i] = targetNamePool[indices[nameIdx]]
				nameIdx++
			}

			// Build generators for each target.
			var gens []gopter.Gen

			// Old-only targets (one gen each).
			for _, name := range oldOnlyNames {
				gens = append(gens, genSimpleTarget(name))
			}
			// New-only targets (one gen each).
			for _, name := range newOnlyNames {
				gens = append(gens, genSimpleTarget(name))
			}
			// Shared-identical targets (one gen each, used in both lists).
			for _, name := range sharedIdenticalNames {
				gens = append(gens, genSimpleTarget(name))
			}
			// Shared-changed targets: two gens per name (old config, new config).
			for _, name := range sharedChangedNames {
				gens = append(gens, genSimpleTarget(name))
				gens = append(gens, genSimpleTarget(name))
			}

			if len(gens) == 0 {
				return gen.Const(targetListPair{
					Old: []config.TargetConfig{},
					New: []config.TargetConfig{},
				})
			}

			return gopter.CombineGens(gens...).Map(func(vals []interface{}) targetListPair {
				idx := 0
				var oldList, newList []config.TargetConfig

				// Old-only → old list only.
				for i := 0; i < oldOnlyCount; i++ {
					oldList = append(oldList, vals[idx].(config.TargetConfig))
					idx++
				}
				// New-only → new list only.
				for i := 0; i < newOnlyCount; i++ {
					newList = append(newList, vals[idx].(config.TargetConfig))
					idx++
				}
				// Shared-identical → same target in both lists.
				for i := 0; i < sharedIdenticalCount; i++ {
					t := vals[idx].(config.TargetConfig)
					oldList = append(oldList, t)
					newList = append(newList, t)
					idx++
				}
				// Shared-changed → old config in old list, new config in new list.
				// Ensure they are actually different by forcing a field change.
				for i := 0; i < sharedChangedCount; i++ {
					oldT := vals[idx].(config.TargetConfig)
					idx++
					newT := vals[idx].(config.TargetConfig)
					idx++
					// Force difference if they happen to be identical.
					if reflect.DeepEqual(oldT, newT) {
						if newT.ProbeType == config.ProbeTypeTCP {
							newT.ProbeType = config.ProbeTypeHTTP
						} else {
							newT.ProbeType = config.ProbeTypeTCP
						}
						newT.Address = newT.Address + "0"
					}
					oldList = append(oldList, oldT)
					newList = append(newList, newT)
				}

				return targetListPair{Old: oldList, New: newList}
			})
		}, reflect.TypeOf(targetListPair{}))
	}, reflect.TypeOf(targetListPair{}))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// names extracts the set of target names from a slice.
func names(targets []config.TargetConfig) map[string]bool {
	m := make(map[string]bool, len(targets))
	for _, t := range targets {
		m[t.Name] = true
	}
	return m
}

// targetByName builds a name→TargetConfig lookup map.
func targetByName(targets []config.TargetConfig) map[string]config.TargetConfig {
	m := make(map[string]config.TargetConfig, len(targets))
	for _, t := range targets {
		m[t.Name] = t
	}
	return m
}

// TestPropertyDiffTargetsSetCorrectness verifies Property 3: for all pairs of
// valid target lists, diffTargets produces correct disjoint sets whose union
// covers all input names, and unchanged targets have identical configuration.
// **Validates: Requirement 3.1**
// Property 4: scheduler goroutine count matches target count
// After Start(), the number of running probe entries tracked by the scheduler
// SHALL equal the number of targets in the configuration.
func TestPropertySchedulerGoroutineCountMatchesTargetCount(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow property-based test in short mode")
	}
	parameters := gopter.DefaultTestParameters()
	parameters.MinSuccessfulTests = 100
	properties := gopter.NewProperties(parameters)

	properties.Property("scheduler goroutine count matches target count after Start", prop.ForAll(
		func(targets []config.TargetConfig) bool {
			me := metrics.NewMetricsExporter([]string{"service", "scope", "provider", "target_region", "target_partition", "visibility", "port", "impact"}, metrics.ExporterOptions{})

			// noopProber blocks until context is cancelled, simulating a
			// long-running probe so goroutines stay alive for counting.
			noopProberFn := func(_ config.TargetConfig) probe.Prober {
				return &noopProber{}
			}

			s := New(me, noopProberFn)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			cfg := config.Config{
				Agent: config.AgentConfig{
					ListenAddr:      ":9275",
					DefaultInterval: 30 * time.Second,
				},
				Targets: targets,
			}

			s.Start(ctx, cfg)

			// Wait for goroutines to launch with a bounded poll instead of
			// a fixed sleep, which is fragile under CI load.
			expected := len(targets)
			deadline := time.Now().Add(2 * time.Second)
			for s.Targets() != expected && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}

			got := s.Targets()

			if got != expected {
				t.Logf("goroutine count mismatch: got %d, expected %d", got, expected)
			}

			s.Stop()

			return got == expected
		},
		genUniqueTargetList(),
	))

	properties.TestingRun(t)
}

// noopProber is a Prober that blocks until the context is cancelled,
// returning a successful result. Used to keep scheduler goroutines alive
// during counting.
type noopProber struct{}

func (p *noopProber) Probe(ctx context.Context, target config.TargetConfig) probe.ProbeResult {
	<-ctx.Done()
	return probe.ProbeResult{Success: true, Duration: time.Millisecond}
}

// genUniqueTargetList generates a slice of 0–10 TargetConfig values with
// unique names, valid intervals, and TCP probe type.
func genUniqueTargetList() gopter.Gen {
	return gen.IntRange(0, 10).FlatMap(func(v interface{}) gopter.Gen {
		count := v.(int)
		if count == 0 {
			return gen.Const([]config.TargetConfig{})
		}

		gens := make([]gopter.Gen, count)
		for i := 0; i < count; i++ {
			name := targetNamePool[i]
			gens[i] = genSimpleTarget(name)
		}

		return gopter.CombineGens(gens...).Map(func(vals []interface{}) []config.TargetConfig {
			result := make([]config.TargetConfig, len(vals))
			for i, v := range vals {
				result[i] = v.(config.TargetConfig)
			}
			return result
		})
	}, reflect.TypeOf([]config.TargetConfig{}))
}

// **Validates: Requirement 16.3**
// Property 5: scheduler stop leaves zero goroutines
// After Stop(), the scheduler SHALL have zero tracked probe entries and all
// goroutines SHALL have completed (wg.Wait returns immediately).
func TestPropertySchedulerStopLeavesZeroGoroutines(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow property-based test in short mode")
	}
	parameters := gopter.DefaultTestParameters()
	parameters.MinSuccessfulTests = 100
	properties := gopter.NewProperties(parameters)

	properties.Property("scheduler stop leaves zero goroutines", prop.ForAll(
		func(targets []config.TargetConfig) bool {
			me := metrics.NewMetricsExporter([]string{"service", "scope", "provider", "target_region", "target_partition", "visibility", "port", "impact"}, metrics.ExporterOptions{})

			// blockingProber blocks until context is cancelled so goroutines
			// stay alive until Stop() cancels them.
			blockingProberFn := func(_ config.TargetConfig) probe.Prober {
				return &noopProber{}
			}

			s := New(me, blockingProberFn)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			cfg := config.Config{
				Agent: config.AgentConfig{
					ListenAddr:      ":9275",
					DefaultInterval: 30 * time.Second,
				},
				Targets: targets,
			}

			s.Start(ctx, cfg)

			// Wait for goroutines to launch with a bounded poll.
			deadline := time.Now().Add(2 * time.Second)
			for s.Targets() != len(targets) && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}

			// Verify goroutines are running before stop.
			beforeStop := s.Targets()
			if beforeStop != len(targets) {
				t.Logf("pre-stop count mismatch: got %d, expected %d", beforeStop, len(targets))
				return false
			}

			// Stop the scheduler — this must cancel all goroutines and wait.
			s.Stop()

			// After Stop(), tracked probe count must be zero.
			afterStop := s.Targets()
			if afterStop != 0 {
				t.Logf("post-stop target count: got %d, expected 0", afterStop)
				return false
			}

			// Verify wg.Wait() returns immediately (all goroutines exited).
			// We call it with a tight deadline — if any goroutine leaked,
			// this would block.
			done := make(chan struct{})
			go func() {
				s.wg.Wait()
				close(done)
			}()

			select {
			case <-done:
				// All goroutines completed — success.
			case <-time.After(2 * time.Second):
				t.Log("wg.Wait() did not return within 2s — goroutine leak detected")
				return false
			}

			return true
		},
		genUniqueTargetList(),
	))

	properties.TestingRun(t)
}

func TestPropertyDiffTargetsSetCorrectness(t *testing.T) {
	parameters := gopter.DefaultTestParameters()
	parameters.MinSuccessfulTests = 200
	properties := gopter.NewProperties(parameters)

	properties.Property("diffTargets set correctness: disjoint, coverage, unchanged classification", prop.ForAll(
		func(pair targetListPair) bool {
			toStop, toStart, unchanged := diffTargets(pair.Old, pair.New)

			unchangedNames := names(unchanged)
			toStopNames := names(toStop)
			toStartNames := names(toStart)

			// 1. Disjoint: unchanged names must not overlap with toStop or toStart names.
			for name := range unchangedNames {
				if toStopNames[name] {
					t.Logf("Disjoint violation: %q in both unchanged and toStop", name)
					return false
				}
				if toStartNames[name] {
					t.Logf("Disjoint violation: %q in both unchanged and toStart", name)
					return false
				}
			}

			// 2. Coverage: union of all output names = union of old and new input names.
			outputUnion := make(map[string]bool)
			for name := range toStopNames {
				outputUnion[name] = true
			}
			for name := range toStartNames {
				outputUnion[name] = true
			}
			for name := range unchangedNames {
				outputUnion[name] = true
			}

			inputUnion := make(map[string]bool)
			for _, t := range pair.Old {
				inputUnion[t.Name] = true
			}
			for _, t := range pair.New {
				inputUnion[t.Name] = true
			}

			if !reflect.DeepEqual(outputUnion, inputUnion) {
				t.Logf("Coverage violation: output union %v != input union %v", outputUnion, inputUnion)
				return false
			}

			// 3. Unchanged correctness: every unchanged target must exist in both
			//    old and new with identical configuration.
			oldByName := targetByName(pair.Old)
			newByName := targetByName(pair.New)

			for _, u := range unchanged {
				oldT, inOld := oldByName[u.Name]
				newT, inNew := newByName[u.Name]
				if !inOld || !inNew {
					t.Logf("Unchanged %q not present in both old (present=%v) and new (present=%v)", u.Name, inOld, inNew)
					return false
				}
				if !reflect.DeepEqual(oldT, newT) {
					t.Logf("Unchanged %q has different configs: old=%+v new=%+v", u.Name, oldT, newT)
					return false
				}
			}

			return true
		},
		genTargetListPair(),
	))

	properties.TestingRun(t)
}
