package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"netsonar/internal/config"
	"netsonar/internal/metrics"
	"netsonar/internal/probe"
)

func TestParseLogLevel(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"", slog.LevelInfo},
		{"unknown", slog.LevelInfo},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := parseLogLevel(tc.in); got != tc.want {
				t.Errorf("parseLogLevel(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestConfigureLogger_UpdatesLevelVarAtomically exercises configureLogger
// directly through agentLogLevel.Level() without touching slog.Default(),
// so the test does not leak global logger state to other tests.
func TestConfigureLogger_UpdatesLevelVarAtomically(t *testing.T) {
	original := agentLogLevel.Level()
	t.Cleanup(func() { agentLogLevel.Set(original) })

	cases := []struct {
		level string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"debug", slog.LevelDebug},
	}
	for _, tc := range cases {
		configureLogger(tc.level)
		if got := agentLogLevel.Level(); got != tc.want {
			t.Errorf("after configureLogger(%q): agentLogLevel.Level() = %v, want %v", tc.level, got, tc.want)
		}
	}
}

// TestSetupLogger_WiresLevelVarIntoDefaultHandler verifies the one-shot
// startup path: handler is created once, bound to agentLogLevel, and the
// default logger honors subsequent level changes without re-setup.
func TestSetupLogger_WiresLevelVarIntoDefaultHandler(t *testing.T) {
	originalDefault := slog.Default()
	originalLevel := agentLogLevel.Level()
	t.Cleanup(func() {
		slog.SetDefault(originalDefault)
		agentLogLevel.Set(originalLevel)
	})

	ctx := context.Background()

	setupLogger("warn")
	if got := agentLogLevel.Level(); got != slog.LevelWarn {
		t.Fatalf("after setupLogger(warn): level = %v, want warn", got)
	}

	// Change level through configureLogger (the reload path). The default
	// handler installed by setupLogger must observe the new level without
	// any further handler swap.
	configureLogger("debug")
	if !slog.Default().Enabled(ctx, slog.LevelDebug) {
		t.Errorf("default logger should be enabled for LevelDebug after configureLogger(debug)")
	}
	if slog.Default().Enabled(ctx, slog.LevelDebug-1) {
		t.Errorf("default logger should not be enabled below LevelDebug")
	}

	configureLogger("error")
	if slog.Default().Enabled(ctx, slog.LevelWarn) {
		t.Errorf("default logger should not be enabled for LevelWarn after configureLogger(error)")
	}
	if !slog.Default().Enabled(ctx, slog.LevelError) {
		t.Errorf("default logger should be enabled for LevelError after configureLogger(error)")
	}
}

func TestNewHTTPServer_HasTimeouts(t *testing.T) {
	exporter := metrics.NewMetricsExporter(nil)
	srv := newHTTPServer(":9275", "/metrics", exporter)

	if srv.ReadHeaderTimeout != 5*time.Second {
		t.Errorf("ReadHeaderTimeout = %s, want 5s", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout != 10*time.Second {
		t.Errorf("ReadTimeout = %s, want 10s", srv.ReadTimeout)
	}
	if srv.WriteTimeout != 10*time.Second {
		t.Errorf("WriteTimeout = %s, want 10s", srv.WriteTimeout)
	}
	if srv.IdleTimeout != 60*time.Second {
		t.Errorf("IdleTimeout = %s, want 60s", srv.IdleTimeout)
	}
}

func TestNewHTTPMux_Healthz(t *testing.T) {
	exporter := metrics.NewMetricsExporter(nil)
	handler := newHTTPMux("/metrics", exporter)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.String() != "ok\n" {
		t.Fatalf("body = %q, want ok newline", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", ct)
	}
}

func TestNewHTTPMux_Readyz(t *testing.T) {
	exporter := metrics.NewMetricsExporter(nil)
	handler := newHTTPMux("/metrics", exporter)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.String() != "ok\n" {
		t.Fatalf("body = %q, want ok newline", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", ct)
	}
}

func TestNewProber_CoversAllValidProbeTypes(t *testing.T) {
	expected := map[config.ProbeType]reflect.Type{
		config.ProbeTypeTCP:      reflect.TypeOf(&probe.TCPProber{}),
		config.ProbeTypeHTTP:     reflect.TypeOf(&probe.HTTPProber{}),
		config.ProbeTypeICMP:     reflect.TypeOf(&probe.ICMPProber{}),
		config.ProbeTypeMTU:      reflect.TypeOf(&probe.MTUProber{}),
		config.ProbeTypeDNS:      reflect.TypeOf(&probe.DNSProber{}),
		config.ProbeTypeTLSCert:  reflect.TypeOf(&probe.TLSCertProber{}),
		config.ProbeTypeHTTPBody: reflect.TypeOf(&probe.HTTPBodyProber{}),
		config.ProbeTypeProxy:    reflect.TypeOf(&probe.ProxyProber{}),
	}

	if len(expected) != len(config.ValidProbeTypes) {
		t.Fatalf("expected table has %d entries, ValidProbeTypes has %d", len(expected), len(config.ValidProbeTypes))
	}

	for probeType := range config.ValidProbeTypes {
		t.Run(string(probeType), func(t *testing.T) {
			target := config.TargetConfig{
				Name:      "test",
				Address:   "example.com:443",
				ProbeType: probeType,
			}

			gotType := reflect.TypeOf(newProber(target))
			wantType, ok := expected[probeType]
			if !ok {
				t.Fatalf("missing expected prober type for %q", probeType)
			}
			if gotType != wantType {
				t.Fatalf("newProber(%q) returned %v, want %v", probeType, gotType, wantType)
			}
		})
	}
}
