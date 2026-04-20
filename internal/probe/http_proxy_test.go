package probe

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"netsonar/internal/config"
)

// TestHTTPProber_ProxyRouting verifies that when proxy_url is configured,
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

	prober := NewHTTPProber(false, true, proxy.URL)
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	if !proxyHit.Load() {
		t.Fatal("expected request to be routed through proxy, but proxy was not hit")
	}
}

// TestHTTPProber_NoProxyDirect verifies that when proxy_url is empty,
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

	prober := NewHTTPProber(false, true, "")
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	if !directHit.Load() {
		t.Fatal("expected direct connection to server")
	}
}

// TestHTTPProber_ProxyUnreachable verifies that when proxy_url points to
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
	prober := NewHTTPProber(false, true, "http://127.0.0.1:19999")
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false when proxy is unreachable")
	}
	if result.Error == "" {
		t.Fatal("expected non-empty Error when proxy is unreachable")
	}
}

func TestNewHTTPProber_InvalidProxyURLPanics(t *testing.T) {
	assertPanicsWith(t, "NewHTTPProber", func() {
		NewHTTPProber(false, true, "ftp://proxy.internal:21")
	})
}

// TestHTTPBodyProber_ProxyRouting verifies that when proxy_url is configured,
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

	prober := NewHTTPBodyProber(false, true, proxy.URL, "")
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

// TestHTTPBodyProber_ProxyUnreachable verifies that when proxy_url points
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

	prober := NewHTTPBodyProber(false, true, "http://127.0.0.1:19999", "")
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false when proxy is unreachable")
	}
	if result.Error == "" {
		t.Fatal("expected non-empty Error when proxy is unreachable")
	}
}

func TestNewHTTPBodyProber_InvalidProxyURLPanics(t *testing.T) {
	assertPanicsWith(t, "NewHTTPBodyProber", func() {
		NewHTTPBodyProber(false, true, "http://proxy.internal:8888/path", "")
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
