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

// TestHTTPProber_ProxyPath_RedirectsDisabled verifies that when
// follow_redirects is false on the proxy path, a 302 response is the
// final result: StatusCode is the 302, Success depends on
// expected_status_codes, and the /final backend handler is not invoked.
func TestHTTPProber_ProxyPath_RedirectsDisabled(t *testing.T) {
	var finalHits atomic.Int64
	var backend *httptest.Server
	backend = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, backend.URL+"/final", http.StatusFound)
		case "/final":
			finalHits.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("final"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxyAddr, cleanup := forwardingProxy(t, 0)
	defer cleanup()

	target := config.TargetConfig{
		Name:      "proxy-redirect-disabled",
		Address:   backend.URL + "/redirect",
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ExpectedStatusCodes: []int{http.StatusFound},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPProber(false, false, testResolvedProxy("http://"+proxyAddr))
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true (302 in expected list), got false; error: %s", result.Error)
	}
	if result.StatusCode != http.StatusFound {
		t.Fatalf("expected StatusCode=%d, got %d", http.StatusFound, result.StatusCode)
	}
	if got := finalHits.Load(); got != 0 {
		t.Fatalf("expected /final to receive 0 hits when follow_redirects=false, got %d", got)
	}
}

// TestHTTPProber_ProxyPath_RedirectsEnabled verifies that when
// follow_redirects is true on the proxy path, the redirect is followed
// and the probe reports the final 200 response.
func TestHTTPProber_ProxyPath_RedirectsEnabled(t *testing.T) {
	var finalHits atomic.Int64
	var backend *httptest.Server
	backend = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, backend.URL+"/final", http.StatusFound)
		case "/final":
			finalHits.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("final"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxyAddr, cleanup := forwardingProxy(t, 0)
	defer cleanup()

	target := config.TargetConfig{
		Name:      "proxy-redirect-enabled",
		Address:   backend.URL + "/redirect",
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ExpectedStatusCodes: []int{http.StatusOK},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPProber(false, true, testResolvedProxy("http://"+proxyAddr))
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true (followed to 200), got false; error: %s", result.Error)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("expected StatusCode=%d, got %d", http.StatusOK, result.StatusCode)
	}
	if got := finalHits.Load(); got != 1 {
		t.Fatalf("expected /final to receive 1 hit, got %d", got)
	}
}

// TestHTTPProber_ProxyPath_RelativeLocationRedirect verifies that a
// relative Location header resolves against the request URL and the
// redirect is followed correctly.
func TestHTTPProber_ProxyPath_RelativeLocationRedirect(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			// Relative path — no scheme/host.
			w.Header().Set("Location", "/dest")
			w.WriteHeader(http.StatusFound)
		case "/dest":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("arrived"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxyAddr, cleanup := forwardingProxy(t, 0)
	defer cleanup()

	target := config.TargetConfig{
		Name:      "proxy-relative-redirect",
		Address:   backend.URL + "/start",
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ExpectedStatusCodes: []int{http.StatusOK},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPProber(false, true, testResolvedProxy("http://"+proxyAddr))
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("expected StatusCode=%d, got %d", http.StatusOK, result.StatusCode)
	}
}

// TestHTTPProber_ProxyPath_TooManyRedirects verifies that a redirect
// chain exceeding proxyRedirectLimit fails with the same policy error as
// Go's default http.Client rather than treating the last 3xx as success.
func TestHTTPProber_ProxyPath_TooManyRedirects(t *testing.T) {
	var hopHits atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hopHits.Add(1)
		// Every path 307s to the next numbered path. The chain is
		// infinite unless the prober stops it.
		next := fmt.Sprintf("%s?n=%d", r.URL.Path, hopHits.Load())
		w.Header().Set("Location", next)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer backend.Close()

	proxyAddr, cleanup := forwardingProxy(t, 0)
	defer cleanup()

	target := config.TargetConfig{
		Name:      "proxy-too-many-redirects",
		Address:   backend.URL + "/loop",
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   5 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPProber(false, true, testResolvedProxy("http://"+proxyAddr))
	result := prober.Probe(ctx, target)

	wantHops := int64(proxyRedirectLimit)
	if got := hopHits.Load(); got != wantHops {
		t.Fatalf("expected backend to see %d hops, got %d", wantHops, got)
	}
	if result.Success {
		t.Fatal("expected Success=false after too many redirects")
	}
	if !strings.Contains(result.Error, "stopped after 10 redirects") {
		t.Fatalf("expected redirect-limit error, got %q", result.Error)
	}
}

// TestHTTPProber_ProxyPath_RedirectPhasesAreFinalHopOnly verifies that
// the reported Phases reflect the final redirect hop only — phases from
// earlier hops are not carried forward or summed. Each hop introduces a
// fresh server delay; the final hop is fastest, so if the reported ttfb
// matches only the final delay within tolerance, we know phases were not
// accumulated.
func TestHTTPProber_ProxyPath_RedirectPhasesAreFinalHopOnly(t *testing.T) {
	const slowDelay = 120 * time.Millisecond
	const fastDelay = 5 * time.Millisecond
	const tolerance = 50 * time.Millisecond

	var backend *httptest.Server
	backend = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slow":
			time.Sleep(slowDelay)
			http.Redirect(w, r, backend.URL+"/fast", http.StatusFound)
		case "/fast":
			time.Sleep(fastDelay)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxyAddr, cleanup := forwardingProxy(t, 0)
	defer cleanup()

	target := config.TargetConfig{
		Name:      "proxy-redirect-phases",
		Address:   backend.URL + "/slow",
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
	// ttfb should reflect only the final hop's delay, not the sum of
	// both delays. If phases were carried over, ttfb would be >= slowDelay.
	ttfb, ok := result.Phases[PhaseTTFB]
	if !ok {
		t.Fatalf("expected ttfb phase, got %v", result.Phases)
	}
	if ttfb >= slowDelay {
		t.Fatalf("ttfb=%v >= slowDelay %v; earlier-hop timing leaked into the final result", ttfb, slowDelay)
	}
	if ttfb > fastDelay+tolerance {
		t.Fatalf("ttfb=%v exceeds fastDelay %v + tolerance %v; final hop not isolated", ttfb, fastDelay, tolerance)
	}

	// Required phases for a plain HTTP target via plain HTTP proxy. The
	// forbidden set documents what must not appear even across redirects.
	assertPhaseSetExact(t, result,
		[]string{"proxy_dial", "request_write", "ttfb", "transfer"},
		[]string{"tcp_connect", "tls_handshake", "proxy_connect", "proxy_tls", "dns_resolve"},
	)
}

// TestHTTPBodyProber_ProxyPath_RedirectsEnabled mirrors the http prober
// test for http_body, including body match against the final response.
func TestHTTPBodyProber_ProxyPath_RedirectsEnabled(t *testing.T) {
	var finalHits atomic.Int64
	var backend *httptest.Server
	backend = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, backend.URL+"/final", http.StatusFound)
		case "/final":
			finalHits.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("healthy-final"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxyAddr, cleanup := forwardingProxy(t, 0)
	defer cleanup()

	target := config.TargetConfig{
		Name:      "proxy-body-redirect-enabled",
		Address:   backend.URL + "/redirect",
		ProbeType: config.ProbeTypeHTTPBody,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			BodyMatchString: "healthy-final",
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPBodyProber(false, true, testResolvedProxy("http://"+proxyAddr), "")
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	if !result.BodyMatch {
		t.Fatal("expected BodyMatch=true against the final response")
	}
	if finalHits.Load() != 1 {
		t.Fatalf("expected /final to receive 1 hit, got %d", finalHits.Load())
	}
}

// TestHTTPBodyProber_ProxyPath_RedirectsDisabled mirrors the http prober
// test: follow_redirects=false on the proxy path treats the 302 as the
// final response and does not reach the /final handler.
func TestHTTPBodyProber_ProxyPath_RedirectsDisabled(t *testing.T) {
	var finalHits atomic.Int64
	var backend *httptest.Server
	backend = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			// 302 with a short body so http_body's evaluation path can
			// still produce a deterministic BodyMatch.
			w.Header().Set("Location", backend.URL+"/final")
			w.WriteHeader(http.StatusFound)
			_, _ = w.Write([]byte("redirect-body"))
		case "/final":
			finalHits.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("final"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxyAddr, cleanup := forwardingProxy(t, 0)
	defer cleanup()

	target := config.TargetConfig{
		Name:      "proxy-body-redirect-disabled",
		Address:   backend.URL + "/redirect",
		ProbeType: config.ProbeTypeHTTPBody,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			BodyMatchString:     "redirect-body",
			ExpectedStatusCodes: []int{http.StatusFound},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPBodyProber(false, false, testResolvedProxy("http://"+proxyAddr), "")
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true against the 302 body, got false; error: %s", result.Error)
	}
	if result.StatusCode != http.StatusFound {
		t.Fatalf("expected StatusCode=%d, got %d", http.StatusFound, result.StatusCode)
	}
	if got := finalHits.Load(); got != 0 {
		t.Fatalf("expected /final to receive 0 hits, got %d", got)
	}
}

// TestHTTPProber_ProxyPath_RedirectMissingLocationIsFinal verifies that a
// 3xx response without a Location header is treated as the final response,
// matching Go's default http.Client behavior.
func TestHTTPProber_ProxyPath_RedirectMissingLocationIsFinal(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 302 with no Location header.
		w.WriteHeader(http.StatusFound)
	}))
	defer backend.Close()

	proxyAddr, cleanup := forwardingProxy(t, 0)
	defer cleanup()

	target := config.TargetConfig{
		Name:      "proxy-redirect-no-location",
		Address:   backend.URL + "/broken",
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   5 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPProber(false, true, testResolvedProxy("http://"+proxyAddr))
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true when empty expected_status_codes accepts final 302, got false; error: %s", result.Error)
	}
	if result.StatusCode != http.StatusFound {
		t.Fatalf("expected final StatusCode=%d, got %d", http.StatusFound, result.StatusCode)
	}
}

// TestHTTPProber_ProxyPath_TemporaryRedirectReplaysPostBody verifies that
// 307/308 redirects preserve method and replay the generated request body,
// matching Go's redirect behavior for replayable bodies.
func TestHTTPProber_ProxyPath_TemporaryRedirectReplaysPostBody(t *testing.T) {
	var finalMethod atomic.Value
	var finalBodyLen atomic.Int64
	var backend *httptest.Server
	backend = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			w.Header().Set("Location", backend.URL+"/final")
			w.WriteHeader(http.StatusTemporaryRedirect)
		case "/final":
			finalMethod.Store(r.Method)
			body, _ := io.ReadAll(r.Body)
			finalBodyLen.Store(int64(len(body)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxyAddr, cleanup := forwardingProxy(t, 0)
	defer cleanup()

	const bodySize int64 = 4096
	target := config.TargetConfig{
		Name:      "proxy-redirect-307-post",
		Address:   backend.URL + "/redirect",
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			Method:           http.MethodPost,
			RequestBodyBytes: bodySize,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPProber(false, true, testResolvedProxy("http://"+proxyAddr))
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	if got, _ := finalMethod.Load().(string); got != http.MethodPost {
		t.Fatalf("expected final method POST after 307, got %q", got)
	}
	if got := finalBodyLen.Load(); got != bodySize {
		t.Fatalf("expected replayed body length %d, got %d", bodySize, got)
	}
}

// TestHTTPProber_ProxyPath_PostToGetThenTemporaryRedirectDoesNotReplayBody
// verifies that once a 302 changes POST to GET, a later 307 preserves GET
// without resurrecting the original generated body.
func TestHTTPProber_ProxyPath_PostToGetThenTemporaryRedirectDoesNotReplayBody(t *testing.T) {
	var finalMethod atomic.Value
	var finalBodyLen atomic.Int64
	var backend *httptest.Server
	backend = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			w.Header().Set("Location", backend.URL+"/middle")
			w.WriteHeader(http.StatusFound)
		case "/middle":
			w.Header().Set("Location", backend.URL+"/final")
			w.WriteHeader(http.StatusTemporaryRedirect)
		case "/final":
			finalMethod.Store(r.Method)
			body, _ := io.ReadAll(r.Body)
			finalBodyLen.Store(int64(len(body)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxyAddr, cleanup := forwardingProxy(t, 0)
	defer cleanup()

	target := config.TargetConfig{
		Name:      "proxy-redirect-post-get-307",
		Address:   backend.URL + "/start",
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			Method:           http.MethodPost,
			RequestBodyBytes: 1024,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPProber(false, true, testResolvedProxy("http://"+proxyAddr))
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	if got, _ := finalMethod.Load().(string); got != http.MethodGet {
		t.Fatalf("expected final method GET after 302 then 307, got %q", got)
	}
	if got := finalBodyLen.Load(); got != 0 {
		t.Fatalf("expected no body after POST became GET, got %d bytes", got)
	}
}
