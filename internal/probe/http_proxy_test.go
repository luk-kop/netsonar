package probe

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"netsonar/internal/config"
)

func setEnvProxyTrap(t *testing.T, proxyURL string) {
	t.Helper()

	for _, env := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(env, proxyURL)
	}
	for _, env := range []string{"NO_PROXY", "no_proxy"} {
		t.Setenv(env, "*")
	}
}

func newRecordingForwardProxy(t *testing.T, responseFallbackBody string) (*httptest.Server, *atomic.Bool) {
	t.Helper()

	var hit atomic.Bool
	transport := &http.Transport{Proxy: nil}
	t.Cleanup(func() { transport.CloseIdleConnections() })

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)

		outReq, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for k, values := range r.Header {
			if strings.EqualFold(k, "Proxy-Authorization") || strings.EqualFold(k, "Proxy-Connection") {
				continue
			}
			for _, value := range values {
				outReq.Header.Add(k, value)
			}
		}

		resp, err := transport.RoundTrip(outReq)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			if responseFallbackBody != "" {
				_, _ = w.Write([]byte(responseFallbackBody))
			}
			return
		}
		defer func() { _ = resp.Body.Close() }()

		for k, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(k, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		if _, err := io.Copy(w, resp.Body); err != nil && responseFallbackBody != "" {
			_, _ = w.Write([]byte(responseFallbackBody))
		}
	}))
	t.Cleanup(proxy.Close)

	return proxy, &hit
}

// TestHTTPProber_ProxyRouting verifies that when proxy_name is configured,
// the HTTP prober routes requests through the proxy. We spin up a fake
// proxy server that records whether it received the request.
func TestHTTPProber_ProxyRouting(t *testing.T) {
	// Backend server — the actual target.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("backend"))
	}))
	defer backend.Close()

	// Proxy server — forwards to backend and records the hit.
	var proxyHit atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHit.Store(true)
		// Forward the request to the backend.
		resp, err := http.Get(r.URL.String())
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		w.WriteHeader(resp.StatusCode)
	}))
	defer proxy.Close()

	target := config.TargetConfig{
		Name:      "test-http-proxy",
		Address:   backend.URL,
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   5 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPProber(false, true, testResolvedProxy(proxy.URL))
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	if !proxyHit.Load() {
		t.Fatal("expected request to be routed through proxy, but proxy was not hit")
	}
}

// TestHTTPProber_NoProxyDirect verifies that when proxy_name is empty,
// the HTTP prober connects directly to the target (no proxy).
func TestHTTPProber_NoProxyDirect(t *testing.T) {
	var directHit atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		directHit.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := config.TargetConfig{
		Name:      "test-http-no-proxy",
		Address:   srv.URL,
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   5 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPProber(false, true, nil)
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	if !directHit.Load() {
		t.Fatal("expected direct connection to server")
	}
}

func TestHTTPProber_DirectIgnoresEnvironmentProxy(t *testing.T) {
	var directHit atomic.Bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		directHit.Store(true)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("backend"))
	}))
	defer backend.Close()

	envProxy, envProxyHit := newRecordingForwardProxy(t, "env-proxy")
	setEnvProxyTrap(t, envProxy.URL)

	target := config.TargetConfig{
		Name:      "direct-ignores-env-proxy",
		Address:   backend.URL,
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   5 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	result := NewHTTPProber(false, true, nil).Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	if !directHit.Load() {
		t.Fatal("expected direct backend to be hit")
	}
	if envProxyHit.Load() {
		t.Fatal("direct HTTP probe used environment proxy")
	}
}

func TestHTTPBodyProber_DirectIgnoresEnvironmentProxy(t *testing.T) {
	var directHit atomic.Bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		directHit.Store(true)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("healthy"))
	}))
	defer backend.Close()

	envProxy, envProxyHit := newRecordingForwardProxy(t, "env-proxy")
	setEnvProxyTrap(t, envProxy.URL)

	target := config.TargetConfig{
		Name:      "body-direct-ignores-env-proxy",
		Address:   backend.URL,
		ProbeType: config.ProbeTypeHTTPBody,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{BodyMatchString: "healthy"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	result := NewHTTPBodyProber(false, true, nil, "").Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	if !result.BodyMatch {
		t.Fatal("expected BodyMatch=true")
	}
	if !directHit.Load() {
		t.Fatal("expected direct backend to be hit")
	}
	if envProxyHit.Load() {
		t.Fatal("direct HTTP body probe used environment proxy")
	}
}

func TestHTTPProber_ConfiguredProxyWinsOverEnvironmentProxy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("backend"))
	}))
	defer backend.Close()

	envProxy, envProxyHit := newRecordingForwardProxy(t, "env-proxy")
	configuredProxy, configuredProxyHit := newRecordingForwardProxy(t, "configured-proxy")
	setEnvProxyTrap(t, envProxy.URL)

	target := config.TargetConfig{
		Name:      "configured-proxy-wins",
		Address:   backend.URL,
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   5 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	result := NewHTTPProber(false, true, testResolvedProxy(configuredProxy.URL)).Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	if !configuredProxyHit.Load() {
		t.Fatal("expected configured proxy to be hit")
	}
	if envProxyHit.Load() {
		t.Fatal("configured proxy probe used environment proxy")
	}
}

func TestHTTPBodyProber_ConfiguredProxyWinsOverEnvironmentProxy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("healthy"))
	}))
	defer backend.Close()

	envProxy, envProxyHit := newRecordingForwardProxy(t, "env-proxy")
	configuredProxy, configuredProxyHit := newRecordingForwardProxy(t, "configured-proxy")
	setEnvProxyTrap(t, envProxy.URL)

	target := config.TargetConfig{
		Name:      "body-configured-proxy-wins",
		Address:   backend.URL,
		ProbeType: config.ProbeTypeHTTPBody,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{BodyMatchString: "healthy"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	result := NewHTTPBodyProber(false, true, testResolvedProxy(configuredProxy.URL), "").Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	if !result.BodyMatch {
		t.Fatal("expected BodyMatch=true")
	}
	if !configuredProxyHit.Load() {
		t.Fatal("expected configured proxy to be hit")
	}
	if envProxyHit.Load() {
		t.Fatal("configured HTTP body proxy probe used environment proxy")
	}
}

// TestHTTPProber_ProxyUnreachable verifies that when proxy_name points to
// an unreachable proxy, the probe fails with an error.
func TestHTTPProber_ProxyUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := config.TargetConfig{
		Name:      "test-http-proxy-unreachable",
		Address:   srv.URL,
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   2 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	// Point to a port that is not listening.
	prober := NewHTTPProber(false, true, testResolvedProxy("http://127.0.0.1:19999"))
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false when proxy is unreachable")
	}
	if result.Error == "" {
		t.Fatal("expected non-empty Error when proxy is unreachable")
	}
}

func TestNewHTTPProber_InvalidProxyEndpointPanics(t *testing.T) {
	assertPanicsWith(t, "NewHTTPProber", func() {
		NewHTTPProber(false, true, testResolvedProxy("ftp://proxy.internal:21"))
	})
}

// TestHTTPBodyProber_ProxyRouting verifies that when proxy_name is configured,
// the HTTP body prober routes requests through the proxy.
func TestHTTPBodyProber_ProxyRouting(t *testing.T) {
	// Backend server.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("healthy"))
	}))
	defer backend.Close()

	// Proxy server.
	var proxyHit atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHit.Store(true)
		resp, err := http.Get(r.URL.String())
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		w.WriteHeader(resp.StatusCode)
		// Forward body for body match validation.
		_, _ = w.Write([]byte("healthy"))
	}))
	defer proxy.Close()

	target := config.TargetConfig{
		Name:      "test-http-body-proxy",
		Address:   backend.URL,
		ProbeType: config.ProbeTypeHTTPBody,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			BodyMatchString: "healthy",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPBodyProber(false, true, testResolvedProxy(proxy.URL), "")
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	if !proxyHit.Load() {
		t.Fatal("expected request to be routed through proxy, but proxy was not hit")
	}
	if !result.BodyMatch {
		t.Fatal("expected BodyMatch=true when body contains 'healthy'")
	}
}

// TestHTTPBodyProber_ProxyUnreachable verifies that when proxy_name points
// to an unreachable proxy, the HTTP body probe fails.
func TestHTTPBodyProber_ProxyUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	target := config.TargetConfig{
		Name:      "test-http-body-proxy-unreachable",
		Address:   srv.URL,
		ProbeType: config.ProbeTypeHTTPBody,
		Timeout:   2 * time.Second,
		ProbeOpts: config.ProbeOptions{
			BodyMatchString: "ok",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPBodyProber(false, true, testResolvedProxy("http://127.0.0.1:19999"), "")
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false when proxy is unreachable")
	}
	if result.Error == "" {
		t.Fatal("expected non-empty Error when proxy is unreachable")
	}
}

func TestNewHTTPBodyProber_InvalidProxyEndpointPanics(t *testing.T) {
	assertPanicsWith(t, "NewHTTPBodyProber", func() {
		NewHTTPBodyProber(false, true, testResolvedProxy("http://proxy.internal:8888/path"), "")
	})
}

func assertPanicsWith(t *testing.T, want string, fn func()) {
	t.Helper()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic containing %q", want)
		}
		got := fmt.Sprint(r)
		if !strings.Contains(got, want) {
			t.Fatalf("panic = %q, want it to contain %q", got, want)
		}
	}()

	fn()
}
