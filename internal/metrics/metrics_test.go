package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"netsonar/internal/config"
	"netsonar/internal/probe"

	dto "github.com/prometheus/client_model/go"
)

// testTagKeys is the standard set of tag keys used across most tests.
// It mirrors the typical production configuration.
var testTagKeys = []string{
	"impact", "port", "provider", "scope", "service",
	"target_partition", "target_region", "visibility",
}

// helper: gather all metric families from the exporter's registry.
func gatherMetrics(t *testing.T, m *MetricsExporter) map[string]*dto.MetricFamily {
	t.Helper()
	families, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}
	result := make(map[string]*dto.MetricFamily, len(families))
	for _, f := range families {
		result[f.GetName()] = f
	}
	return result
}

// helper: build a target with all common tags populated.
func makeTarget(name, address string, probeType config.ProbeType, tags map[string]string) config.TargetConfig {
	return config.TargetConfig{
		Name:      name,
		Address:   address,
		ProbeType: probeType,
		Interval:  30 * time.Second,
		Timeout:   5 * time.Second,
		Tags:      tags,
	}
}

// helper: find a label value in a metric's label pairs.
func labelValue(metric *dto.Metric, name string) string {
	for _, lp := range metric.GetLabel() {
		if lp.GetName() == name {
			return lp.GetValue()
		}
	}
	return ""
}

// ---------- Metric Registration Tests ----------

func TestNewMetricsExporter_RegistersAllMetrics(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)

	// Record a minimal result so gauge vecs produce at least one time series.
	target := makeTarget("reg-test", "10.0.0.1:443", config.ProbeTypeTCP, map[string]string{
		"service": "test-svc",
	})
	m.Record(target, probe.ProbeResult{Success: true, Duration: 42 * time.Millisecond})

	families := gatherMetrics(t, m)

	expected := []string{
		"probe_success",
		"probe_duration_seconds",
		"agent_targets_total",
		"agent_config_reload_timestamp",
	}
	for _, name := range expected {
		if _, ok := families[name]; !ok {
			t.Errorf("expected metric %q to be registered, but it was not found", name)
		}
	}
}

func TestNewMetricsExporter_HTTPMetricsRegistered(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)

	target := makeTarget("http-reg", "https://example.com", config.ProbeTypeHTTP, map[string]string{
		"service": "web",
	})
	result := probe.ProbeResult{
		Success:    true,
		Duration:   100 * time.Millisecond,
		StatusCode: 200,
		Phases: map[string]time.Duration{
			"dns_resolve":   10 * time.Millisecond,
			"tcp_connect":   20 * time.Millisecond,
			"tls_handshake": 30 * time.Millisecond,
			"ttfb":          25 * time.Millisecond,
			"transfer":      15 * time.Millisecond,
		},
		CertExpiry: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	m.Record(target, result)

	families := gatherMetrics(t, m)

	httpMetrics := []string{
		"probe_http_status_code",
		"probe_phase_duration_seconds",
		"probe_tls_cert_expiry_timestamp",
	}
	for _, name := range httpMetrics {
		if _, ok := families[name]; !ok {
			t.Errorf("expected HTTP metric %q to be registered after recording HTTP result", name)
		}
	}
}

func TestNewMetricsExporter_ICMPMetricsRegistered(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)

	target := makeTarget("icmp-reg", "10.0.0.1", config.ProbeTypeICMP, nil)
	m.Record(target, probe.ProbeResult{
		Success:    true,
		Duration:   50 * time.Millisecond,
		ICMPAvgRTT: 12 * time.Millisecond,
		PacketLoss: 0.2,
		HopCount:   12,
	})

	families := gatherMetrics(t, m)

	for _, name := range []string{"probe_icmp_packet_loss_ratio", "probe_icmp_avg_rtt_seconds", "probe_icmp_hop_count"} {
		if _, ok := families[name]; !ok {
			t.Errorf("expected ICMP metric %q to be registered", name)
		}
	}
}

func TestNewMetricsExporter_MTUMetricRegistered(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)

	target := makeTarget("mtu-reg", "10.0.0.1", config.ProbeTypeMTU, nil)
	m.Record(target, probe.ProbeResult{Success: true, Duration: 1 * time.Second, PathMTU: 1500})

	families := gatherMetrics(t, m)
	if _, ok := families["probe_mtu_path_bytes"]; !ok {
		t.Error("expected probe_mtu_path_bytes to be registered")
	}
	if _, ok := families["probe_mtu_bytes"]; !ok {
		t.Error("expected probe_mtu_bytes to be registered")
	}
	if _, ok := families["probe_mtu_state"]; !ok {
		t.Error("expected probe_mtu_state to be registered")
	}
}

func TestNewMetricsExporter_DNSMetricsRegistered(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)

	target := makeTarget("dns-reg", "example.com", config.ProbeTypeDNS, nil)
	target.ProbeOpts.DNSExpectedResults = []string{"1.2.3.4"}
	m.Record(target, probe.ProbeResult{
		Success:        true,
		Duration:       20 * time.Millisecond,
		DNSResolveTime: 15 * time.Millisecond,
	})

	families := gatherMetrics(t, m)
	for _, name := range []string{"probe_dns_resolve_seconds", "probe_dns_result_match"} {
		if _, ok := families[name]; !ok {
			t.Errorf("expected DNS metric %q to be registered", name)
		}
	}
}

func TestNewMetricsExporter_HTTPBodyMetricRegistered(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)

	target := makeTarget("body-reg", "https://example.com", config.ProbeTypeHTTPBody, nil)
	m.Record(target, probe.ProbeResult{
		Success:    true,
		Duration:   50 * time.Millisecond,
		StatusCode: 200,
		BodyMatch:  true,
	})

	families := gatherMetrics(t, m)
	if _, ok := families["probe_http_body_match"]; !ok {
		t.Error("expected probe_http_body_match to be registered")
	}
}

func TestNewMetricsExporter_AgentInfoRegistered(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)

	// Set agent info gauges.
	m.SetAgentInfo("1.0.0")
	m.SetConfigInfo("abc123def456")
	m.agentTargetsTotal.Set(5)
	m.agentConfigReloadTS.Set(float64(time.Now().Unix()))

	families := gatherMetrics(t, m)
	for _, name := range []string{"agent_info", "agent_config_info", "agent_targets_total", "agent_config_reload_timestamp"} {
		if _, ok := families[name]; !ok {
			t.Errorf("expected agent metadata metric %q to be registered", name)
		}
	}

	// agent_info should only carry the "version" label now.
	if fam, ok := families["agent_info"]; ok && len(fam.GetMetric()) > 0 {
		for _, lp := range fam.GetMetric()[0].GetLabel() {
			if lp.GetName() == "config_hash" {
				t.Error("agent_info must not carry legacy config_hash label")
			}
		}
	}

	// agent_config_info should carry the hash label.
	if fam, ok := families["agent_config_info"]; ok && len(fam.GetMetric()) > 0 {
		var hashVal string
		for _, lp := range fam.GetMetric()[0].GetLabel() {
			if lp.GetName() == "hash" {
				hashVal = lp.GetValue()
			}
		}
		if hashVal != "abc123def456" {
			t.Errorf("agent_config_info hash = %q, want %q", hashVal, "abc123def456")
		}
	}
}

func TestSetConfigInfo_ResetsPreviousHash(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)

	m.SetConfigInfo("hash-old-111")
	m.SetConfigInfo("hash-new-222")

	families := gatherMetrics(t, m)
	fam, ok := families["agent_config_info"]
	if !ok {
		t.Fatal("agent_config_info not registered")
	}
	if got := len(fam.GetMetric()); got != 1 {
		t.Fatalf("agent_config_info has %d series, want 1 (old hash must be reset)", got)
	}
	for _, lp := range fam.GetMetric()[0].GetLabel() {
		if lp.GetName() == "hash" && lp.GetValue() != "hash-new-222" {
			t.Errorf("agent_config_info hash = %q, want %q", lp.GetValue(), "hash-new-222")
		}
	}
}

func TestNewMetricsExporter_UsesCustomRegistry(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)
	if m.Registry() == nil {
		t.Fatal("expected non-nil custom registry")
	}

	// The custom registry should not contain Go runtime metrics that the
	// default registry includes (e.g. go_goroutines).
	families := gatherMetrics(t, m)
	if _, ok := families["go_goroutines"]; ok {
		t.Error("custom registry should not contain default Go runtime metrics")
	}
}

// ---------- Label Correctness Tests ----------

func TestRecord_CommonLabelsApplied(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)

	tags := map[string]string{
		"service":          "api-gw",
		"scope":            "cross-region",
		"provider":         "aws",
		"target_region":    "eu-central-1",
		"target_partition": "global",
		"visibility":       "public",
		"port":             "443",
		"impact":      "critical",
	}
	target := makeTarget("label-test", "10.0.0.1:443", config.ProbeTypeTCP, tags)
	m.Record(target, probe.ProbeResult{Success: true, Duration: 10 * time.Millisecond})

	families := gatherMetrics(t, m)
	fam, ok := families["probe_success"]
	if !ok {
		t.Fatal("probe_success metric not found")
	}

	if len(fam.GetMetric()) == 0 {
		t.Fatal("probe_success has no time series")
	}

	metric := fam.GetMetric()[0]

	// Verify target and probe_type labels.
	if got := labelValue(metric, "target"); got != "10.0.0.1:443" {
		t.Errorf("target label: got %q, want %q", got, "10.0.0.1:443")
	}
	if got := labelValue(metric, "probe_type"); got != "tcp" {
		t.Errorf("probe_type label: got %q, want %q", got, "tcp")
	}

	// Verify all tag-derived labels.
	for key, want := range tags {
		if got := labelValue(metric, key); got != want {
			t.Errorf("label %q: got %q, want %q", key, got, want)
		}
	}
}

func TestRecord_MissingTagsDefaultToEmpty(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)

	// Only set a subset of tags — the rest should default to "".
	target := makeTarget("sparse-tags", "10.0.0.1:443", config.ProbeTypeTCP, map[string]string{
		"service": "minimal",
	})
	m.Record(target, probe.ProbeResult{Success: true, Duration: 5 * time.Millisecond})

	families := gatherMetrics(t, m)
	fam := families["probe_success"]
	metric := fam.GetMetric()[0]

	// "service" should be set.
	if got := labelValue(metric, "service"); got != "minimal" {
		t.Errorf("service label: got %q, want %q", got, "minimal")
	}

	// Other tag-derived labels should be empty strings.
	for _, key := range []string{"scope", "provider", "target_region", "target_partition", "visibility", "port", "impact"} {
		if got := labelValue(metric, key); got != "" {
			t.Errorf("label %q should be empty for missing tag, got %q", key, got)
		}
	}
}

func TestRecord_NilTagsHandled(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)

	target := makeTarget("nil-tags", "10.0.0.1:80", config.ProbeTypeTCP, nil)
	m.Record(target, probe.ProbeResult{Success: false, Duration: 1 * time.Second, Error: "refused"})

	families := gatherMetrics(t, m)
	fam, ok := families["probe_success"]
	if !ok {
		t.Fatal("probe_success not found after recording with nil tags")
	}
	metric := fam.GetMetric()[0]

	// All tag-derived labels should be empty.
	for _, key := range testTagKeys {
		if got := labelValue(metric, key); got != "" {
			t.Errorf("label %q should be empty for nil tags, got %q", key, got)
		}
	}
}

func TestRecord_HTTPPhaseLabels(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)

	target := makeTarget("phase-labels", "https://example.com", config.ProbeTypeHTTP, map[string]string{
		"service": "web",
	})
	phases := map[string]time.Duration{
		"dns_resolve":   5 * time.Millisecond,
		"tcp_connect":   10 * time.Millisecond,
		"tls_handshake": 15 * time.Millisecond,
		"ttfb":          20 * time.Millisecond,
		"transfer":      8 * time.Millisecond,
	}
	m.Record(target, probe.ProbeResult{
		Success:    true,
		Duration:   58 * time.Millisecond,
		StatusCode: 200,
		Phases:     phases,
	})

	families := gatherMetrics(t, m)
	fam, ok := families["probe_phase_duration_seconds"]
	if !ok {
		t.Fatal("probe_phase_duration_seconds not found")
	}

	// Should have one time series per phase.
	if got := len(fam.GetMetric()); got != len(phases) {
		t.Fatalf("expected %d phase metrics, got %d", len(phases), got)
	}

	// Collect the phase label values.
	foundPhases := make(map[string]bool)
	for _, metric := range fam.GetMetric() {
		phase := labelValue(metric, "phase")
		foundPhases[phase] = true
		// Each phase metric should also carry the common labels.
		if got := labelValue(metric, "target"); got != "https://example.com" {
			t.Errorf("phase %q: target label got %q, want %q", phase, got, "https://example.com")
		}
	}

	for phaseName := range phases {
		if !foundPhases[phaseName] {
			t.Errorf("phase %q not found in metrics", phaseName)
		}
	}
}

func TestRecord_ProxyPhaseLabels(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)

	target := makeTarget("proxy-phase-labels", "https://example.com", config.ProbeTypeProxy, map[string]string{
		"service": "proxy",
	})
	phases := map[string]time.Duration{
		"proxy_dial":    7 * time.Millisecond,
		"proxy_tls":     11 * time.Millisecond,
		"proxy_connect": 13 * time.Millisecond,
	}
	m.Record(target, probe.ProbeResult{
		Success:  true,
		Duration: 40 * time.Millisecond,
		Phases:   phases,
	})

	families := gatherMetrics(t, m)
	fam, ok := families["probe_phase_duration_seconds"]
	if !ok {
		t.Fatal("probe_phase_duration_seconds not found")
	}

	foundPhases := make(map[string]bool)
	for _, metric := range fam.GetMetric() {
		if labelValue(metric, "target_name") != "proxy-phase-labels" {
			continue
		}
		phase := labelValue(metric, "phase")
		foundPhases[phase] = true
		if got := labelValue(metric, "probe_type"); got != "proxy" {
			t.Errorf("phase %q: probe_type label got %q, want %q", phase, got, "proxy")
		}
	}

	for phaseName := range phases {
		if !foundPhases[phaseName] {
			t.Errorf("phase %q not found in metrics", phaseName)
		}
	}
}

func TestRecord_TCPDoesNotEmitPhaseMetrics(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)

	target := makeTarget("tcp-no-phases", "10.0.0.1:443", config.ProbeTypeTCP, nil)
	m.Record(target, probe.ProbeResult{Success: true, Duration: 10 * time.Millisecond})

	families := gatherMetrics(t, m)
	if fam, ok := families["probe_phase_duration_seconds"]; ok {
		for _, metric := range fam.GetMetric() {
			if labelValue(metric, "target_name") == "tcp-no-phases" {
				t.Fatalf("unexpected TCP phase metric: phase=%s", labelValue(metric, "phase"))
			}
		}
	}
}

// ---------- Metric Value Correctness Tests ----------

func TestRecord_SuccessValue(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)
	target := makeTarget("val-success", "10.0.0.1:80", config.ProbeTypeTCP, nil)

	// Record success.
	m.Record(target, probe.ProbeResult{Success: true, Duration: 10 * time.Millisecond})
	families := gatherMetrics(t, m)
	val := families["probe_success"].GetMetric()[0].GetGauge().GetValue()
	if val != 1.0 {
		t.Errorf("probe_success for successful probe: got %f, want 1.0", val)
	}

	// Record failure — should overwrite.
	m.Record(target, probe.ProbeResult{Success: false, Duration: 10 * time.Millisecond, Error: "timeout"})
	families = gatherMetrics(t, m)
	val = families["probe_success"].GetMetric()[0].GetGauge().GetValue()
	if val != 0.0 {
		t.Errorf("probe_success for failed probe: got %f, want 0.0", val)
	}
}

func TestRecord_DurationValue(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)
	target := makeTarget("val-dur", "10.0.0.1:80", config.ProbeTypeTCP, nil)

	dur := 123 * time.Millisecond
	m.Record(target, probe.ProbeResult{Success: true, Duration: dur})

	families := gatherMetrics(t, m)
	val := families["probe_duration_seconds"].GetMetric()[0].GetGauge().GetValue()
	if val != dur.Seconds() {
		t.Errorf("probe_duration_seconds: got %f, want %f", val, dur.Seconds())
	}
}

func TestRecord_ICMPValues(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)
	target := makeTarget("val-icmp", "10.0.0.1", config.ProbeTypeICMP, nil)

	m.Record(target, probe.ProbeResult{
		Success:    true,
		Duration:   50 * time.Millisecond,
		ICMPAvgRTT: 8 * time.Millisecond,
		PacketLoss: 0.4,
		HopCount:   15,
	})

	families := gatherMetrics(t, m)

	loss := families["probe_icmp_packet_loss_ratio"].GetMetric()[0].GetGauge().GetValue()
	if loss != 0.4 {
		t.Errorf("packet_loss: got %f, want 0.4", loss)
	}

	hops := families["probe_icmp_hop_count"].GetMetric()[0].GetGauge().GetValue()
	if hops != 15.0 {
		t.Errorf("hop_count: got %f, want 15.0", hops)
	}

	avgRTT := families["probe_icmp_avg_rtt_seconds"].GetMetric()[0].GetGauge().GetValue()
	if avgRTT != 0.008 {
		t.Errorf("avg_rtt: got %f, want 0.008", avgRTT)
	}
}

func TestRecord_ICMPAvgRTTZeroOnFailure(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)
	target := makeTarget("fail-icmp", "10.0.0.1", config.ProbeTypeICMP, nil)

	m.Record(target, probe.ProbeResult{
		Success:    false,
		Duration:   2 * time.Second,
		PacketLoss: 1.0,
		Error:      "all ICMP echo requests timed out or failed",
	})

	families := gatherMetrics(t, m)
	avgRTT := families["probe_icmp_avg_rtt_seconds"].GetMetric()[0].GetGauge().GetValue()
	if avgRTT != 0 {
		t.Errorf("avg_rtt on failure: got %f, want 0", avgRTT)
	}
}

func TestRecord_MTUValue(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)
	target := makeTarget("val-mtu", "10.0.0.1", config.ProbeTypeMTU, nil)
	target.ProbeOpts.ExpectedMinMTU = 1500

	m.Record(target, probe.ProbeResult{Success: true, Duration: 1 * time.Second, PathMTU: 1500})

	families := gatherMetrics(t, m)
	val := families["probe_mtu_path_bytes"].GetMetric()[0].GetGauge().GetValue()
	if val != 1500.0 {
		t.Errorf("path_mtu: got %f, want 1500.0", val)
	}
	mtuBytes := families["probe_mtu_bytes"].GetMetric()[0].GetGauge().GetValue()
	if mtuBytes != 1500.0 {
		t.Errorf("probe_mtu_bytes: got %f, want 1500.0", mtuBytes)
	}
	stateMetric := families["probe_mtu_state"].GetMetric()[0]
	if got := labelValue(stateMetric, "state"); got != probe.MTUStateOK {
		t.Errorf("state label: got %q, want %q", got, probe.MTUStateOK)
	}
	if got := labelValue(stateMetric, "detail"); got != probe.MTUDetailLargestSizeConfirmed {
		t.Errorf("detail label: got %q, want %q", got, probe.MTUDetailLargestSizeConfirmed)
	}
}

func TestRecord_MTUFailureValue(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)
	target := makeTarget("val-mtu-fail", "10.0.0.1", config.ProbeTypeMTU, nil)

	m.Record(target, probe.ProbeResult{Success: false, Duration: 5 * time.Second, PathMTU: -1, Error: "all sizes failed"})

	families := gatherMetrics(t, m)
	val := families["probe_mtu_path_bytes"].GetMetric()[0].GetGauge().GetValue()
	if val != -1.0 {
		t.Errorf("path_mtu on failure: got %f, want -1.0", val)
	}
	if fam, ok := families["probe_mtu_bytes"]; ok && len(fam.GetMetric()) > 0 {
		t.Error("probe_mtu_bytes should be absent when no MTU size was confirmed")
	}
	stateMetric := families["probe_mtu_state"].GetMetric()[0]
	if got := labelValue(stateMetric, "state"); got != probe.MTUStateDegraded {
		t.Errorf("state label: got %q, want %q", got, probe.MTUStateDegraded)
	}
	if got := labelValue(stateMetric, "detail"); got != probe.MTUDetailAllSizesTimedOut {
		t.Errorf("detail label: got %q, want %q", got, probe.MTUDetailAllSizesTimedOut)
	}
}

func TestRecord_MTUDegradedBelowExpectedMin(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)
	target := makeTarget("val-mtu-low", "10.0.0.1", config.ProbeTypeMTU, nil)
	target.ProbeOpts.ExpectedMinMTU = 1500

	m.Record(target, probe.ProbeResult{Success: true, Duration: 1 * time.Second, PathMTU: 1420})

	families := gatherMetrics(t, m)
	stateMetric := families["probe_mtu_state"].GetMetric()[0]
	if got := labelValue(stateMetric, "state"); got != probe.MTUStateDegraded {
		t.Errorf("state label: got %q, want %q", got, probe.MTUStateDegraded)
	}
	if got := labelValue(stateMetric, "detail"); got != probe.MTUDetailLargestSizeConfirmed {
		t.Errorf("detail label: got %q, want %q", got, probe.MTUDetailLargestSizeConfirmed)
	}
}

func TestRecord_MTUErrorDetails(t *testing.T) {
	tests := []struct {
		name       string
		errText    string
		wantDetail string
	}{
		{name: "permission", errText: "permission denied: CAP_NET_RAW required", wantDetail: probe.MTUDetailPermissionDenied},
		{name: "resolve", errText: "resolve IPv4 address: no such host", wantDetail: probe.MTUDetailResolveError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewMetricsExporter(testTagKeys)
			target := makeTarget("val-mtu-"+tt.name, "10.0.0.1", config.ProbeTypeMTU, nil)

			m.Record(target, probe.ProbeResult{Success: false, Duration: 1 * time.Second, PathMTU: -1, Error: tt.errText})

			families := gatherMetrics(t, m)
			stateMetric := families["probe_mtu_state"].GetMetric()[0]
			if got := labelValue(stateMetric, "state"); got != probe.MTUStateError {
				t.Errorf("state label: got %q, want %q", got, probe.MTUStateError)
			}
			if got := labelValue(stateMetric, "detail"); got != tt.wantDetail {
				t.Errorf("detail label: got %q, want %q", got, tt.wantDetail)
			}
		})
	}
}

func TestRecord_MTUDiagnosticCounters(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)
	target := makeTarget("mtu-counters", "10.0.0.1", config.ProbeTypeMTU, nil)
	target.ProbeOpts.ExpectedMinMTU = 1500

	m.Record(target, probe.ProbeResult{
		Success:            true,
		Duration:           1 * time.Second,
		PathMTU:            1500,
		MTUFragNeededCount: 1,
		MTUTimeoutCount:    2,
		MTURetryCount:      2,
		MTULocalErrorCount: 1,
	})
	m.Record(target, probe.ProbeResult{
		Success:            true,
		Duration:           1 * time.Second,
		PathMTU:            1500,
		MTUFragNeededCount: 2,
		MTUTimeoutCount:    3,
		MTURetryCount:      4,
		MTULocalErrorCount: 5,
	})

	families := gatherMetrics(t, m)
	tests := []struct {
		name string
		want float64
	}{
		{name: "probe_mtu_frag_needed_total", want: 3},
		{name: "probe_mtu_timeouts_total", want: 5},
		{name: "probe_mtu_retries_total", want: 6},
		{name: "probe_mtu_local_errors_total", want: 6},
	}

	for _, tt := range tests {
		fam, ok := families[tt.name]
		if !ok || len(fam.GetMetric()) == 0 {
			t.Fatalf("expected %s to be present", tt.name)
		}
		got := fam.GetMetric()[0].GetCounter().GetValue()
		if got != tt.want {
			t.Errorf("%s = %f, want %f", tt.name, got, tt.want)
		}
	}

	stateMetric := families["probe_mtu_state"].GetMetric()[0]
	if got := labelValue(stateMetric, "state"); got != probe.MTUStateOK {
		t.Errorf("state label: got %q, want %q", got, probe.MTUStateOK)
	}
}

func TestRecord_TLSCertExpiry(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)
	target := makeTarget("val-tls", "example.com:443", config.ProbeTypeTLSCert, nil)

	expiry := time.Date(2027, 6, 15, 12, 0, 0, 0, time.UTC)
	m.Record(target, probe.ProbeResult{Success: true, Duration: 100 * time.Millisecond, CertExpiry: expiry})

	families := gatherMetrics(t, m)
	val := families["probe_tls_cert_expiry_timestamp"].GetMetric()[0].GetGauge().GetValue()
	if val != float64(expiry.Unix()) {
		t.Errorf("cert_expiry: got %f, want %f", val, float64(expiry.Unix()))
	}
}

func TestRecord_HTTPBodyMatchValues(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)
	target := makeTarget("val-body", "https://example.com", config.ProbeTypeHTTPBody, nil)

	// Match = true.
	m.Record(target, probe.ProbeResult{Success: true, Duration: 50 * time.Millisecond, StatusCode: 200, BodyMatch: true})
	families := gatherMetrics(t, m)
	val := families["probe_http_body_match"].GetMetric()[0].GetGauge().GetValue()
	if val != 1.0 {
		t.Errorf("body_match true: got %f, want 1.0", val)
	}

	// Match = false.
	m.Record(target, probe.ProbeResult{Success: false, Duration: 50 * time.Millisecond, StatusCode: 200, BodyMatch: false})
	families = gatherMetrics(t, m)
	val = families["probe_http_body_match"].GetMetric()[0].GetGauge().GetValue()
	if val != 0.0 {
		t.Errorf("body_match false: got %f, want 0.0", val)
	}
}

func TestRecord_DNSValues(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)
	target := makeTarget("val-dns", "example.com", config.ProbeTypeDNS, nil)
	target.ProbeOpts.DNSExpectedResults = []string{"1.2.3.4"}

	resolveTime := 12 * time.Millisecond
	m.Record(target, probe.ProbeResult{
		Success:        true,
		Duration:       15 * time.Millisecond,
		DNSResolveTime: resolveTime,
	})

	families := gatherMetrics(t, m)

	dnsVal := families["probe_dns_resolve_seconds"].GetMetric()[0].GetGauge().GetValue()
	if dnsVal != resolveTime.Seconds() {
		t.Errorf("dns_resolve_seconds: got %f, want %f", dnsVal, resolveTime.Seconds())
	}

	matchVal := families["probe_dns_result_match"].GetMetric()[0].GetGauge().GetValue()
	if matchVal != 1.0 {
		t.Errorf("dns_result_match for success: got %f, want 1.0", matchVal)
	}
}

func TestRecord_DNSResultMatchNotSetWithoutExpected(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)
	target := makeTarget("val-dns-noexp", "example.com", config.ProbeTypeDNS, nil)
	// No DNSExpectedResults set.

	m.Record(target, probe.ProbeResult{
		Success:        true,
		Duration:       10 * time.Millisecond,
		DNSResolveTime: 8 * time.Millisecond,
	})

	families := gatherMetrics(t, m)
	// dns_result_match should not have any time series since no expected results were configured.
	if fam, ok := families["probe_dns_result_match"]; ok && len(fam.GetMetric()) > 0 {
		t.Error("dns_result_match should not be set when DNSExpectedResults is empty")
	}
}

// ---------- Handler / HTTP Endpoint Tests ----------

func TestHandler_ServesPrometheusFormat(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)

	target := makeTarget("handler-test", "10.0.0.1:443", config.ProbeTypeTCP, map[string]string{
		"service": "test",
	})
	m.Record(target, probe.ProbeResult{Success: true, Duration: 42 * time.Millisecond})

	handler := m.Handler()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("handler returned status %d, want 200", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}

	bodyStr := string(body)

	// Should contain Prometheus text exposition format lines.
	if !strings.Contains(bodyStr, "probe_success") {
		t.Error("response body should contain probe_success metric")
	}
	if !strings.Contains(bodyStr, "probe_duration_seconds") {
		t.Error("response body should contain probe_duration_seconds metric")
	}
	// Verify label presence in the output.
	if !strings.Contains(bodyStr, `target="10.0.0.1:443"`) {
		t.Error("response body should contain target label")
	}
	if !strings.Contains(bodyStr, `probe_type="tcp"`) {
		t.Error("response body should contain probe_type label")
	}
}

// ---------- Concurrent Scrape Safety Tests ----------

func TestRecord_ConcurrentSafety(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)

	targets := []config.TargetConfig{
		makeTarget("conc-tcp", "10.0.0.1:80", config.ProbeTypeTCP, map[string]string{"service": "a"}),
		makeTarget("conc-http", "https://example.com", config.ProbeTypeHTTP, map[string]string{"service": "b"}),
		makeTarget("conc-icmp", "10.0.0.2", config.ProbeTypeICMP, map[string]string{"service": "c"}),
		makeTarget("conc-mtu", "10.0.0.3", config.ProbeTypeMTU, map[string]string{"service": "d"}),
		makeTarget("conc-dns", "example.com", config.ProbeTypeDNS, map[string]string{"service": "e"}),
	}

	results := []probe.ProbeResult{
		{Success: true, Duration: 10 * time.Millisecond},
		{Success: true, Duration: 100 * time.Millisecond, StatusCode: 200, Phases: map[string]time.Duration{
			"dns_resolve": 5 * time.Millisecond, "tcp_connect": 10 * time.Millisecond,
			"tls_handshake": 15 * time.Millisecond, "ttfb": 20 * time.Millisecond, "transfer": 5 * time.Millisecond,
		}},
		{Success: true, Duration: 50 * time.Millisecond, PacketLoss: 0.0, HopCount: 10},
		{Success: true, Duration: 1 * time.Second, PathMTU: 1472},
		{Success: true, Duration: 12 * time.Millisecond, DNSResolveTime: 10 * time.Millisecond},
	}

	// Run concurrent Record calls and scrapes.
	var wg sync.WaitGroup
	iterations := 100

	// Writers: record probe results concurrently.
	for i := 0; i < len(targets); i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				m.Record(targets[idx], results[idx])
			}
		}(i)
	}

	// Readers: scrape metrics concurrently.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			handler := m.Handler()
			for j := 0; j < iterations; j++ {
				req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Errorf("concurrent scrape returned status %d", rec.Code)
				}
			}
		}()
	}

	// Concurrent Gather calls.
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				_, err := m.registry.Gather()
				if err != nil {
					t.Errorf("concurrent gather failed: %v", err)
				}
			}
		}()
	}

	wg.Wait()

	// After all goroutines complete, metrics should still be gatherable.
	families := gatherMetrics(t, m)
	if _, ok := families["probe_success"]; !ok {
		t.Error("probe_success should exist after concurrent operations")
	}
}

// ---------- buildLabels Tests ----------

func TestBuildLabels_AllTagsPresent(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)

	tags := map[string]string{
		"service":          "api",
		"scope":            "same-region",
		"provider":         "aws",
		"target_region":    "us-east-1",
		"target_partition": "global",
		"visibility":       "internal",
		"port":             "8080",
		"impact":      "high",
	}
	target := makeTarget("bl-test", "host:8080", config.ProbeTypeHTTP, tags)
	labels := m.buildLabels(target)

	if labels["target"] != "host:8080" {
		t.Errorf("target: got %q, want %q", labels["target"], "host:8080")
	}
	if labels["probe_type"] != "http" {
		t.Errorf("probe_type: got %q, want %q", labels["probe_type"], "http")
	}
	for key, want := range tags {
		if labels[key] != want {
			t.Errorf("label %q: got %q, want %q", key, labels[key], want)
		}
	}
}

func TestBuildLabels_DynamicTagKeys(t *testing.T) {
	// Create an exporter with custom tag keys including a non-standard one.
	customKeys := []string{"custom_tag", "service"}
	m := NewMetricsExporter(customKeys)

	tags := map[string]string{
		"service":    "svc",
		"custom_tag": "hello",
	}
	target := makeTarget("bl-dynamic", "host:80", config.ProbeTypeTCP, tags)
	labels := m.buildLabels(target)

	// Both custom_tag and service should be present.
	if labels["custom_tag"] != "hello" {
		t.Errorf("custom_tag: got %q, want %q", labels["custom_tag"], "hello")
	}
	if labels["service"] != "svc" {
		t.Errorf("service: got %q, want %q", labels["service"], "svc")
	}

	// Expected label count: target + target_name + probe_type + proxied + 2 tag keys = 6.
	if len(labels) != 6 {
		t.Errorf("expected 6 labels, got %d", len(labels))
	}
}

func TestDeleteTarget_RemovesAllSeries(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)

	target := makeTarget("del-me", "10.0.0.1", config.ProbeTypeICMP, map[string]string{
		"service": "web",
	})
	result := probe.ProbeResult{
		Success:    true,
		Duration:   42 * time.Millisecond,
		ICMPAvgRTT: 5 * time.Millisecond,
		PacketLoss: 0,
		HopCount:   12,
	}
	m.Record(target, result)

	// Verify series exists before delete.
	families := gatherMetrics(t, m)
	if fam, ok := families["probe_success"]; !ok || len(fam.GetMetric()) == 0 {
		t.Fatal("expected probe_success series before delete")
	}
	if fam, ok := families["probe_icmp_avg_rtt_seconds"]; !ok || len(fam.GetMetric()) == 0 {
		t.Fatal("expected probe_icmp_avg_rtt_seconds series before delete")
	}

	m.DeleteTarget(target)

	// Verify series is gone after delete.
	families = gatherMetrics(t, m)
	if fam, ok := families["probe_success"]; ok {
		for _, metric := range fam.GetMetric() {
			if labelValue(metric, "target_name") == "del-me" {
				t.Error("probe_success series for del-me still present after DeleteTarget")
			}
		}
	}
	if fam, ok := families["probe_duration_seconds"]; ok {
		for _, metric := range fam.GetMetric() {
			if labelValue(metric, "target_name") == "del-me" {
				t.Error("probe_duration_seconds series for del-me still present after DeleteTarget")
			}
		}
	}
	if fam, ok := families["probe_icmp_avg_rtt_seconds"]; ok {
		for _, metric := range fam.GetMetric() {
			if labelValue(metric, "target_name") == "del-me" {
				t.Error("probe_icmp_avg_rtt_seconds series for del-me still present after DeleteTarget")
			}
		}
	}
}

func TestDeleteTarget_RemovesMTUSeries(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)

	target := makeTarget("mtu-del", "10.0.0.1", config.ProbeTypeMTU, map[string]string{
		"service": "net",
	})
	target.ProbeOpts.ExpectedMinMTU = 1500
	m.Record(target, probe.ProbeResult{
		Success:            true,
		Duration:           42 * time.Millisecond,
		PathMTU:            1500,
		MTUFragNeededCount: 1,
		MTUTimeoutCount:    1,
		MTURetryCount:      1,
		MTULocalErrorCount: 1,
	})

	families := gatherMetrics(t, m)
	mtuMetricNames := []string{
		"probe_mtu_path_bytes",
		"probe_mtu_bytes",
		"probe_mtu_state",
		"probe_mtu_frag_needed_total",
		"probe_mtu_timeouts_total",
		"probe_mtu_retries_total",
		"probe_mtu_local_errors_total",
	}
	for _, name := range mtuMetricNames {
		if fam, ok := families[name]; !ok || len(fam.GetMetric()) == 0 {
			t.Fatalf("expected %s series before delete", name)
		}
	}

	m.DeleteTarget(target)

	families = gatherMetrics(t, m)
	for _, name := range mtuMetricNames {
		if fam, ok := families[name]; ok {
			for _, metric := range fam.GetMetric() {
				if labelValue(metric, "target_name") == "mtu-del" {
					t.Errorf("%s series for mtu-del still present after DeleteTarget", name)
				}
			}
		}
	}
}

func TestIncrSkippedOverlap(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)
	target := makeTarget("slow-mtu", "10.0.0.1", config.ProbeTypeMTU, nil)

	m.IncrSkippedOverlap(target)
	m.IncrSkippedOverlap(target)

	families := gatherMetrics(t, m)
	fam, ok := families["probe_skipped_overlap_total"]
	if !ok || len(fam.GetMetric()) == 0 {
		t.Fatal("expected probe_skipped_overlap_total to be present")
	}
	got := fam.GetMetric()[0].GetCounter().GetValue()
	if got != 2 {
		t.Errorf("probe_skipped_overlap_total = %f, want 2", got)
	}

	m.DeleteTarget(target)
	families = gatherMetrics(t, m)
	if fam, ok := families["probe_skipped_overlap_total"]; ok {
		for _, metric := range fam.GetMetric() {
			if labelValue(metric, "target_name") == "slow-mtu" {
				t.Error("probe_skipped_overlap_total series for slow-mtu still present after DeleteTarget")
			}
		}
	}
}

func TestDeleteTarget_HTTPPhasesRemoved(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)

	target := makeTarget("http-del", "https://example.com", config.ProbeTypeHTTP, map[string]string{
		"service": "api",
	})
	result := probe.ProbeResult{
		Success:    true,
		Duration:   100 * time.Millisecond,
		StatusCode: 200,
		Phases: map[string]time.Duration{
			"dns_resolve":   10 * time.Millisecond,
			"tcp_connect":   20 * time.Millisecond,
			"tls_handshake": 30 * time.Millisecond,
			"ttfb":          25 * time.Millisecond,
			"transfer":      15 * time.Millisecond,
		},
	}
	m.Record(target, result)

	m.DeleteTarget(target)

	families := gatherMetrics(t, m)
	if fam, ok := families["probe_phase_duration_seconds"]; ok {
		for _, metric := range fam.GetMetric() {
			if labelValue(metric, "target_name") == "http-del" {
				t.Errorf("phase series for http-del still present: phase=%s", labelValue(metric, "phase"))
			}
		}
	}
}

func TestDeleteTarget_ProxyPhasesRemoved(t *testing.T) {
	m := NewMetricsExporter(testTagKeys)

	target := makeTarget("proxy-del", "https://example.com", config.ProbeTypeProxy, map[string]string{
		"service": "proxy",
	})
	result := probe.ProbeResult{
		Success:  true,
		Duration: 50 * time.Millisecond,
		Phases: map[string]time.Duration{
			"proxy_dial":    10 * time.Millisecond,
			"proxy_tls":     15 * time.Millisecond,
			"proxy_connect": 20 * time.Millisecond,
		},
	}
	m.Record(target, result)

	m.DeleteTarget(target)

	families := gatherMetrics(t, m)
	if fam, ok := families["probe_phase_duration_seconds"]; ok {
		for _, metric := range fam.GetMetric() {
			if labelValue(metric, "target_name") == "proxy-del" {
				t.Errorf("phase series for proxy-del still present: phase=%s", labelValue(metric, "phase"))
			}
		}
	}
}
