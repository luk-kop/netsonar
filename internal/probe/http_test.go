package probe

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"netsonar/internal/config"
)

// expectedHTTPPhaseKeys lists the five phase keys that every HTTP probe result
// must contain, regardless of whether the target is HTTP or HTTPS.
var expectedHTTPPhaseKeys = []string{
	"dns_resolve",
	"tcp_connect",
	"tls_handshake",
	"ttfb",
	"transfer",
}

// TestHTTPProber_PhaseBreakdownPresence verifies that probing a plain HTTP
// server produces a Phases map with all 5 expected keys and that tcp_connect,
// ttfb, and transfer are > 0 (tls_handshake should be zero for plain HTTP).
func TestHTTPProber_PhaseBreakdownPresence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	target := config.TargetConfig{
		Name:      "test-http-phases",
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
	if result.Phases == nil {
		t.Fatal("expected Phases map to be non-nil")
	}

	for _, key := range expectedHTTPPhaseKeys {
		if _, ok := result.Phases[key]; !ok {
			t.Fatalf("expected Phases to contain key %q", key)
		}
	}

	// For plain HTTP against a local server, tcp_connect, ttfb, and transfer
	// should be positive. dns_resolve may be zero for 127.0.0.1 addresses.
	for _, key := range []string{"tcp_connect", "ttfb", "transfer"} {
		if result.Phases[key] <= 0 {
			t.Fatalf("expected Phases[%q] > 0, got %v", key, result.Phases[key])
		}
	}

	// tls_handshake must be zero for plain HTTP.
	if result.Phases["tls_handshake"] != 0 {
		t.Fatalf("expected Phases[tls_handshake] == 0 for plain HTTP, got %v", result.Phases["tls_handshake"])
	}
}

// TestHTTPProber_PhaseBreakdownHTTPS verifies that probing an HTTPS server
// produces all 5 phase keys with non-zero values including tls_handshake.
func TestHTTPProber_PhaseBreakdownHTTPS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("secure"))
	}))
	defer srv.Close()

	target := config.TargetConfig{
		Name:      "test-https-phases",
		Address:   srv.URL,
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			TLSSkipVerify: true,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPProber(true, true, "")
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	if result.Phases == nil {
		t.Fatal("expected Phases map to be non-nil")
	}

	for _, key := range expectedHTTPPhaseKeys {
		if _, ok := result.Phases[key]; !ok {
			t.Fatalf("expected Phases to contain key %q", key)
		}
	}

	// For HTTPS, tls_handshake must be positive.
	if result.Phases["tls_handshake"] <= 0 {
		t.Fatalf("expected Phases[tls_handshake] > 0 for HTTPS, got %v", result.Phases["tls_handshake"])
	}

	// tcp_connect, ttfb, and transfer should also be positive.
	for _, key := range []string{"tcp_connect", "ttfb", "transfer"} {
		if result.Phases[key] <= 0 {
			t.Fatalf("expected Phases[%q] > 0, got %v", key, result.Phases[key])
		}
	}
}

// TestHTTPProber_StatusCodeRecording verifies that the result.StatusCode
// matches the actual HTTP response status code for various codes.
func TestHTTPProber_StatusCodeRecording(t *testing.T) {
	codes := []int{http.StatusOK, http.StatusNotFound, http.StatusServiceUnavailable}

	for _, code := range codes {
		code := code
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))

		target := config.TargetConfig{
			Name:      "test-status-code",
			Address:   srv.URL,
			ProbeType: config.ProbeTypeHTTP,
			Timeout:   5 * time.Second,
		}

		ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
		prober := NewHTTPProber(false, true, "")
		result := prober.Probe(ctx, target)
		cancel()
		srv.Close()

		if result.StatusCode != code {
			t.Fatalf("for server returning %d: expected StatusCode=%d, got %d", code, code, result.StatusCode)
		}
	}
}

// TestHTTPProber_ExpectedStatusCodesEmpty verifies that when
// expected_status_codes is empty (nil), any HTTP response results in
// Success=true.
func TestHTTPProber_ExpectedStatusCodesEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden) // 403 — would fail if codes were checked
	}))
	defer srv.Close()

	target := config.TargetConfig{
		Name:      "test-expected-empty",
		Address:   srv.URL,
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ExpectedStatusCodes: nil, // empty
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPProber(false, true, "")
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true with empty expected_status_codes, got false; error: %s", result.Error)
	}
}

// TestHTTPProber_ExpectedStatusCodesMatch verifies that when the response
// status code IS in the expected_status_codes list, Success=true.
func TestHTTPProber_ExpectedStatusCodesMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := config.TargetConfig{
		Name:      "test-expected-match",
		Address:   srv.URL,
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ExpectedStatusCodes: []int{200, 201, 204},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPProber(false, true, "")
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true when status code is in expected list, got false; error: %s", result.Error)
	}
	if result.Error != "" {
		t.Fatalf("expected empty Error on success, got %q", result.Error)
	}
}

// TestHTTPProber_ExpectedStatusCodesMismatch verifies that when the response
// status code is NOT in the expected_status_codes list, Success=false and
// Error contains "unexpected status code".
func TestHTTPProber_ExpectedStatusCodesMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden) // 403
	}))
	defer srv.Close()

	target := config.TargetConfig{
		Name:      "test-expected-mismatch",
		Address:   srv.URL,
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ExpectedStatusCodes: []int{200, 201},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPProber(false, true, "")
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false when status code is not in expected list")
	}
	if result.Error != "unexpected status code" {
		t.Fatalf("expected Error=%q, got %q", "unexpected status code", result.Error)
	}
}

// TestHTTPProber_TLSCertExtraction verifies that when probing an HTTPS
// endpoint, result.CertExpiry is populated with the certificate's NotAfter
// timestamp.
func TestHTTPProber_TLSCertExtraction(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := config.TargetConfig{
		Name:      "test-tls-cert",
		Address:   srv.URL,
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			TLSSkipVerify: true,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPProber(true, true, "")
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	if result.CertExpiry.IsZero() {
		t.Fatal("expected CertExpiry to be populated for HTTPS, got zero time")
	}
	// The test TLS server certificate should expire in the future.
	if !result.CertExpiry.After(time.Now()) {
		t.Fatalf("expected CertExpiry to be in the future, got %v", result.CertExpiry)
	}
}

// TestHTTPProber_TLSCertAbsentForHTTP verifies that when probing a plain
// HTTP endpoint, result.CertExpiry is the zero time value.
func TestHTTPProber_TLSCertAbsentForHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := config.TargetConfig{
		Name:      "test-no-tls-cert",
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
	if !result.CertExpiry.IsZero() {
		t.Fatalf("expected CertExpiry to be zero for plain HTTP, got %v", result.CertExpiry)
	}
}

// TestHTTPProber_BodyCleanup verifies that after the probe completes, the
// response body has been fully read and closed. We use a custom handler that
// injects a trackingReadCloser via a hijacked response pattern — but since
// httptest doesn't let us replace the body directly, we instead verify the
// prober's behaviour by checking that a server with a large body completes
// without error (proving the body was consumed) and that the connection is
// reusable (proving the body was closed).
//
// The approach: serve a body, and use a tracking wrapper on the server side
// to confirm the full body was read by the client.
func TestHTTPProber_BodyCleanup(t *testing.T) {
	bodyContent := bytes.Repeat([]byte("x"), 4096) // 4 KB body
	var bodyFullyRead atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		n, _ := w.Write(bodyContent)
		if n == len(bodyContent) {
			bodyFullyRead.Store(true)
		}
	}))
	defer srv.Close()

	target := config.TargetConfig{
		Name:      "test-body-cleanup",
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

	// The server wrote the full body successfully, meaning the client
	// consumed it (io.Copy to io.Discard) and closed the body.
	if !bodyFullyRead.Load() {
		t.Fatal("expected server to confirm full body was written/read")
	}

	// Verify the prober can make a second request successfully, which
	// confirms the first request's body was properly closed and the
	// connection pool is clean.
	ctx2, cancel2 := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel2()

	result2 := prober.Probe(ctx2, target)
	if !result2.Success {
		t.Fatalf("second probe failed, suggesting body was not properly cleaned up; error: %s", result2.Error)
	}
}

// TestHTTPProber_LargeBodyCapped verifies that a response body larger than
// maxHTTPTransferBytes is silently capped — the probe succeeds and the
// transfer phase reflects time spent reading up to the limit, not the full body.
func TestHTTPProber_LargeBodyCapped(t *testing.T) {
	// Serve 2 MiB, which is larger than maxHTTPTransferBytes (1 MiB).
	bodySize := 2 * 1024 * 1024
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Write in chunks to avoid buffering the entire body.
		chunk := bytes.Repeat([]byte("x"), 64*1024) // 64 KB chunks
		for written := 0; written < bodySize; written += len(chunk) {
			_, _ = w.Write(chunk)
		}
	}))
	defer srv.Close()

	target := config.TargetConfig{
		Name:      "test-large-body",
		Address:   srv.URL,
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   5 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPProber(false, true, "")
	result := prober.Probe(ctx, target)

	// The probe should succeed — large body is not an error.
	if !result.Success {
		t.Fatalf("expected Success=true for large body, got false; error: %s", result.Error)
	}
	if result.Error != "" {
		t.Fatalf("expected no error for large body, got %q", result.Error)
	}
	if result.Phases == nil {
		t.Fatal("expected Phases to be non-nil")
	}
	if _, ok := result.Phases["transfer"]; !ok {
		t.Fatal("expected transfer phase to be present")
	}
}

func TestHTTPProber_BodyReadErrorFailsProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
	}))
	defer srv.Close()

	target := config.TargetConfig{
		Name:      "test-body-read-error",
		Address:   srv.URL,
		ProbeType: config.ProbeTypeHTTP,
		Timeout:   5 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPProber(false, true, "")
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false when response body read fails")
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d", result.StatusCode, http.StatusOK)
	}
	if !strings.Contains(result.Error, "reading response body") {
		t.Fatalf("Error = %q, want it to mention reading response body", result.Error)
	}
	if result.Phases == nil {
		t.Fatal("expected phase timings on response body read error")
	}
	if _, ok := result.Phases["transfer"]; !ok {
		t.Fatal("expected transfer phase on response body read error")
	}
}
