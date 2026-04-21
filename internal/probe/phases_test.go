package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
	"time"

	"netsonar/internal/config"
)

// TestAllPhasesCoversEmittedPhases runs the probers that emit phase timings
// against local mocks and asserts that every phase key they set is declared
// in AllPhases. This closes the drift gap where a prober could emit a new
// phase label without the metrics package learning to delete stale series
// for it.
func TestAllPhasesCoversEmittedPhases(t *testing.T) {
	emitted := make(map[string]struct{})

	// HTTPS server — exercises HTTPProber's full phase set including
	// tls_handshake.
	httpsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer httpsSrv.Close()

	httpsTarget := config.TargetConfig{
		Name:      "phases-http",
		Address:   localHostURL(t, httpsSrv.URL),
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   5 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), httpsTarget.Timeout)
	defer cancel()

	httpResult := NewHTTPProber(true, true, "").Probe(ctx, httpsTarget)
	if !httpResult.Success {
		t.Fatalf("HTTPS probe failed: %s", httpResult.Error)
	}
	for phase := range httpResult.Phases {
		emitted[phase] = struct{}{}
	}

	// HTTP CONNECT proxy — exercises ProxyProber's proxy_dial and
	// proxy_connect (proxy_tls is HTTPS-proxy-only and is covered by
	// TestProxyProber_PreservesPhasesOnHTTPSProxyTLSFailure; we add it here
	// manually to keep this test self-contained without extra TLS plumbing).
	proxyAddr, cleanup := mockProxy(t, http.StatusOK)
	defer cleanup()

	proxyTarget := config.TargetConfig{
		Name:      "phases-proxy",
		Address:   "https://example.com",
		ProbeType: config.ProbeTypeProxy,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{ProxyURL: "http://" + proxyAddr},
	}
	proxyCtx, proxyCancel := context.WithTimeout(context.Background(), proxyTarget.Timeout)
	defer proxyCancel()

	proxyResult := (&ProxyProber{}).Probe(proxyCtx, proxyTarget)
	if !proxyResult.Success {
		t.Fatalf("proxy probe failed: %s", proxyResult.Error)
	}
	for phase := range proxyResult.Phases {
		emitted[phase] = struct{}{}
	}
	// proxy_tls only fires for HTTPS proxies; assert AllPhases still covers
	// it via direct lookup so the parity check below remains meaningful.
	emitted[PhaseProxyTLS] = struct{}{}

	// Every emitted phase must be declared in AllPhases.
	for phase := range emitted {
		if !slices.Contains(AllPhases, phase) {
			t.Errorf("prober emitted phase %q not listed in AllPhases", phase)
		}
	}

	// And every AllPhases entry must be observed — otherwise it's dead
	// config the metrics layer allocates delete-labels for pointlessly.
	for _, phase := range AllPhases {
		if _, ok := emitted[phase]; !ok {
			t.Errorf("AllPhases entry %q was not observed in any prober output", phase)
		}
	}
}

func localHostURL(t *testing.T, raw string) string {
	t.Helper()

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split host/port for %q: %v", u.Host, err)
	}
	u.Host = net.JoinHostPort("localhost", port)
	return u.String()
}
