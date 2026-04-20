// Package scheduler manages probe goroutine lifecycles.
package scheduler

import (
	"context"
	"log/slog"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"netsonar/internal/config"
	"netsonar/internal/metrics"
	"netsonar/internal/probe"
)

// probeEntry tracks a running probe goroutine for a single target.
type probeEntry struct {
	target config.TargetConfig
	cancel context.CancelFunc
	done   chan struct{} // closed when goroutine exits
}

type probeState struct {
	seen        bool
	lastSuccess bool
	lastError   string
}

// Scheduler manages the lifecycle of probe goroutines. Each target gets its
// own goroutine that fires at the configured interval. The scheduler supports
// diff-based reload: on config change only affected targets are restarted.
type Scheduler struct {
	mu       sync.Mutex
	probes   map[string]*probeEntry // key: target name
	wg       sync.WaitGroup
	metrics  *metrics.MetricsExporter
	proberFn func(config.TargetConfig) probe.Prober
}

// New creates a Scheduler that records probe results into the given
// MetricsExporter. The proberFn callback is called once per target to
// obtain the appropriate Prober implementation.
func New(m *metrics.MetricsExporter, proberFn func(config.TargetConfig) probe.Prober) *Scheduler {
	return &Scheduler{
		probes:   make(map[string]*probeEntry),
		metrics:  m,
		proberFn: proberFn,
	}
}

// Start launches one probe goroutine per target in cfg.Targets. Each
// goroutine executes an immediate first probe, then repeats at the
// target's configured interval.
//
// Preconditions:
//   - cfg has been validated by config.LoadConfig
//   - ctx is a valid, non-cancelled context
//   - Scheduler has not been started yet (or has been fully stopped)
//
// Postconditions:
//   - One goroutine is running per target
//   - All goroutines respect ctx cancellation
//   - s.probes contains an entry for each target
func (s *Scheduler) Start(ctx context.Context, cfg config.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, t := range cfg.Targets {
		s.startTarget(ctx, t, cfg.Agent.InitialProbeJitter)
	}

	slog.Info("scheduler started", "targets", len(cfg.Targets))
}

// Stop cancels all probe goroutines and waits for them to complete.
//
// Postconditions:
//   - All probe goroutines have returned
//   - s.probes is empty
//   - Zero orphaned goroutines remain
func (s *Scheduler) Stop() {
	s.mu.Lock()
	for name, entry := range s.probes {
		entry.cancel()
		delete(s.probes, name)
	}
	s.mu.Unlock()

	s.wg.Wait()

	slog.Info("scheduler stopped")
}

// Reload diffs the new configuration against the currently running probes
// and applies the minimal set of changes: stop removed targets, start new
// targets, restart changed targets, leave unchanged targets running.
//
// Preconditions:
//   - cfg has been validated by config.LoadConfig
//   - Scheduler is currently running (Start was called)
//
// Postconditions:
//   - Removed targets are stopped
//   - New targets are started
//   - Changed targets are restarted
//   - Unchanged targets continue without interruption
func (s *Scheduler) Reload(ctx context.Context, cfg config.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Build the old target list from currently running probes.
	oldTargets := make([]config.TargetConfig, 0, len(s.probes))
	for _, entry := range s.probes {
		oldTargets = append(oldTargets, entry.target)
	}

	toStop, toStart, unchanged := diffTargets(oldTargets, cfg.Targets)

	// Stop removed and changed targets before starting replacements. This,
	// together with executeProbe's post-probe parent-context check, prevents
	// stale results from recreating deleted or changed target metrics.
	//
	// Collect done channels so we can wait for the old goroutines to fully
	// exit before starting replacements, preventing overlapping probe traffic.
	stoppedDone := make([]chan struct{}, 0, len(toStop))
	for _, t := range toStop {
		if entry, ok := s.probes[t.Name]; ok {
			entry.cancel()
			stoppedDone = append(stoppedDone, entry.done)
			delete(s.probes, t.Name)
		}
		s.metrics.DeleteTarget(t)
	}

	// Wait for cancelled goroutines to exit before starting replacements.
	// This is scoped to only the stopped targets, so unrelated probes are
	// not blocked.
	for _, done := range stoppedDone {
		<-done
	}

	// Start new and changed targets.
	for _, t := range toStart {
		s.startTarget(ctx, t, cfg.Agent.InitialProbeJitter)
	}

	slog.Info("scheduler reloaded",
		"stopped", len(toStop),
		"started", len(toStart),
		"unchanged", len(unchanged),
		"total", len(s.probes),
	)
}

// Targets returns the number of currently running probe goroutines.
func (s *Scheduler) Targets() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.probes)
}

// startTarget launches a probe goroutine for a single target. Caller must
// hold s.mu.
func (s *Scheduler) startTarget(ctx context.Context, t config.TargetConfig, initialProbeJitter time.Duration) {
	probeCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.probes[t.Name] = &probeEntry{target: t, cancel: cancel, done: done}

	prober := s.proberFn(t)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(done)
		runProbeLoop(probeCtx, t, prober, s.metrics, initialProbeJitter)
	}()
}

// runProbeLoop is the per-target goroutine. It executes an immediate first
// probe, optionally delayed by initialProbeJitter, then repeats at the target's
// configured interval until the context is cancelled.
//
// Invariant: at most one probe is in-flight for this target at any time.
func runProbeLoop(ctx context.Context, target config.TargetConfig, prober probe.Prober, m *metrics.MetricsExporter, initialProbeJitter time.Duration) {
	var state probeState

	if !waitInitialProbeJitter(ctx, initialProbeJitter) {
		return
	}

	executeProbe(ctx, target, prober, m, &state)

	ticker := time.NewTicker(target.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			executeProbe(ctx, target, prober, m, &state)
			// Drain any stale tick that accumulated while the probe was
			// running. Without this, a probe that takes longer than the
			// interval would immediately fire again back-to-back.
			select {
			case <-ticker.C:
				m.IncrSkippedOverlap(target)
				slog.Debug("skipped stale tick",
					"target_name", target.Name,
					"probe_type", target.ProbeType,
				)
			default:
			}
		}
	}
}

// randomInitialProbeDelay returns a random delay in [0, max]. It is a package
// variable so tests can substitute a deterministic implementation.
var randomInitialProbeDelay = func(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	// int64(MaxInt64)+1 overflows to MinInt64, which would make Int64N panic;
	// handle the boundary explicitly.
	if max == time.Duration(math.MaxInt64) {
		return time.Duration(rand.Int64N(math.MaxInt64))
	}
	return time.Duration(rand.Int64N(int64(max) + 1))
}

// waitInitialProbeJitter sleeps for a random delay in [0, max] before the first
// probe so agents starting simultaneously don't fire in lockstep. Returns true
// if the wait completed normally, false if ctx was cancelled during or before.
func waitInitialProbeJitter(ctx context.Context, max time.Duration) bool {
	delay := randomInitialProbeDelay(max)
	if delay <= 0 {
		return ctx.Err() == nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// executeProbe runs a single probe with a per-probe timeout derived from
// the target configuration and records the result in the metrics exporter.
//
// Precondition:  target is validated, prober matches target.ProbeType
// Postcondition: metrics are updated with probe result
// Invariant:     probe timeout is always enforced via context
func executeProbe(ctx context.Context, target config.TargetConfig, prober probe.Prober, m *metrics.MetricsExporter, state *probeState) {
	// If the parent context is already cancelled, skip the probe.
	if ctx.Err() != nil {
		return
	}

	probeCtx, cancel := context.WithTimeout(ctx, target.Timeout)
	defer cancel()

	result := prober.Probe(probeCtx, target)
	// Check parent context after Probe returns: if the target was removed or
	// changed during a slow probe (Reload/Stop called cancel on ctx), discard
	// the result so we don't recreate Prometheus series after DeleteTarget.
	// Do NOT check probeCtx.Err() here — a probe timeout is a valid result
	// that must be recorded.
	if ctx.Err() != nil {
		return
	}
	m.Record(target, result)
	logProbeResult(target, result, state)
}

func logProbeResult(target config.TargetConfig, result probe.ProbeResult, state *probeState) {
	if state == nil {
		return
	}

	if result.Success {
		if state.seen && !state.lastSuccess {
			slog.Info("probe recovered",
				"target_name", target.Name,
				"target", target.Address,
				"probe_type", target.ProbeType,
				"duration", result.Duration,
			)
		}
		state.seen = true
		state.lastSuccess = true
		state.lastError = ""
		return
	}

	if !state.seen || state.lastSuccess || state.lastError != result.Error {
		slog.Warn("probe failed",
			"target_name", target.Name,
			"target", target.Address,
			"probe_type", target.ProbeType,
			"duration", result.Duration,
			"error", result.Error,
		)
	} else {
		slog.Debug("probe still failing",
			"target_name", target.Name,
			"target", target.Address,
			"probe_type", target.ProbeType,
			"duration", result.Duration,
			"error", result.Error,
		)
	}

	state.seen = true
	state.lastSuccess = false
	state.lastError = result.Error
}

// diffTargets partitions old and new target lists into three disjoint sets:
//   - toStop:     targets present in old but absent in new, or present in both
//     but with changed configuration
//   - toStart:    targets present in new but absent in old, or present in both
//     but with changed configuration
//   - unchanged:  targets present in both with identical configuration
//
// Precondition:  oldTargets and newTargets are both validated
// Postcondition: union(toStop, toStart, unchanged names) = union(old, new names)
//
//	toStop, toStart, unchanged are pairwise disjoint by name
//	(changed targets appear in both toStop and toStart with their
//	respective old/new configs)
func diffTargets(oldTargets, newTargets []config.TargetConfig) (toStop, toStart, unchanged []config.TargetConfig) {
	oldMap := make(map[string]config.TargetConfig, len(oldTargets))
	for _, t := range oldTargets {
		oldMap[t.Name] = t
	}

	newMap := make(map[string]config.TargetConfig, len(newTargets))
	for _, t := range newTargets {
		newMap[t.Name] = t
	}

	// Targets in old but not in new → stop.
	for name, t := range oldMap {
		if _, exists := newMap[name]; !exists {
			toStop = append(toStop, t)
		}
	}

	// Targets in new but not in old → start.
	// Targets in both but changed → stop old + start new.
	for name, newT := range newMap {
		oldT, exists := oldMap[name]
		if !exists {
			toStart = append(toStart, newT)
		} else if !oldT.Equal(newT) {
			toStop = append(toStop, oldT)
			toStart = append(toStart, newT)
		} else {
			unchanged = append(unchanged, newT)
		}
	}

	return toStop, toStart, unchanged
}
