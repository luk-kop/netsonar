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

	// Direct TCP listener — exercises TCPProber's tcp_connect phase.
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start TCP listener: %v", err)
	}
	defer func() { _ = tcpLn.Close() }()
	go func() {
		for {
			conn, err := tcpLn.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	tcpTarget := config.TargetConfig{
		Name:      "phases-tcp",
		Address:   tcpLn.Addr().String(),
		ProbeType: config.ProbeTypeTCP,
		Timeout:   5 * time.Second,
	}
	tcpCtx, tcpCancel := context.WithTimeout(context.Background(), tcpTarget.Timeout)
	defer tcpCancel()

	tcpResult := (&TCPProber{}).Probe(tcpCtx, tcpTarget)
	if !tcpResult.Success {
		t.Fatalf("TCP probe failed: %s", tcpResult.Error)
	}
	for phase := range tcpResult.Phases {
		emitted[phase] = struct{}{}
	}

	// Direct TLS certificate probe — exercises tls_cert phase emission for
	// tcp_connect and tls_handshake outside the HTTP path.
	tlsTarget := config.TargetConfig{
		Name:      "phases-tls-cert",
		Address:   httpsSrv.Listener.Addr().String(),
		ProbeType: config.ProbeTypeTLSCert,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			TLSSkipVerify: true,
		},
	}
	tlsCtx, tlsCancel := context.WithTimeout(context.Background(), tlsTarget.Timeout)
	defer tlsCancel()

	tlsResult := (&TLSCertProber{}).Probe(tlsCtx, tlsTarget)
	if !tlsResult.Success {
		t.Fatalf("direct TLS cert probe failed: %s", tlsResult.Error)
	}
	for phase := range tlsResult.Phases {
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
		Address:   "example.com:443",
		ProbeType: config.ProbeTypeProxyConnect,
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
