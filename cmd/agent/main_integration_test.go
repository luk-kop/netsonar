//go:build integration

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"go.yaml.in/yaml/v4"

	"netsonar/internal/config"
	"netsonar/internal/metrics"
	"netsonar/internal/probe"
	"netsonar/internal/scheduler"
)

// TestIntegration_MetricsEndpoint starts the agent in-process, runs a TCP
// probe against a local listener, scrapes /metrics, and verifies that all
// expected metric names and labels are present.
func TestIntegration_MetricsEndpoint(t *testing.T) {
	// --- 1. Start a local TCP listener as the probe target ---
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start TCP listener: %v", err)
	}
	t.Cleanup(func() { tcpListener.Close() })

	// Accept connections in background so the TCP probe succeeds.
	go func() {
		for {
			conn, err := tcpListener.Accept()
			if err != nil {
				return // listener closed
			}
			conn.Close()
		}
	}()

	tcpAddr := tcpListener.Addr().String()
	_, tcpPort, _ := net.SplitHostPort(tcpAddr)

	// --- 2. Write a temporary YAML config ---
	cfgData := map[string]interface{}{
		"agent": map[string]interface{}{
			"listen_addr":      ":0",
			"metrics_path":     "/metrics",
			"default_interval": "30s",
			"log_level":        "info",
		},
		"targets": []map[string]interface{}{
			{
				"name":       "test-tcp-target",
				"address":    tcpAddr,
				"probe_type": "tcp",
				"interval":   "5s",
				"timeout":    "2s",
				"tags": map[string]string{
					"service":          "test-svc",
					"scope":            "local",
					"provider":         "test",
					"target_region":    "test-region",
					"target_partition": "test",
					"visibility":       "internal",
					"port":             tcpPort,
					"impact":           "low",
				},
			},
		},
	}

	cfgBytes, err := yaml.Marshal(cfgData)
	if err != nil {
		t.Fatalf("failed to marshal config YAML: %v", err)
	}

	tmpFile, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	if err != nil {
		t.Fatalf("failed to create temp config file: %v", err)
	}
	if _, err := tmpFile.Write(cfgBytes); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
	tmpFile.Close()

	// --- 3. Load config via the real config loader ---
	cfg, err := config.LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	// --- 4. Wire up the agent components in-process ---
	exporter := metrics.NewMetricsExporter(config.CollectTagKeys(cfg), metrics.ExporterOptions{EnableRuntimeMetrics: cfg.Agent.EnableRuntimeMetrics})

	proberFactory := func(target config.TargetConfig) probe.Prober {
		return &probe.TCPProber{}
	}

	sched := scheduler.New(exporter, proberFactory)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		sched.Stop()
		cancel()
	})

	sched.Start(ctx, *cfg)

	// Set agent metadata metrics (mirrors main.go updateAgentMetrics).
	exporter.SetBuildInfo("test", "test-revision", "2026-04-26T12:00:00Z")
	hash, _ := config.ComputeHash(cfg)
	exporter.SetConfigInfo(hash)
	exporter.SetTargetsTotal(len(cfg.Targets))
	exporter.SetConfigReloadTimestamp(time.Now())

	// --- 5. Start HTTP server on a free port ---
	metricsListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start metrics listener: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", exporter.Handler())
	srv := &http.Server{Handler: mux}

	go srv.Serve(metricsListener)
	t.Cleanup(func() {
		srv.Shutdown(context.Background())
	})

	metricsURL := fmt.Sprintf("http://%s/metrics", metricsListener.Addr().String())

	// --- 6. Wait for the first probe to execute, then scrape ---
	time.Sleep(500 * time.Millisecond)

	resp, err := http.Get(metricsURL)
	if err != nil {
		t.Fatalf("failed to scrape /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics returned status %d, want 200", resp.StatusCode)
	}

	// --- 7. Parse the Prometheus text exposition response ---
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		// expfmt may return partial results with a non-nil error for
		// trailing whitespace; only fail if we got zero families.
		if len(families) == 0 {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("failed to parse metrics response: %v\nbody: %s", err, string(body))
		}
	}

	// --- 8. Verify expected metric names ---
	expectedMetrics := []string{
		"probe_success",
		"probe_duration_seconds",
		"netsonar_build_info",
		"netsonar_targets_total",
		"netsonar_config_reload_timestamp_seconds",
	}

	for _, name := range expectedMetrics {
		if _, ok := families[name]; !ok {
			t.Errorf("expected metric %q not found in /metrics response", name)
		}
	}

	// --- 9. Verify common labels on probe metrics ---
	expectedLabels := []string{
		"target",
		"probe_type",
		"network_path",
		"service",
		"scope",
		"provider",
		"target_region",
		"target_partition",
		"visibility",
		"port",
		"impact",
	}

	probeMetrics := []string{"probe_success", "probe_duration_seconds"}
	for _, metricName := range probeMetrics {
		fam, ok := families[metricName]
		if !ok {
			continue // already reported above
		}
		if len(fam.GetMetric()) == 0 {
			t.Errorf("metric %q has no time series", metricName)
			continue
		}

		metric := fam.GetMetric()[0]
		labelSet := make(map[string]string)
		for _, lp := range metric.GetLabel() {
			labelSet[lp.GetName()] = lp.GetValue()
		}

		for _, label := range expectedLabels {
			if _, ok := labelSet[label]; !ok {
				t.Errorf("metric %q missing expected label %q", metricName, label)
			}
		}
	}

	// --- 10. Verify netsonar_build_info carries build metadata only ---
	if fam, ok := families["netsonar_build_info"]; ok {
		if len(fam.GetMetric()) == 0 {
			t.Error("netsonar_build_info has no time series")
		} else {
			metric := fam.GetMetric()[0]
			labelSet := make(map[string]string)
			for _, lp := range metric.GetLabel() {
				labelSet[lp.GetName()] = lp.GetValue()
			}
			wantLabels := map[string]string{
				"version":    "test",
				"revision":   "test-revision",
				"build_date": "2026-04-26T12:00:00Z",
			}
			for name, want := range wantLabels {
				if got := labelSet[name]; got != want {
					t.Errorf("netsonar_build_info label %q = %q, want %q", name, got, want)
				}
			}
			if _, ok := labelSet["config_hash"]; ok {
				t.Error("netsonar_build_info must not carry legacy config_hash label")
			}
			for _, label := range expectedLabels {
				if _, ok := labelSet[label]; ok {
					t.Errorf("netsonar_build_info must not carry probe label %q", label)
				}
			}
		}
	}

	// --- 11. Verify netsonar_config_info exposes the current config hash ---
	if fam, ok := families["netsonar_config_info"]; ok {
		if len(fam.GetMetric()) != 1 {
			t.Errorf("netsonar_config_info has %d series, want 1", len(fam.GetMetric()))
		} else {
			var hash string
			for _, lp := range fam.GetMetric()[0].GetLabel() {
				if lp.GetName() == "hash" {
					hash = lp.GetValue()
				}
			}
			if hash == "" {
				t.Error("netsonar_config_info hash label is empty")
			}
		}
	} else {
		t.Error("netsonar_config_info metric not found")
	}
}

// TestIntegration_ConfigReloadSIGHUP verifies that reloading the configuration
// (simulating SIGHUP) correctly adds new targets, removes old targets, and
// updates the netsonar_targets_total metric. It exercises the same code path as
// the real SIGHUP handler in main.go: re-read config → validate → scheduler.Reload.
func TestIntegration_ConfigReloadSIGHUP(t *testing.T) {
	// --- 1. Start two local TCP listeners as probe targets ---
	// Listener A is the initial target; listener B is the post-reload target.
	listenerA, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener A: %v", err)
	}
	t.Cleanup(func() { listenerA.Close() })

	listenerB, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener B: %v", err)
	}
	t.Cleanup(func() { listenerB.Close() })

	// Accept connections in background so TCP probes succeed.
	for _, ln := range []net.Listener{listenerA, listenerB} {
		ln := ln
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				conn.Close()
			}
		}()
	}

	addrA := listenerA.Addr().String()
	_, portA, _ := net.SplitHostPort(addrA)
	addrB := listenerB.Addr().String()
	_, portB, _ := net.SplitHostPort(addrB)

	// --- 2. Write initial config with target-A only ---
	tmpDir := t.TempDir()
	configPath := tmpDir + "/config.yaml"

	writeConfig := func(targets []map[string]interface{}) {
		t.Helper()
		cfgData := map[string]interface{}{
			"agent": map[string]interface{}{
				"listen_addr":      ":0",
				"metrics_path":     "/metrics",
				"default_interval": "30s",
				"log_level":        "info",
			},
			"targets": targets,
		}
		cfgBytes, err := yaml.Marshal(cfgData)
		if err != nil {
			t.Fatalf("failed to marshal config: %v", err)
		}
		if err := os.WriteFile(configPath, cfgBytes, 0644); err != nil {
			t.Fatalf("failed to write config: %v", err)
		}
	}

	targetA := map[string]interface{}{
		"name":       "target-a",
		"address":    addrA,
		"probe_type": "tcp",
		"interval":   "5s",
		"timeout":    "2s",
		"tags": map[string]string{
			"service":          "svc-a",
			"scope":            "local",
			"provider":         "test",
			"target_region":    "test-region",
			"target_partition": "test",
			"visibility":       "internal",
			"port":             portA,
			"impact":           "low",
		},
	}

	targetB := map[string]interface{}{
		"name":       "target-b",
		"address":    addrB,
		"probe_type": "tcp",
		"interval":   "5s",
		"timeout":    "2s",
		"tags": map[string]string{
			"service":          "svc-b",
			"scope":            "local",
			"provider":         "test",
			"target_region":    "test-region",
			"target_partition": "test",
			"visibility":       "internal",
			"port":             portB,
			"impact":           "high",
		},
	}

	writeConfig([]map[string]interface{}{targetA})

	// --- 3. Load initial config and start agent components ---
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig (initial) failed: %v", err)
	}

	tagKeys := config.CollectTagKeys(cfg)
	exporter := metrics.NewMetricsExporter(tagKeys, metrics.ExporterOptions{EnableRuntimeMetrics: cfg.Agent.EnableRuntimeMetrics})
	proberFactory := func(target config.TargetConfig) probe.Prober {
		return &probe.TCPProber{}
	}
	sched := scheduler.New(exporter, proberFactory)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		sched.Stop()
		cancel()
	})

	sched.Start(ctx, *cfg)
	exporter.SetBuildInfo("test", "test-revision", "2026-04-26T12:00:00Z")
	hash, _ := config.ComputeHash(cfg)
	exporter.SetConfigInfo(hash)
	exporter.SetTargetsTotal(len(cfg.Targets))
	exporter.SetConfigReloadTimestamp(time.Now())

	// --- 4. Start HTTP server on a free port ---
	metricsListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start metrics listener: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", exporter.Handler())
	srv := &http.Server{Handler: mux}
	go srv.Serve(metricsListener)
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	metricsURL := fmt.Sprintf("http://%s/metrics", metricsListener.Addr().String())

	// scrapeAndParse fetches /metrics and returns parsed metric families.
	scrapeAndParse := func() map[string]*dto.MetricFamily {
		t.Helper()
		resp, err := http.Get(metricsURL)
		if err != nil {
			t.Fatalf("failed to scrape /metrics: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("/metrics returned status %d", resp.StatusCode)
		}
		parser := expfmt.NewTextParser(model.LegacyValidation)
		families, err := parser.TextToMetricFamilies(resp.Body)
		if err != nil && len(families) == 0 {
			t.Fatalf("failed to parse metrics: %v", err)
		}
		return families
	}

	// findTargetAddresses extracts all "target" label values from probe_success.
	findTargetAddresses := func(families map[string]*dto.MetricFamily) map[string]bool {
		t.Helper()
		addrs := make(map[string]bool)
		fam, ok := families["probe_success"]
		if !ok {
			return addrs
		}
		for _, m := range fam.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "target" {
					addrs[lp.GetValue()] = true
				}
			}
		}
		return addrs
	}

	// getAgentTargetsTotal reads the netsonar_targets_total gauge value.
	getAgentTargetsTotal := func(families map[string]*dto.MetricFamily) float64 {
		t.Helper()
		fam, ok := families["netsonar_targets_total"]
		if !ok {
			t.Fatal("netsonar_targets_total not found")
		}
		if len(fam.GetMetric()) == 0 {
			t.Fatal("netsonar_targets_total has no time series")
		}
		return fam.GetMetric()[0].GetGauge().GetValue()
	}

	// --- 5. Verify initial state: only target-A is present ---
	time.Sleep(500 * time.Millisecond)

	families := scrapeAndParse()
	addrs := findTargetAddresses(families)

	if !addrs[addrA] {
		t.Errorf("initial scrape: expected target-a address %q in probe_success, got %v", addrA, addrs)
	}
	if addrs[addrB] {
		t.Errorf("initial scrape: target-b address %q should not be present before reload", addrB)
	}
	if total := getAgentTargetsTotal(families); total != 1 {
		t.Errorf("initial netsonar_targets_total = %v, want 1", total)
	}

	// --- 6. Overwrite config: remove target-A, add target-B ---
	writeConfig([]map[string]interface{}{targetB})

	// Simulate SIGHUP by calling handleReload (same code path as the signal handler).
	handleReload(ctx, configPath, sched, exporter, tagKeys, "text", ":0", "/metrics", false)

	// --- 7. Wait for the new probe to execute, then scrape again ---
	time.Sleep(500 * time.Millisecond)

	families = scrapeAndParse()
	addrs = findTargetAddresses(families)

	if !addrs[addrB] {
		t.Errorf("post-reload scrape: expected target-b address %q in probe_success, got %v", addrB, addrs)
	}
	if total := getAgentTargetsTotal(families); total != 1 {
		t.Errorf("post-reload netsonar_targets_total = %v, want 1", total)
	}

	// --- 8. Verify netsonar_config_reload_timestamp_seconds was updated ---
	if fam, ok := families["netsonar_config_reload_timestamp_seconds"]; ok {
		if len(fam.GetMetric()) == 0 {
			t.Error("netsonar_config_reload_timestamp_seconds has no time series")
		} else {
			ts := fam.GetMetric()[0].GetGauge().GetValue()
			if ts <= 0 {
				t.Errorf("netsonar_config_reload_timestamp_seconds = %v, want > 0", ts)
			}
		}
	} else {
		t.Error("netsonar_config_reload_timestamp_seconds not found after reload")
	}

	// --- 9. Test reload with both targets (additive reload) ---
	writeConfig([]map[string]interface{}{targetA, targetB})
	handleReload(ctx, configPath, sched, exporter, tagKeys, "text", ":0", "/metrics", false)

	time.Sleep(500 * time.Millisecond)

	families = scrapeAndParse()
	addrs = findTargetAddresses(families)

	if !addrs[addrA] {
		t.Errorf("additive reload: expected target-a address %q in probe_success", addrA)
	}
	if !addrs[addrB] {
		t.Errorf("additive reload: expected target-b address %q in probe_success", addrB)
	}
	if total := getAgentTargetsTotal(families); total != 2 {
		t.Errorf("additive reload netsonar_targets_total = %v, want 2", total)
	}

	// --- 10. Test reload with invalid config (agent keeps previous config) ---
	if err := os.WriteFile(configPath, []byte("invalid: yaml: [[["), 0644); err != nil {
		t.Fatalf("failed to write invalid config: %v", err)
	}

	handleReload(ctx, configPath, sched, exporter, tagKeys, "text", ":0", "/metrics", false)

	// Agent should still be running with the previous 2-target config.
	time.Sleep(200 * time.Millisecond)

	families = scrapeAndParse()
	addrs = findTargetAddresses(families)

	if !addrs[addrA] || !addrs[addrB] {
		t.Errorf("after invalid reload: expected both targets still present, got %v", addrs)
	}
	if total := getAgentTargetsTotal(families); total != 2 {
		t.Errorf("after invalid reload netsonar_targets_total = %v, want 2 (should keep previous config)", total)
	}
}

// TestIntegration_GracefulShutdown verifies that calling the shutdown function
// (the same code path triggered by SIGTERM/SIGINT) completes within 5 seconds,
// stops all probe goroutines, shuts down the HTTP server, and returns exit
// code 0.
func TestIntegration_GracefulShutdown(t *testing.T) {
	// --- 1. Start a local TCP listener as the probe target ---
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start TCP listener: %v", err)
	}
	t.Cleanup(func() { tcpListener.Close() })

	go func() {
		for {
			conn, err := tcpListener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	tcpAddr := tcpListener.Addr().String()
	_, tcpPort, _ := net.SplitHostPort(tcpAddr)

	// --- 2. Write a temporary YAML config with two targets ---
	cfgData := map[string]interface{}{
		"agent": map[string]interface{}{
			"listen_addr":      ":0",
			"metrics_path":     "/metrics",
			"default_interval": "30s",
			"log_level":        "info",
		},
		"targets": []map[string]interface{}{
			{
				"name":       "shutdown-target-1",
				"address":    tcpAddr,
				"probe_type": "tcp",
				"interval":   "5s",
				"timeout":    "2s",
				"tags": map[string]string{
					"service":          "test-svc",
					"scope":            "local",
					"provider":         "test",
					"target_region":    "test-region",
					"target_partition": "test",
					"visibility":       "internal",
					"port":             tcpPort,
					"impact":           "low",
				},
			},
			{
				"name":       "shutdown-target-2",
				"address":    tcpAddr,
				"probe_type": "tcp",
				"interval":   "5s",
				"timeout":    "2s",
				"tags": map[string]string{
					"service":          "test-svc-2",
					"scope":            "local",
					"provider":         "test",
					"target_region":    "test-region",
					"target_partition": "test",
					"visibility":       "internal",
					"port":             tcpPort,
					"impact":           "low",
				},
			},
		},
	}

	cfgBytes, err := yaml.Marshal(cfgData)
	if err != nil {
		t.Fatalf("failed to marshal config YAML: %v", err)
	}

	tmpFile, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	if err != nil {
		t.Fatalf("failed to create temp config file: %v", err)
	}
	if _, err := tmpFile.Write(cfgBytes); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
	tmpFile.Close()

	// --- 3. Load config and start agent components ---
	cfg, err := config.LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	exporter := metrics.NewMetricsExporter(config.CollectTagKeys(cfg), metrics.ExporterOptions{EnableRuntimeMetrics: cfg.Agent.EnableRuntimeMetrics})
	proberFactory := func(target config.TargetConfig) probe.Prober {
		return &probe.TCPProber{}
	}
	sched := scheduler.New(exporter, proberFactory)

	ctx, cancel := context.WithCancel(context.Background())
	// No t.Cleanup for sched.Stop/cancel — shutdown() will handle that.

	sched.Start(ctx, *cfg)
	exporter.SetBuildInfo("test", "test-revision", "2026-04-26T12:00:00Z")
	hash, _ := config.ComputeHash(cfg)
	exporter.SetConfigInfo(hash)
	exporter.SetTargetsTotal(len(cfg.Targets))
	exporter.SetConfigReloadTimestamp(time.Now())

	// --- 4. Start HTTP server on a free port ---
	metricsListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start metrics listener: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", exporter.Handler())
	srv := &http.Server{Handler: mux}

	go srv.Serve(metricsListener)

	metricsURL := fmt.Sprintf("http://%s/metrics", metricsListener.Addr().String())

	// --- 5. Verify the agent is serving metrics before shutdown ---
	time.Sleep(500 * time.Millisecond)

	resp, err := http.Get(metricsURL)
	if err != nil {
		t.Fatalf("pre-shutdown: failed to scrape /metrics: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pre-shutdown: /metrics returned status %d, want 200", resp.StatusCode)
	}

	if targets := sched.Targets(); targets != 2 {
		t.Fatalf("pre-shutdown: scheduler has %d targets, want 2", targets)
	}

	// --- 6. Call shutdown() and verify it completes within 5 seconds ---
	shutdownDone := make(chan int, 1)
	start := time.Now()

	go func() {
		exitCode := shutdown(sched, srv, cancel)
		shutdownDone <- exitCode
	}()

	select {
	case exitCode := <-shutdownDone:
		elapsed := time.Since(start)

		// Verify exit code is 0 (clean shutdown).
		if exitCode != 0 {
			t.Errorf("shutdown returned exit code %d, want 0", exitCode)
		}

		// Verify shutdown completed within the 5-second grace period.
		if elapsed > 5*time.Second {
			t.Errorf("shutdown took %v, want ≤ 5s", elapsed)
		}

		t.Logf("shutdown completed in %v with exit code %d", elapsed, exitCode)

	case <-time.After(6 * time.Second):
		t.Fatal("shutdown did not complete within 6 seconds (exceeds 5s grace period)")
	}

	// --- 7. Verify the scheduler has zero running targets ---
	if targets := sched.Targets(); targets != 0 {
		t.Errorf("post-shutdown: scheduler has %d targets, want 0", targets)
	}

	// --- 8. Verify the HTTP server is no longer accepting connections ---
	client := &http.Client{Timeout: 1 * time.Second}
	_, err = client.Get(metricsURL)
	if err == nil {
		t.Error("post-shutdown: HTTP server still accepting connections, expected connection refused")
	}
}

// TestIntegration_ConfigReloadLogLevel verifies that changing agent.log_level
// in the configuration file and calling handleReload (the SIGHUP code path)
// atomically updates the live log level without restarting the agent.
//
// The test walks through the full startup → reload sequence so that the
// code path (setupLogger → configureLogger) is exercised exactly as it
// runs in production.
func TestIntegration_ConfigReloadLogLevel(t *testing.T) {
	// Save and restore global logger state so this test never leaks
	// into other tests in the same binary.
	originalDefault := slog.Default()
	originalLevel := agentLogLevel.Level()
	t.Cleanup(func() {
		slog.SetDefault(originalDefault)
		agentLogLevel.Set(originalLevel)
	})

	// --- 1. Minimal TCP target (log_level reload does not need real probe traffic) ---
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	addr := listener.Addr().String()
	_, port, _ := net.SplitHostPort(addr)

	// --- 2. Write initial config with log_level: info ---
	tmpDir := t.TempDir()
	configPath := tmpDir + "/config.yaml"

	writeConfig := func(logLevel string) {
		t.Helper()
		cfgData := map[string]interface{}{
			"agent": map[string]interface{}{
				"listen_addr":      ":0",
				"metrics_path":     "/metrics",
				"default_interval": "30s",
				"log_level":        logLevel,
			},
			"targets": []map[string]interface{}{
				{
					"name":       "loglevel-target",
					"address":    addr,
					"probe_type": "tcp",
					"interval":   "5s",
					"timeout":    "2s",
					"tags": map[string]string{
						"service":          "svc",
						"scope":            "local",
						"provider":         "test",
						"target_region":    "test-region",
						"target_partition": "test",
						"visibility":       "internal",
						"port":             port,
						"impact":           "low",
					},
				},
			},
		}
		cfgBytes, err := yaml.Marshal(cfgData)
		if err != nil {
			t.Fatalf("failed to marshal config: %v", err)
		}
		if err := os.WriteFile(configPath, cfgBytes, 0644); err != nil {
			t.Fatalf("failed to write config: %v", err)
		}
	}

	writeConfig("info")

	// --- 3. Load initial config, wire up the real startup path ---
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig (initial) failed: %v", err)
	}

	setupLogger(cfg.Agent.LogLevel, cfg.Agent.LogFormat)

	ctx := context.Background()

	// Sanity check: after setupLogger("info") the level var is Info.
	if got := agentLogLevel.Level(); got != slog.LevelInfo {
		t.Fatalf("after setupLogger(info): level = %v, want info", got)
	}
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		t.Error("at log_level=info, Debug should be disabled")
	}

	tagKeys := config.CollectTagKeys(cfg)
	exporter := metrics.NewMetricsExporter(tagKeys, metrics.ExporterOptions{EnableRuntimeMetrics: cfg.Agent.EnableRuntimeMetrics})
	proberFactory := func(target config.TargetConfig) probe.Prober {
		return &probe.TCPProber{}
	}
	sched := scheduler.New(exporter, proberFactory)

	schedCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		sched.Stop()
		cancel()
	})

	sched.Start(schedCtx, *cfg)
	updateAgentMetrics(exporter, cfg)

	// --- 4. Rewrite config with log_level: debug and reload ---
	writeConfig("debug")
	handleReload(schedCtx, configPath, sched, exporter, tagKeys, "text", ":0", "/metrics", false)

	if got := agentLogLevel.Level(); got != slog.LevelDebug {
		t.Errorf("after reload to debug: level = %v, want debug", got)
	}
	if !slog.Default().Enabled(ctx, slog.LevelDebug) {
		t.Error("after reload to debug: default logger must enable Debug")
	}

	// --- 5. Reload to error and verify down-level transition ---
	writeConfig("error")
	handleReload(schedCtx, configPath, sched, exporter, tagKeys, "text", ":0", "/metrics", false)

	if got := agentLogLevel.Level(); got != slog.LevelError {
		t.Errorf("after reload to error: level = %v, want error", got)
	}
	if slog.Default().Enabled(ctx, slog.LevelWarn) {
		t.Error("after reload to error: Warn should be disabled")
	}
	if !slog.Default().Enabled(ctx, slog.LevelError) {
		t.Error("after reload to error: Error must still be enabled")
	}

	// --- 6. Rejected reload (invalid log_level) must NOT change the level ---
	if err := os.WriteFile(configPath, []byte(`agent:
  default_interval: 30s
  log_level: DEBUG
targets:
  - name: x
    address: `+addr+`
    probe_type: tcp
    interval: 5s
    timeout: 2s
    tags:
      service: svc
      scope: local
      provider: test
      target_region: test-region
`), 0644); err != nil {
		t.Fatalf("failed to write invalid config: %v", err)
	}
	handleReload(schedCtx, configPath, sched, exporter, tagKeys, "text", ":0", "/metrics", false)

	// Rejected reload keeps previous level (error from step 5).
	if got := agentLogLevel.Level(); got != slog.LevelError {
		t.Errorf("after rejected reload: level = %v, want error (previous)", got)
	}

	// --- 7. Rejected reload (log_format change) must NOT change the level ---
	if err := os.WriteFile(configPath, []byte(`agent:
  default_interval: 30s
  log_level: debug
  log_format: json
targets:
  - name: x
    address: `+addr+`
    probe_type: tcp
    interval: 5s
    timeout: 2s
    tags:
      service: svc
      scope: local
      provider: test
      target_region: test-region
`), 0644); err != nil {
		t.Fatalf("failed to write log_format change config: %v", err)
	}
	handleReload(schedCtx, configPath, sched, exporter, tagKeys, "text", ":0", "/metrics", false)

	if got := agentLogLevel.Level(); got != slog.LevelError {
		t.Errorf("after rejected log_format reload: level = %v, want error (previous)", got)
	}

	// --- 8. Rejected reload (enable_runtime_metrics change) must NOT change the level ---
	if err := os.WriteFile(configPath, []byte(`agent:
  default_interval: 30s
  log_level: debug
  enable_runtime_metrics: true
targets:
  - name: x
    address: `+addr+`
    probe_type: tcp
    interval: 5s
    timeout: 2s
    tags:
      service: svc
      scope: local
      provider: test
      target_region: test-region
`), 0644); err != nil {
		t.Fatalf("failed to write enable_runtime_metrics change config: %v", err)
	}
	handleReload(schedCtx, configPath, sched, exporter, tagKeys, "text", ":0", "/metrics", false)

	if got := agentLogLevel.Level(); got != slog.LevelError {
		t.Errorf("after rejected enable_runtime_metrics reload: level = %v, want error (previous)", got)
	}
}
