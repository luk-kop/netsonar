package probe

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"netsonar/internal/config"
)

// forwardingProxy accepts inbound TCP, reads an HTTP request in proxy form
// (absolute URI), forwards it to the requested URL, and pipes the response
// back. This exercises the http://target via http://proxy path.
//
// The proxy also accepts CONNECT requests for https://target paths, creates
// a raw TCP tunnel to the requested destination, and blindly proxies bytes.
// This is sufficient for an HTTPS test backend running on the same test
// host. The optional connectDelay is inserted between "receive CONNECT" and
// "send 200 Connection Established" so tests can verify that CONNECT
// latency is attributed to proxy_connect rather than tcp_connect.
func forwardingProxy(t *testing.T, connectDelay time.Duration) (string, func()) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start proxy listener: %v", err)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleForwardingProxyConn(conn, connectDelay)
		}
	}()

	return ln.Addr().String(), func() { _ = ln.Close() }
}

func handleForwardingProxyConn(c net.Conn, connectDelay time.Duration) {
	defer func() { _ = c.Close() }()

	reader := bufio.NewReader(c)
	req, err := http.ReadRequest(reader)
	if err != nil {
		return
	}

	if req.Method == http.MethodConnect {
		handleConnectTunnel(c, req, connectDelay)
		return
	}

	// Forward the request as-is to the absolute URL.
	outReq, err := http.NewRequest(req.Method, req.URL.String(), req.Body)
	if err != nil {
		writeErrorResponse(c, http.StatusBadRequest)
		return
	}
	for k, v := range req.Header {
		if strings.EqualFold(k, "Proxy-Authorization") || strings.EqualFold(k, "Proxy-Connection") {
			continue
		}
		for _, vv := range v {
			outReq.Header.Add(k, vv)
		}
	}

	// Do NOT follow redirects inside the proxy — the prober under test
	// is responsible for that and expects to see every 3xx response.
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(outReq)
	if err != nil {
		writeErrorResponse(c, http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if err := resp.Write(c); err != nil {
		return
	}
}

func handleConnectTunnel(c net.Conn, req *http.Request, connectDelay time.Duration) {
	target := req.URL.Host
	if target == "" {
		target = req.Host
	}
	if connectDelay > 0 {
		time.Sleep(connectDelay)
	}
	upstream, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		writeErrorResponse(c, http.StatusBadGateway)
		return
	}
	defer func() { _ = upstream.Close() }()

	_, _ = c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// Bidirectional pipe until either side closes.
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, c)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(c, upstream)
		done <- struct{}{}
	}()
	<-done
}

func writeErrorResponse(c net.Conn, status int) {
	_, _ = fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\n\r\n", status, http.StatusText(status))
}

// httpsProxyListener starts a TLS-wrapped forwarding proxy. The proxy uses
// the supplied TLS certificate (from httptest.NewTLSServer) so clients
// trusting that certificate can talk to it over TLS. When a handler is
// provided via onRequest, it is called with the received request for
// observation.
func httpsProxyListener(t *testing.T, cfg *tls.Config) (string, func()) {
	t.Helper()

	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatalf("start https proxy listener: %v", err)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleForwardingProxyConn(conn, 0)
		}
	}()

	return ln.Addr().String(), func() { _ = ln.Close() }
}

// TestHTTPProber_ProxyPath_HTTPTarget_PlainProxy verifies that for a plain
// HTTP target through a plain HTTP proxy, the prober emits proxy_dial,
// request_write, ttfb, and transfer; and does NOT emit tcp_connect,
// tls_handshake, proxy_connect, proxy_tls, or dns_resolve.
func TestHTTPProber_ProxyPath_HTTPTarget_PlainProxy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	proxyAddr, cleanup := forwardingProxy(t, 0)
	defer cleanup()

	target := config.TargetConfig{
		Name:      "proxy-http-target-plain-proxy",
		Address:   backend.URL,
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   5 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPProber(false, true, testResolvedProxy("http://"+proxyAddr))
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	assertPhaseSetExact(t, result,
		[]string{"proxy_dial", "request_write", "ttfb", "transfer"},
		[]string{"tcp_connect", "tls_handshake", "proxy_connect", "proxy_tls", "dns_resolve"},
	)
}

// TestHTTPBodyProber_ProxyPath_HTTPTarget_PlainProxy mirrors the above but
// exercises HTTPBodyProber.
func TestHTTPBodyProber_ProxyPath_HTTPTarget_PlainProxy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("healthy"))
	}))
	defer backend.Close()

	proxyAddr, cleanup := forwardingProxy(t, 0)
	defer cleanup()

	target := config.TargetConfig{
		Name:      "proxy-http-body-target-plain-proxy",
		Address:   backend.URL,
		ProbeType: config.ProbeTypeHTTPBody,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{BodyMatchString: "healthy"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPBodyProber(false, true, testResolvedProxy("http://"+proxyAddr), "")
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	if !result.BodyMatch {
		t.Fatal("expected BodyMatch=true")
	}
	assertPhaseSetExact(t, result,
		[]string{"proxy_dial", "request_write", "ttfb", "transfer"},
		[]string{"tcp_connect", "tls_handshake", "proxy_connect", "proxy_tls", "dns_resolve"},
	)
}

// TestHTTPProber_ProxyPath_HTTPSTarget_ConnectDelay verifies that CONNECT
// latency is attributed to proxy_connect, not tcp_connect. An artificial
// 150ms delay is injected before the proxy replies to CONNECT. The prober
// must report proxy_connect >= delay and proxy_dial << delay.
func TestHTTPProber_ProxyPath_HTTPSTarget_ConnectDelay(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	const connectDelay = 150 * time.Millisecond
	const tolerance = 40 * time.Millisecond

	proxyAddr, cleanup := forwardingProxy(t, connectDelay)
	defer cleanup()

	target := config.TargetConfig{
		Name:      "proxy-https-target-connect-delay",
		Address:   backend.URL,
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{TLSSkipVerify: true},
	}
	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPProber(true, true, testResolvedProxy("http://"+proxyAddr))
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	assertPhaseSetExact(t, result,
		[]string{"proxy_dial", "proxy_connect", "tls_handshake", "request_write", "ttfb", "transfer"},
		[]string{"tcp_connect", "proxy_tls", "dns_resolve"},
	)

	if got := result.Phases[PhaseProxyConnect]; got < connectDelay-tolerance {
		t.Fatalf("expected proxy_connect >= %v, got %v", connectDelay-tolerance, got)
	}
	if got := result.Phases[PhaseProxyDial]; got > connectDelay/2 {
		t.Fatalf("expected proxy_dial to be short (<%v), got %v", connectDelay/2, got)
	}
}

// TestHTTPBodyProber_ProxyPath_HTTPSTarget_ConnectDelay mirrors the HTTP
// prober test.
func TestHTTPBodyProber_ProxyPath_HTTPSTarget_ConnectDelay(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("healthy"))
	}))
	defer backend.Close()

	const connectDelay = 150 * time.Millisecond
	const tolerance = 40 * time.Millisecond

	proxyAddr, cleanup := forwardingProxy(t, connectDelay)
	defer cleanup()

	target := config.TargetConfig{
		Name:      "proxy-https-body-connect-delay",
		Address:   backend.URL,
		ProbeType: config.ProbeTypeHTTPBody,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			BodyMatchString: "healthy",
			TLSSkipVerify:   true,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPBodyProber(true, true, testResolvedProxy("http://"+proxyAddr), "")
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	assertPhaseSetExact(t, result,
		[]string{"proxy_dial", "proxy_connect", "tls_handshake", "request_write", "ttfb", "transfer"},
		[]string{"tcp_connect", "proxy_tls", "dns_resolve"},
	)

	if got := result.Phases[PhaseProxyConnect]; got < connectDelay-tolerance {
		t.Fatalf("expected proxy_connect >= %v, got %v", connectDelay-tolerance, got)
	}
}

// TestHTTPProber_ProxyPath_HTTPTarget_HTTPSProxy verifies that for a plain
// HTTP target via an HTTPS proxy, proxy_tls is observed and proxy_connect
// is not (forward HTTP does not use CONNECT).
func TestHTTPProber_ProxyPath_HTTPTarget_HTTPSProxy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	// Reuse an httptest TLS server just to get a self-signed certificate to
	// wrap the proxy listener.
	certSrv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer certSrv.Close()
	tlsCfg := certSrv.TLS.Clone()
	tlsCfg.NextProtos = nil

	proxyAddr, cleanup := httpsProxyListener(t, tlsCfg)
	defer cleanup()

	target := config.TargetConfig{
		Name:      "proxy-http-target-https-proxy",
		Address:   backend.URL,
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{TLSSkipVerify: true},
	}
	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPProber(true, true, testResolvedProxyWithTLSSkipVerify("https://"+proxyAddr))
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	assertPhaseSetExact(t, result,
		[]string{"proxy_dial", "proxy_tls", "request_write", "ttfb", "transfer"},
		[]string{"tcp_connect", "tls_handshake", "proxy_connect", "dns_resolve"},
	)
}

func TestHTTPProber_ProxyPath_HTTPTarget_HTTPSProxyRequiresProxyTLSSkipVerify(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	certSrv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer certSrv.Close()
	tlsCfg := certSrv.TLS.Clone()
	tlsCfg.NextProtos = nil

	proxyAddr, cleanup := httpsProxyListener(t, tlsCfg)
	defer cleanup()

	target := config.TargetConfig{
		Name:      "proxy-http-target-https-proxy-requires-proxy-tls-skip",
		Address:   backend.URL,
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{TLSSkipVerify: true},
	}
	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPProber(true, true, testResolvedProxy("https://"+proxyAddr))
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false when HTTPS proxy has untrusted cert and proxy tls_skip_verify is false")
	}
	if !strings.Contains(result.Error, "proxy tls handshake") {
		t.Fatalf("expected proxy TLS handshake error, got %q", result.Error)
	}
	if result.Phases["proxy_tls"] <= 0 {
		t.Fatalf("expected proxy_tls phase on proxy TLS failure, got %v", result.Phases)
	}
}

func TestHTTPBodyProber_ProxyPath_HTTPTarget_HTTPSProxyRequiresProxyTLSSkipVerify(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("healthy"))
	}))
	defer backend.Close()

	certSrv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer certSrv.Close()
	tlsCfg := certSrv.TLS.Clone()
	tlsCfg.NextProtos = nil

	proxyAddr, cleanup := httpsProxyListener(t, tlsCfg)
	defer cleanup()

	target := config.TargetConfig{
		Name:      "body-proxy-http-target-https-proxy-requires-proxy-tls-skip",
		Address:   backend.URL,
		ProbeType: config.ProbeTypeHTTPBody,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			TLSSkipVerify:   true,
			BodyMatchString: "healthy",
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPBodyProber(true, true, testResolvedProxy("https://"+proxyAddr), "")
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false when HTTPS proxy has untrusted cert and proxy tls_skip_verify is false")
	}
	if !strings.Contains(result.Error, "proxy tls handshake") {
		t.Fatalf("expected proxy TLS handshake error, got %q", result.Error)
	}
	if result.Phases["proxy_tls"] <= 0 {
		t.Fatalf("expected proxy_tls phase on proxy TLS failure, got %v", result.Phases)
	}
}

// TestHTTPProber_ProxyPath_RoutesThroughProxy checks that the proxy-aware
// code path actually routes through the proxy rather than talking directly
// to the backend. Confirms the refactor preserved the basic behavior.
func TestHTTPProber_ProxyPath_RoutesThroughProxy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	// Use a plain mockProxy that just returns 200 for CONNECT without
	// actually tunneling. For plain HTTP targets, the prober forwards the
	// HTTP request to the proxy; we count connections as a proxy-routing
	// signal.
	var proxyHits atomic.Int64
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			proxyHits.Add(1)
			go handleForwardingProxyConn(conn, 0)
		}
	}()

	target := config.TargetConfig{
		Name:      "proxy-routes-through",
		Address:   backend.URL,
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   5 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPProber(false, true, testResolvedProxy("http://"+ln.Addr().String()))
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	if proxyHits.Load() == 0 {
		t.Fatal("expected proxy to receive at least one connection")
	}
}

// TestHTTPProber_ProxyPath_SumApproximatesDuration verifies the phase sum
// invariant holds for proxy-path probes: phases are non-overlapping and
// their sum is close to probe duration.
func TestHTTPProber_ProxyPath_SumApproximatesDuration(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	proxyAddr, cleanup := forwardingProxy(t, 0)
	defer cleanup()

	target := config.TargetConfig{
		Name:      "proxy-phase-sum",
		Address:   backend.URL,
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{TLSSkipVerify: true},
	}
	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPProber(true, true, testResolvedProxy("http://"+proxyAddr))
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}

	var sum time.Duration
	for _, d := range result.Phases {
		sum += d
	}
	// Allow 30ms tolerance — test servers add scheduling jitter.
	tolerance := 30 * time.Millisecond
	diff := result.Duration - sum
	if diff < 0 {
		diff = -diff
	}
	if diff > tolerance {
		t.Fatalf("phase sum %v differs from duration %v by %v (> %v tolerance); phases=%v",
			sum, result.Duration, diff, tolerance, result.Phases)
	}
}

// assertPhaseSetExact asserts the result's phase map contains exactly the
// required phase keys and none of the forbidden ones.
func assertPhaseSetExact(t *testing.T, result ProbeResult, required, forbidden []string) {
	t.Helper()

	if result.Phases == nil {
		t.Fatal("expected Phases to be non-nil")
	}
	for _, phase := range required {
		if _, ok := result.Phases[phase]; !ok {
			t.Fatalf("expected Phases to contain %q, have %v", phase, result.Phases)
		}
	}
	for _, phase := range forbidden {
		if _, ok := result.Phases[phase]; ok {
			t.Fatalf("did NOT expect Phases to contain %q, have %v", phase, result.Phases)
		}
	}
}

// TestHTTPProber_ProxyPath_HTTPSTarget_TLSCertExtraction verifies that
// the target TLS peer certificate chain is exposed through resp.TLS on
// the proxy path, so tls_emit_cert_metrics continues to work for
// https:// targets routed through a proxy. This is the regression
// guard for the bug where the manual proxy-path TLS handshake left
// resp.TLS nil and setTLSCertificateResult silently skipped.
func TestHTTPProber_ProxyPath_HTTPSTarget_TLSCertExtraction(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	proxyAddr, cleanup := forwardingProxy(t, 0)
	defer cleanup()

	target := config.TargetConfig{
		Name:      "proxy-https-cert-extract",
		Address:   backend.URL,
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			TLSSkipVerify:      true,
			TLSEmitCertMetrics: true,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPProber(true, true, testResolvedProxy("http://"+proxyAddr))
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	if !result.CertObserved {
		t.Fatal("expected CertObserved=true on HTTPS proxy-path probe")
	}
	if result.CertExpiry.IsZero() {
		t.Fatal("expected non-zero CertExpiry on HTTPS proxy-path probe")
	}
	if len(result.TLSCertificates) == 0 {
		t.Fatal("expected TLSCertificates to be populated on HTTPS proxy-path probe")
	}
}
