package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"netsonar/internal/config"
)

// TestHTTPBodyProber_RegexMatchSuccess verifies that when the response body
// matches the configured body_match_regex, BodyMatch=true and Success=true.
func TestHTTPBodyProber_RegexMatchSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"healthy","version":"1.2.3"}`))
	}))
	defer srv.Close()

	target := config.TargetConfig{
		Name:      "test-body-regex-match",
		Address:   srv.URL,
		ProbeType: config.ProbeTypeHTTPBody,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			BodyMatchRegex: `"status"\s*:\s*"healthy"`,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPBodyProber(false, true, "", target.ProbeOpts.BodyMatchRegex)
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	if !result.BodyMatch {
		t.Fatal("expected BodyMatch=true when regex matches")
	}
	if result.Error != "" {
		t.Fatalf("expected empty Error, got %q", result.Error)
	}
}

// TestHTTPBodyProber_RegexMatchFailure verifies that when the response body
// does not match the regex, both BodyMatch and Success are false.
func TestHTTPBodyProber_RegexMatchFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"degraded"}`))
	}))
	defer srv.Close()

	target := config.TargetConfig{
		Name:      "test-body-regex-no-match",
		Address:   srv.URL,
		ProbeType: config.ProbeTypeHTTPBody,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			BodyMatchRegex: `"status"\s*:\s*"healthy"`,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPBodyProber(false, true, "", target.ProbeOpts.BodyMatchRegex)
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false when body does not match regex")
	}
	if result.BodyMatch {
		t.Fatal("expected BodyMatch=false when regex does not match")
	}
}

// TestHTTPBodyProber_SubstringMatchSuccess verifies that when the response
// body contains the configured body_match_string, BodyMatch=true.
func TestHTTPBodyProber_SubstringMatchSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Welcome to the application dashboard"))
	}))
	defer srv.Close()

	target := config.TargetConfig{
		Name:      "test-body-substring-match",
		Address:   srv.URL,
		ProbeType: config.ProbeTypeHTTPBody,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			BodyMatchString: "application dashboard",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPBodyProber(false, true, "", "")
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	if !result.BodyMatch {
		t.Fatal("expected BodyMatch=true when substring is present")
	}
}

// TestHTTPBodyProber_SubstringMatchFailure verifies that when the response
// body does not contain the configured string, both BodyMatch and Success are false.
func TestHTTPBodyProber_SubstringMatchFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Page not found"))
	}))
	defer srv.Close()

	target := config.TargetConfig{
		Name:      "test-body-substring-no-match",
		Address:   srv.URL,
		ProbeType: config.ProbeTypeHTTPBody,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			BodyMatchString: "application dashboard",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPBodyProber(false, true, "", "")
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false when body does not match substring")
	}
	if result.BodyMatch {
		t.Fatal("expected BodyMatch=false when substring is absent")
	}
}

// TestHTTPBodyProber_InvalidRegex verifies that an invalid body_match_regex
// results in both BodyMatch=false and Success=false.
func TestHTTPBodyProber_InvalidRegex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("some body content"))
	}))
	defer srv.Close()

	target := config.TargetConfig{
		Name:      "test-body-invalid-regex",
		Address:   srv.URL,
		ProbeType: config.ProbeTypeHTTPBody,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			BodyMatchRegex: `[invalid(regex`,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPBodyProber(false, true, "", target.ProbeOpts.BodyMatchRegex)
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false for invalid regex")
	}
	if result.BodyMatch {
		t.Fatal("expected BodyMatch=false for invalid regex")
	}
}

// TestHTTPBodyProber_StatusCodeRecording verifies that result.StatusCode
// matches the actual HTTP response status code.
func TestHTTPBodyProber_StatusCodeRecording(t *testing.T) {
	codes := []int{http.StatusOK, http.StatusNotFound, http.StatusServiceUnavailable}

	for _, code := range codes {
		code := code
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte("body"))
		}))

		target := config.TargetConfig{
			Name:      "test-body-status",
			Address:   srv.URL,
			ProbeType: config.ProbeTypeHTTPBody,
			Timeout:   5 * time.Second,
			ProbeOpts: config.ProbeOptions{
				BodyMatchString: "body",
			},
		}

		ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
		prober := NewHTTPBodyProber(false, true, "", "")
		result := prober.Probe(ctx, target)
		cancel()
		srv.Close()

		if result.StatusCode != code {
			t.Fatalf("for server returning %d: expected StatusCode=%d, got %d", code, code, result.StatusCode)
		}
	}
}

// TestHTTPBodyProber_ExpectedStatusCodesMismatch verifies that body matches
// do not hide an unexpected HTTP status code.
func TestHTTPBodyProber_ExpectedStatusCodesMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("healthy"))
	}))
	defer srv.Close()

	target := config.TargetConfig{
		Name:      "test-body-status-mismatch",
		Address:   srv.URL,
		ProbeType: config.ProbeTypeHTTPBody,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			BodyMatchString:     "healthy",
			ExpectedStatusCodes: []int{http.StatusOK},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPBodyProber(false, true, "", "")
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false when body matches but status code is unexpected")
	}
	if !result.BodyMatch {
		t.Fatal("expected BodyMatch=true when body contains the configured string")
	}
	if result.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected StatusCode=%d, got %d", http.StatusInternalServerError, result.StatusCode)
	}
	if !strings.Contains(result.Error, "unexpected status code") {
		t.Fatalf("expected unexpected status code error, got %q", result.Error)
	}
}

// TestHTTPBodyProber_NeitherPatternConfigured verifies that when neither
// body_match_regex nor body_match_string is set, both BodyMatch and Success are false.
func TestHTTPBodyProber_NeitherPatternConfigured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("some content"))
	}))
	defer srv.Close()

	target := config.TargetConfig{
		Name:      "test-body-no-pattern",
		Address:   srv.URL,
		ProbeType: config.ProbeTypeHTTPBody,
		Timeout:   5 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPBodyProber(false, true, "", "")
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false when no pattern is configured")
	}
	if result.BodyMatch {
		t.Fatal("expected BodyMatch=false when no pattern is configured")
	}
}

// TestHTTPBodyProber_RegexPrecedenceOverSubstring verifies that when both
// body_match_regex and body_match_string are configured, the regex takes
// precedence.
func TestHTTPBodyProber_RegexPrecedenceOverSubstring(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello world"))
	}))
	defer srv.Close()

	// Regex does NOT match, but substring DOES match.
	// Since regex takes precedence, BodyMatch should be false.
	target := config.TargetConfig{
		Name:      "test-body-regex-precedence",
		Address:   srv.URL,
		ProbeType: config.ProbeTypeHTTPBody,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			BodyMatchRegex:  `^goodbye`,
			BodyMatchString: "hello",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPBodyProber(false, true, "", target.ProbeOpts.BodyMatchRegex)
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false because regex takes precedence and does not match")
	}
	if result.BodyMatch {
		t.Fatal("expected BodyMatch=false because regex takes precedence and does not match")
	}
}

// TestHTTPBodyProber_DefaultMethodGET verifies that when no method is
// specified, the prober defaults to GET.
func TestHTTPBodyProber_DefaultMethodGET(t *testing.T) {
	var receivedMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	target := config.TargetConfig{
		Name:      "test-body-default-method",
		Address:   srv.URL,
		ProbeType: config.ProbeTypeHTTPBody,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			BodyMatchString: "ok",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPBodyProber(false, true, "", "")
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	if receivedMethod != http.MethodGet {
		t.Fatalf("expected default method GET, got %q", receivedMethod)
	}
}

// TestHTTPBodyProber_CustomHeaders verifies that custom headers from
// probe_opts are sent with the request.
func TestHTTPBodyProber_CustomHeaders(t *testing.T) {
	var receivedUA string
	var receivedCustom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedUA = r.Header.Get("User-Agent")
		receivedCustom = r.Header.Get("X-Custom-Header")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	target := config.TargetConfig{
		Name:      "test-body-custom-headers",
		Address:   srv.URL,
		ProbeType: config.ProbeTypeHTTPBody,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			Headers: map[string]string{
				"User-Agent":      "netsonar/test",
				"X-Custom-Header": "test-value",
			},
			BodyMatchString: "ok",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPBodyProber(false, true, "", "")
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	if receivedUA != "netsonar/test" {
		t.Fatalf("expected User-Agent=%q, got %q", "netsonar/test", receivedUA)
	}
	if receivedCustom != "test-value" {
		t.Fatalf("expected X-Custom-Header=%q, got %q", "test-value", receivedCustom)
	}
}

// TestHTTPBodyProber_BodyFullyReadAndClosed verifies that the response body
// is fully consumed and the connection is properly cleaned up.
func TestHTTPBodyProber_BodyFullyReadAndClosed(t *testing.T) {
	bodyContent := make([]byte, 4096)
	for i := range bodyContent {
		bodyContent[i] = 'x'
	}
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
		ProbeType: config.ProbeTypeHTTPBody,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			BodyMatchString: "x",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPBodyProber(false, true, "", "")
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	if !bodyFullyRead.Load() {
		t.Fatal("expected server to confirm full body was written/read")
	}

	// Verify a second request succeeds (connection pool is clean).
	ctx2, cancel2 := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel2()
	result2 := prober.Probe(ctx2, target)
	if !result2.Success {
		t.Fatalf("second probe failed, suggesting body was not properly cleaned up; error: %s", result2.Error)
	}
}

func TestHTTPBodyProber_BodyExceedsLimit(t *testing.T) {
	bigBody := strings.Repeat("x", int(maxHTTPBodyBytes)+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(bigBody))
	}))
	defer srv.Close()

	target := config.TargetConfig{
		Name:      "test-body-too-large",
		Address:   srv.URL,
		ProbeType: config.ProbeTypeHTTPBody,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			BodyMatchString: "x",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPBodyProber(false, true, "", "")
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false for body exceeding limit")
	}
	if !strings.Contains(result.Error, "exceeds") {
		t.Errorf("error = %q, want it to mention exceeds", result.Error)
	}
}

func TestHTTPBodyProber_BodyExactlyAtLimit(t *testing.T) {
	body := strings.Repeat("x", int(maxHTTPBodyBytes))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	target := config.TargetConfig{
		Name:      "test-body-at-limit",
		Address:   srv.URL,
		ProbeType: config.ProbeTypeHTTPBody,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			BodyMatchString: "x",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := NewHTTPBodyProber(false, true, "", "")
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true for body at limit, got error: %s", result.Error)
	}
	if !result.BodyMatch {
		t.Fatal("expected BodyMatch=true")
	}
}
