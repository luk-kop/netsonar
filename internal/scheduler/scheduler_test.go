package scheduler

import (
	"bytes"
	"context"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"netsonar/internal/config"
	"netsonar/internal/metrics"
	"netsonar/internal/probe"
)

// helper to build a minimal valid TargetConfig for diffTargets tests.
func makeTarget(name, address string, probeType config.ProbeType, interval time.Duration) config.TargetConfig {
	return config.TargetConfig{
		Name:      name,
		Address:   address,
		ProbeType: probeType,
		Interval:  interval,
		Timeout:   interval / 2,
	}
}

// targetNames extracts sorted-irrelevant name list from a target slice.
func targetNames(targets []config.TargetConfig) map[string]bool {
	m := make(map[string]bool, len(targets))
	for _, t := range targets {
		m[t.Name] = true
	}
	return m
}

func captureLogs(t *testing.T, fn func()) string {
	t.Helper()

	var buf bytes.Buffer
	previous := slog.Default()
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(previous) })

	fn()

	return buf.String()
}

func logTestTarget() config.TargetConfig {
	return config.TargetConfig{
		Name:      "target-a",
		Address:   "example.com:443",
		ProbeType: config.ProbeTypeTCP,
	}
}

func TestLogProbeResult_FirstFailureWarnsWithContext(t *testing.T) {
	target := logTestTarget()
	state := &probeState{}

	logs := captureLogs(t, func() {
		logProbeResult(target, probe.ProbeResult{
			Success:  false,
			Duration: 25 * time.Millisecond,
			Error:    "tcp dial: connection refused",
		}, state)
	})

	for _, want := range []string{
		"level=WARN",
		"msg=\"probe failed\"",
		"target_name=target-a",
		"target=example.com:443",
		"probe_type=tcp",
		"duration=25ms",
		"error=\"tcp dial: connection refused\"",
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("logs = %q, want to contain %q", logs, want)
		}
	}

	if !state.seen || state.lastSuccess || state.lastError != "tcp dial: connection refused" {
		t.Fatalf("state after first failure = %+v", state)
	}
}

func TestLogProbeResult_RepeatedSameFailureDebugs(t *testing.T) {
	target := logTestTarget()
	state := &probeState{
		seen:        true,
		lastSuccess: false,
		lastError:   "timeout",
	}

	logs := captureLogs(t, func() {
		logProbeResult(target, probe.ProbeResult{
			Success:  false,
			Duration: 2 * time.Second,
			Error:    "timeout",
		}, state)
	})

	if !strings.Contains(logs, "level=DEBUG") || !strings.Contains(logs, "msg=\"probe still failing\"") {
		t.Fatalf("logs = %q, want repeated failure debug log", logs)
	}
	if strings.Contains(logs, "level=WARN") {
		t.Fatalf("logs = %q, did not expect warn for repeated same failure", logs)
	}
}

func TestLogProbeResult_ChangedFailureWarns(t *testing.T) {
	target := logTestTarget()
	state := &probeState{
		seen:        true,
		lastSuccess: false,
		lastError:   "dns resolve: no such host",
	}

	logs := captureLogs(t, func() {
		logProbeResult(target, probe.ProbeResult{
			Success:  false,
			Duration: 80 * time.Millisecond,
			Error:    "tls handshake: certificate expired",
		}, state)
	})

	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "msg=\"probe failed\"") {
		t.Fatalf("logs = %q, want changed failure warn log", logs)
	}
	if state.lastError != "tls handshake: certificate expired" {
		t.Fatalf("lastError = %q, want changed error", state.lastError)
	}
}

func TestLogProbeResult_RecoveryInfos(t *testing.T) {
	target := logTestTarget()
	state := &probeState{
		seen:        true,
		lastSuccess: false,
		lastError:   "timeout",
	}

	logs := captureLogs(t, func() {
		logProbeResult(target, probe.ProbeResult{
			Success:  true,
			Duration: 10 * time.Millisecond,
		}, state)
	})

	if !strings.Contains(logs, "level=INFO") || !strings.Contains(logs, "msg=\"probe recovered\"") {
		t.Fatalf("logs = %q, want recovery info log", logs)
	}
	if !state.seen || !state.lastSuccess || state.lastError != "" {
		t.Fatalf("state after recovery = %+v", state)
	}
}

func TestLogProbeResult_SuccessAfterSuccessDoesNotLog(t *testing.T) {
	target := logTestTarget()
	state := &probeState{
		seen:        true,
		lastSuccess: true,
	}

	logs := captureLogs(t, func() {
		logProbeResult(target, probe.ProbeResult{
			Success:  true,
			Duration: 10 * time.Millisecond,
		}, state)
	})

	if logs != "" {
		t.Fatalf("logs = %q, want no log for success after success", logs)
	}
}

func TestLogProbeResult_FirstSuccessDoesNotLog(t *testing.T) {
	target := logTestTarget()
	state := &probeState{}

	logs := captureLogs(t, func() {
		logProbeResult(target, probe.ProbeResult{
			Success:  true,
			Duration: 10 * time.Millisecond,
		}, state)
	})

	if logs != "" {
		t.Fatalf("logs = %q, want no log for first success", logs)
	}
	if !state.seen || !state.lastSuccess || state.lastError != "" {
		t.Fatalf("state after first success = %+v", state)
	}
}

func TestLogProbeResult_NilStateDoesNotPanicOrLog(t *testing.T) {
	target := logTestTarget()

	logs := captureLogs(t, func() {
		logProbeResult(target, probe.ProbeResult{
			Success: false,
			Error:   "timeout",
		}, nil)
	})

	if logs != "" {
		t.Fatalf("logs = %q, want no log for nil state", logs)
	}
}

func TestWaitInitialProbeJitterZeroDoesNotWait(t *testing.T) {
	start := time.Now()
	if !waitInitialProbeJitter(context.Background(), 0) {
		t.Fatal("expected zero initial jitter wait to succeed")
	}
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
		t.Fatalf("zero initial jitter waited too long: %s", elapsed)
	}
}

func TestWaitInitialProbeJitterUsesRandomDelay(t *testing.T) {
	previous := randomInitialProbeDelay
	randomInitialProbeDelay = func(time.Duration) time.Duration {
		return 10 * time.Millisecond
	}
	t.Cleanup(func() { randomInitialProbeDelay = previous })

	start := time.Now()
	if !waitInitialProbeJitter(context.Background(), time.Second) {
		t.Fatal("expected initial jitter wait to succeed")
	}
	if elapsed := time.Since(start); elapsed < 10*time.Millisecond {
		t.Fatalf("initial jitter returned before configured delay: %s", elapsed)
	}
}

func TestWaitInitialProbeJitterRespectsCancellation(t *testing.T) {
	previous := randomInitialProbeDelay
	randomInitialProbeDelay = func(time.Duration) time.Duration {
		return time.Hour
	}
	t.Cleanup(func() { randomInitialProbeDelay = previous })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if waitInitialProbeJitter(ctx, time.Second) {
		t.Fatal("expected cancelled context to abort initial jitter wait")
	}
}

func TestRandomInitialProbeDelayMaxInt64DoesNotPanic(t *testing.T) {
	delay := randomInitialProbeDelay(time.Duration(math.MaxInt64))
	if delay < 0 {
		t.Fatalf("delay = %s, want non-negative", delay)
	}
}

// TestDiffTargets_AddedTargets verifies that targets present only in the
// new list appear in toStart and nowhere else.
func TestDiffTargets_AddedTargets(t *testing.T) {
	oldTargets := []config.TargetConfig{
		makeTarget("existing", "10.0.0.1:443", config.ProbeTypeTCP, 30*time.Second),
	}
	newTargets := []config.TargetConfig{
		makeTarget("existing", "10.0.0.1:443", config.ProbeTypeTCP, 30*time.Second),
		makeTarget("brand-new", "10.0.0.2:443", config.ProbeTypeTCP, 30*time.Second),
		makeTarget("also-new", "10.0.0.3:80", config.ProbeTypeHTTP, 60*time.Second),
	}

	toStop, toStart, unchanged := diffTargets(oldTargets, newTargets)

	if len(toStop) != 0 {
		t.Errorf("expected 0 toStop, got %d: %v", len(toStop), targetNames(toStop))
	}
	startNames := targetNames(toStart)
	if len(toStart) != 2 {
		t.Fatalf("expected 2 toStart, got %d: %v", len(toStart), startNames)
	}
	if !startNames["brand-new"] || !startNames["also-new"] {
		t.Errorf("expected toStart to contain brand-new and also-new, got %v", startNames)
	}
	if len(unchanged) != 1 || unchanged[0].Name != "existing" {
		t.Errorf("expected 1 unchanged (existing), got %d: %v", len(unchanged), targetNames(unchanged))
	}
}

// TestDiffTargets_RemovedTargets verifies that targets present only in the
// old list appear in toStop and nowhere else.
func TestDiffTargets_RemovedTargets(t *testing.T) {
	oldTargets := []config.TargetConfig{
		makeTarget("keep-me", "10.0.0.1:443", config.ProbeTypeTCP, 30*time.Second),
		makeTarget("remove-me", "10.0.0.2:443", config.ProbeTypeTCP, 30*time.Second),
		makeTarget("also-remove", "10.0.0.3:80", config.ProbeTypeHTTP, 60*time.Second),
	}
	newTargets := []config.TargetConfig{
		makeTarget("keep-me", "10.0.0.1:443", config.ProbeTypeTCP, 30*time.Second),
	}

	toStop, toStart, unchanged := diffTargets(oldTargets, newTargets)

	if len(toStart) != 0 {
		t.Errorf("expected 0 toStart, got %d: %v", len(toStart), targetNames(toStart))
	}
	stopNames := targetNames(toStop)
	if len(toStop) != 2 {
		t.Fatalf("expected 2 toStop, got %d: %v", len(toStop), stopNames)
	}
	if !stopNames["remove-me"] || !stopNames["also-remove"] {
		t.Errorf("expected toStop to contain remove-me and also-remove, got %v", stopNames)
	}
	if len(unchanged) != 1 || unchanged[0].Name != "keep-me" {
		t.Errorf("expected 1 unchanged (keep-me), got %d: %v", len(unchanged), targetNames(unchanged))
	}
}

// TestDiffTargets_ChangedTargets verifies that targets present in both lists
// but with different configuration appear in both toStop (old config) and
// toStart (new config).
func TestDiffTargets_ChangedTargets(t *testing.T) {
	oldTargets := []config.TargetConfig{
		makeTarget("changed-interval", "10.0.0.1:443", config.ProbeTypeTCP, 30*time.Second),
		makeTarget("changed-address", "10.0.0.2:443", config.ProbeTypeTCP, 30*time.Second),
		makeTarget("changed-type", "10.0.0.3:443", config.ProbeTypeTCP, 30*time.Second),
		makeTarget("stable", "10.0.0.4:443", config.ProbeTypeTCP, 30*time.Second),
	}
	newTargets := []config.TargetConfig{
		{
			Name: "changed-interval", Address: "10.0.0.1:443",
			ProbeType: config.ProbeTypeTCP, Interval: 60 * time.Second, Timeout: 15 * time.Second,
		},
		{
			Name: "changed-address", Address: "10.0.0.99:443",
			ProbeType: config.ProbeTypeTCP, Interval: 30 * time.Second, Timeout: 15 * time.Second,
		},
		{
			Name: "changed-type", Address: "10.0.0.3:443",
			ProbeType: config.ProbeTypeHTTP, Interval: 30 * time.Second, Timeout: 15 * time.Second,
		},
		makeTarget("stable", "10.0.0.4:443", config.ProbeTypeTCP, 30*time.Second),
	}

	toStop, toStart, unchanged := diffTargets(oldTargets, newTargets)

	stopNames := targetNames(toStop)
	startNames := targetNames(toStart)

	// All three changed targets should appear in both toStop and toStart.
	for _, name := range []string{"changed-interval", "changed-address", "changed-type"} {
		if !stopNames[name] {
			t.Errorf("expected %q in toStop", name)
		}
		if !startNames[name] {
			t.Errorf("expected %q in toStart", name)
		}
	}

	// toStop should contain old configs, toStart should contain new configs.
	for _, s := range toStop {
		if s.Name == "changed-interval" && s.Interval != 30*time.Second {
			t.Errorf("toStop changed-interval should have old interval 30s, got %v", s.Interval)
		}
		if s.Name == "changed-address" && s.Address != "10.0.0.2:443" {
			t.Errorf("toStop changed-address should have old address, got %s", s.Address)
		}
		if s.Name == "changed-type" && s.ProbeType != config.ProbeTypeTCP {
			t.Errorf("toStop changed-type should have old probe type tcp, got %s", s.ProbeType)
		}
	}
	for _, s := range toStart {
		if s.Name == "changed-interval" && s.Interval != 60*time.Second {
			t.Errorf("toStart changed-interval should have new interval 60s, got %v", s.Interval)
		}
		if s.Name == "changed-address" && s.Address != "10.0.0.99:443" {
			t.Errorf("toStart changed-address should have new address, got %s", s.Address)
		}
		if s.Name == "changed-type" && s.ProbeType != config.ProbeTypeHTTP {
			t.Errorf("toStart changed-type should have new probe type http, got %s", s.ProbeType)
		}
	}

	// Stable target should be unchanged only.
	if stopNames["stable"] {
		t.Error("stable should not be in toStop")
	}
	if startNames["stable"] {
		t.Error("stable should not be in toStart")
	}
	if len(unchanged) != 1 || unchanged[0].Name != "stable" {
		t.Errorf("expected 1 unchanged (stable), got %d: %v", len(unchanged), targetNames(unchanged))
	}
}

// TestDiffTargets_UnchangedTargets verifies that identical targets in both
// lists appear only in the unchanged set.
func TestDiffTargets_UnchangedTargets(t *testing.T) {
	targets := []config.TargetConfig{
		makeTarget("alpha", "10.0.0.1:443", config.ProbeTypeTCP, 30*time.Second),
		makeTarget("beta", "10.0.0.2:80", config.ProbeTypeHTTP, 60*time.Second),
		makeTarget("gamma", "10.0.0.3:53", config.ProbeTypeDNS, 120*time.Second),
	}

	// Pass identical slices as old and new.
	toStop, toStart, unchanged := diffTargets(targets, targets)

	if len(toStop) != 0 {
		t.Errorf("expected 0 toStop for identical configs, got %d: %v", len(toStop), targetNames(toStop))
	}
	if len(toStart) != 0 {
		t.Errorf("expected 0 toStart for identical configs, got %d: %v", len(toStart), targetNames(toStart))
	}
	if len(unchanged) != 3 {
		t.Fatalf("expected 3 unchanged, got %d: %v", len(unchanged), targetNames(unchanged))
	}
	unchangedNames := targetNames(unchanged)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if !unchangedNames[name] {
			t.Errorf("expected %q in unchanged set", name)
		}
	}
}

// TestDiffTargets_EmptyOld verifies that when old is empty, all new targets
// appear in toStart.
func TestDiffTargets_EmptyOld(t *testing.T) {
	newTargets := []config.TargetConfig{
		makeTarget("a", "10.0.0.1:443", config.ProbeTypeTCP, 30*time.Second),
		makeTarget("b", "10.0.0.2:80", config.ProbeTypeHTTP, 60*time.Second),
	}

	toStop, toStart, unchanged := diffTargets(nil, newTargets)

	if len(toStop) != 0 {
		t.Errorf("expected 0 toStop, got %d", len(toStop))
	}
	if len(unchanged) != 0 {
		t.Errorf("expected 0 unchanged, got %d", len(unchanged))
	}
	if len(toStart) != 2 {
		t.Fatalf("expected 2 toStart, got %d", len(toStart))
	}
}

// TestDiffTargets_EmptyNew verifies that when new is empty, all old targets
// appear in toStop.
func TestDiffTargets_EmptyNew(t *testing.T) {
	oldTargets := []config.TargetConfig{
		makeTarget("a", "10.0.0.1:443", config.ProbeTypeTCP, 30*time.Second),
		makeTarget("b", "10.0.0.2:80", config.ProbeTypeHTTP, 60*time.Second),
	}

	toStop, toStart, unchanged := diffTargets(oldTargets, nil)

	if len(toStart) != 0 {
		t.Errorf("expected 0 toStart, got %d", len(toStart))
	}
	if len(unchanged) != 0 {
		t.Errorf("expected 0 unchanged, got %d", len(unchanged))
	}
	if len(toStop) != 2 {
		t.Fatalf("expected 2 toStop, got %d", len(toStop))
	}
}

// TestDiffTargets_BothEmpty verifies that diffing two empty lists produces
// three empty sets.
func TestDiffTargets_BothEmpty(t *testing.T) {
	toStop, toStart, unchanged := diffTargets(nil, nil)

	if len(toStop) != 0 || len(toStart) != 0 || len(unchanged) != 0 {
		t.Errorf("expected all empty sets, got toStop=%d toStart=%d unchanged=%d",
			len(toStop), len(toStart), len(unchanged))
	}
}

// TestDiffTargets_MixedOperations verifies a realistic scenario with
// simultaneous additions, removals, changes, and unchanged targets.
func TestDiffTargets_MixedOperations(t *testing.T) {
	oldTargets := []config.TargetConfig{
		makeTarget("keep", "10.0.0.1:443", config.ProbeTypeTCP, 30*time.Second),
		makeTarget("remove", "10.0.0.2:443", config.ProbeTypeTCP, 30*time.Second),
		makeTarget("change", "10.0.0.3:443", config.ProbeTypeTCP, 30*time.Second),
	}
	newTargets := []config.TargetConfig{
		makeTarget("keep", "10.0.0.1:443", config.ProbeTypeTCP, 30*time.Second),
		makeTarget("add", "10.0.0.4:443", config.ProbeTypeTCP, 30*time.Second),
		makeTarget("change", "10.0.0.3:443", config.ProbeTypeHTTP, 30*time.Second), // type changed
	}

	toStop, toStart, unchanged := diffTargets(oldTargets, newTargets)

	stopNames := targetNames(toStop)
	startNames := targetNames(toStart)
	unchangedNames := targetNames(unchanged)

	// "remove" should be stopped.
	if !stopNames["remove"] {
		t.Error("expected 'remove' in toStop")
	}
	// "change" should be in both stop and start.
	if !stopNames["change"] {
		t.Error("expected 'change' in toStop")
	}
	if !startNames["change"] {
		t.Error("expected 'change' in toStart")
	}
	// "add" should be started.
	if !startNames["add"] {
		t.Error("expected 'add' in toStart")
	}
	// "keep" should be unchanged.
	if !unchangedNames["keep"] {
		t.Error("expected 'keep' in unchanged")
	}
	// "keep" should not appear in stop or start.
	if stopNames["keep"] || startNames["keep"] {
		t.Error("'keep' should not appear in toStop or toStart")
	}
	// "add" should not appear in stop.
	if stopNames["add"] {
		t.Error("'add' should not appear in toStop")
	}
	// "remove" should not appear in start or unchanged.
	if startNames["remove"] || unchangedNames["remove"] {
		t.Error("'remove' should not appear in toStart or unchanged")
	}
}

// TestDiffTargets_TagChange verifies that a change in tags (labels) is
// detected as a configuration change.
func TestDiffTargets_TagChange(t *testing.T) {
	oldTargets := []config.TargetConfig{
		{
			Name: "tagged", Address: "10.0.0.1:443",
			ProbeType: config.ProbeTypeTCP, Interval: 30 * time.Second, Timeout: 15 * time.Second,
			Tags: map[string]string{"service": "api", "scope": "same-region"},
		},
	}
	newTargets := []config.TargetConfig{
		{
			Name: "tagged", Address: "10.0.0.1:443",
			ProbeType: config.ProbeTypeTCP, Interval: 30 * time.Second, Timeout: 15 * time.Second,
			Tags: map[string]string{"service": "api", "scope": "cross-region"},
		},
	}

	toStop, toStart, unchanged := diffTargets(oldTargets, newTargets)

	if len(unchanged) != 0 {
		t.Errorf("expected 0 unchanged when tags differ, got %d", len(unchanged))
	}
	if len(toStop) != 1 || toStop[0].Tags["scope"] != "same-region" {
		t.Errorf("expected toStop to contain old config with scope=same-region")
	}
	if len(toStart) != 1 || toStart[0].Tags["scope"] != "cross-region" {
		t.Errorf("expected toStart to contain new config with scope=cross-region")
	}
}

// ---------- Scheduler.Reload() Unit Tests (#10) ----------

// countingProber records how many times Probe was called and returns a
// configurable result. It blocks until the context is cancelled.
type countingProber struct {
	calls int
	mu    sync.Mutex
}

func (p *countingProber) Probe(ctx context.Context, _ config.TargetConfig) probe.ProbeResult {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	<-ctx.Done()
	return probe.ProbeResult{Success: true, Duration: time.Millisecond}
}

func (p *countingProber) CallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// waitForTargets polls s.Targets() until it equals expected or the deadline
// passes. Returns the final count.
func waitForTargets(s *Scheduler, expected int, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.Targets() == expected {
			return expected
		}
		time.Sleep(5 * time.Millisecond)
	}
	return s.Targets()
}

func TestReload_AddTarget(t *testing.T) {
	me := metrics.NewMetricsExporter(nil)
	factory := func(_ config.TargetConfig) probe.Prober { return &noopProber{} }
	s := New(me, factory)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	initial := config.Config{
		Agent:   config.AgentConfig{DefaultInterval: 30 * time.Second},
		Targets: []config.TargetConfig{makeTarget("a", "10.0.0.1:80", config.ProbeTypeTCP, 30*time.Second)},
	}
	s.Start(ctx, initial)
	waitForTargets(s, 1, 2*time.Second)

	// Reload with an additional target.
	reloaded := config.Config{
		Agent: config.AgentConfig{DefaultInterval: 30 * time.Second},
		Targets: []config.TargetConfig{
			makeTarget("a", "10.0.0.1:80", config.ProbeTypeTCP, 30*time.Second),
			makeTarget("b", "10.0.0.2:80", config.ProbeTypeTCP, 30*time.Second),
		},
	}
	s.Reload(ctx, reloaded)

	got := waitForTargets(s, 2, 2*time.Second)
	if got != 2 {
		t.Fatalf("after adding target: Targets() = %d, want 2", got)
	}

	s.Stop()
}

func TestReload_RemoveTarget(t *testing.T) {
	me := metrics.NewMetricsExporter(nil)
	factory := func(_ config.TargetConfig) probe.Prober { return &noopProber{} }
	s := New(me, factory)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	initial := config.Config{
		Agent: config.AgentConfig{DefaultInterval: 30 * time.Second},
		Targets: []config.TargetConfig{
			makeTarget("a", "10.0.0.1:80", config.ProbeTypeTCP, 30*time.Second),
			makeTarget("b", "10.0.0.2:80", config.ProbeTypeTCP, 30*time.Second),
		},
	}
	s.Start(ctx, initial)
	waitForTargets(s, 2, 2*time.Second)

	// Reload with only target "a".
	reloaded := config.Config{
		Agent:   config.AgentConfig{DefaultInterval: 30 * time.Second},
		Targets: []config.TargetConfig{makeTarget("a", "10.0.0.1:80", config.ProbeTypeTCP, 30*time.Second)},
	}
	s.Reload(ctx, reloaded)

	got := waitForTargets(s, 1, 2*time.Second)
	if got != 1 {
		t.Fatalf("after removing target: Targets() = %d, want 1", got)
	}

	s.Stop()
}

func TestReload_ChangeTarget(t *testing.T) {
	me := metrics.NewMetricsExporter(nil)
	var probers []*countingProber
	var mu sync.Mutex
	factory := func(_ config.TargetConfig) probe.Prober {
		p := &countingProber{}
		mu.Lock()
		probers = append(probers, p)
		mu.Unlock()
		return p
	}
	s := New(me, factory)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	initial := config.Config{
		Agent:   config.AgentConfig{DefaultInterval: 30 * time.Second},
		Targets: []config.TargetConfig{makeTarget("a", "10.0.0.1:80", config.ProbeTypeTCP, 30*time.Second)},
	}
	s.Start(ctx, initial)
	waitForTargets(s, 1, 2*time.Second)

	mu.Lock()
	proberCountBefore := len(probers)
	mu.Unlock()

	// Reload with changed interval for target "a".
	reloaded := config.Config{
		Agent:   config.AgentConfig{DefaultInterval: 30 * time.Second},
		Targets: []config.TargetConfig{makeTarget("a", "10.0.0.1:80", config.ProbeTypeTCP, 60*time.Second)},
	}
	s.Reload(ctx, reloaded)

	// Wait for the new goroutine to start.
	waitForTargets(s, 1, 2*time.Second)

	mu.Lock()
	proberCountAfter := len(probers)
	mu.Unlock()

	// A new prober should have been created for the changed target.
	if proberCountAfter <= proberCountBefore {
		t.Fatalf("expected new prober to be created after change, before=%d after=%d", proberCountBefore, proberCountAfter)
	}

	s.Stop()
}

func TestReload_UnchangedTargetContinues(t *testing.T) {
	me := metrics.NewMetricsExporter(nil)
	var proberCount int
	var mu sync.Mutex
	factory := func(_ config.TargetConfig) probe.Prober {
		mu.Lock()
		proberCount++
		mu.Unlock()
		return &noopProber{}
	}
	s := New(me, factory)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := config.Config{
		Agent:   config.AgentConfig{DefaultInterval: 30 * time.Second},
		Targets: []config.TargetConfig{makeTarget("a", "10.0.0.1:80", config.ProbeTypeTCP, 30*time.Second)},
	}
	s.Start(ctx, cfg)
	waitForTargets(s, 1, 2*time.Second)

	mu.Lock()
	countBefore := proberCount
	mu.Unlock()

	// Reload with the same config — no targets should be restarted.
	s.Reload(ctx, cfg)

	// Give a moment for any spurious restarts.
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	countAfter := proberCount
	mu.Unlock()

	if countAfter != countBefore {
		t.Fatalf("unchanged target should not create new prober: before=%d after=%d", countBefore, countAfter)
	}

	s.Stop()
}

// ---------- executeProbe post-cancel discard test (#11) ----------

// blockingProber blocks in Probe() until released via a channel, then returns
// a successful result. This allows tests to control exactly when a probe
// completes relative to context cancellation.
type blockingProber struct {
	started chan struct{} // closed when Probe() begins
	release chan struct{} // close to let Probe() return
}

func newBlockingProber() *blockingProber {
	return &blockingProber{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (p *blockingProber) Probe(_ context.Context, _ config.TargetConfig) probe.ProbeResult {
	close(p.started)
	<-p.release
	return probe.ProbeResult{Success: true, Duration: time.Millisecond}
}

func TestExecuteProbe_DiscardsResultAfterContextCancel(t *testing.T) {
	// This test verifies that when the parent context is cancelled (e.g. by
	// Reload removing a target) while a probe is in flight, the result is
	// discarded and Record is NOT called on the exporter.
	me := metrics.NewMetricsExporter(nil)

	target := config.TargetConfig{
		Name:      "discard-test",
		Address:   "10.0.0.1:80",
		ProbeType: config.ProbeTypeTCP,
		Interval:  30 * time.Second,
		Timeout:   30 * time.Second,
	}

	bp := newBlockingProber()

	// Create a cancellable context to simulate Reload cancelling the target.
	ctx, cancel := context.WithCancel(context.Background())

	// Run executeProbe in a goroutine since it will block.
	done := make(chan struct{})
	go func() {
		defer close(done)
		state := &probeState{}
		executeProbe(ctx, target, bp, me, state)
	}()

	// Wait for the prober to start.
	<-bp.started

	// Cancel the context (simulating Reload removing this target).
	cancel()

	// Release the prober so it returns its result.
	close(bp.release)

	// Wait for executeProbe to finish.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("executeProbe did not return within 5 seconds")
	}

	// Verify that no metrics were recorded for this target. If the discard
	// guard works, probe_success should have no series for "discard-test".
	families, err := me.Registry().Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() == "probe_success" {
			for _, m := range fam.GetMetric() {
				for _, lp := range m.GetLabel() {
					if lp.GetName() == "target_name" && lp.GetValue() == "discard-test" {
						t.Fatal("probe_success was recorded for cancelled target — discard guard failed")
					}
				}
			}
		}
	}
}
