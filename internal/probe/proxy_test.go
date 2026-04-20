package probe

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"netsonar/internal/config"
)

// mockProxy starts a simple HTTP CONNECT proxy that accepts a connection,
// reads the CONNECT request, and responds with the given status code.
// It returns the listener address and a cleanup function.
func mockProxy(t *testing.T, statusCode int) (string, func()) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start mock proxy listener: %v", err)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				// Read the CONNECT request.
				req, err := http.ReadRequest(bufio.NewReader(c))
				if err != nil {
					return
				}
				_ = req.Body.Close()

				// Respond with the configured status code.
				resp := fmt.Sprintf("HTTP/1.1 %d %s\r\n\r\n", statusCode, http.StatusText(statusCode))
				_, _ = c.Write([]byte(resp))
			}(conn)
		}
	}()

	return ln.Addr().String(), func() { _ = ln.Close() }
}

func mockAuthProxy(t *testing.T, expectedAuth string) (string, func()) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start mock proxy listener: %v", err)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()

				req, err := http.ReadRequest(bufio.NewReader(c))
				if err != nil {
					return
				}
				_ = req.Body.Close()

				statusCode := http.StatusOK
				if got := req.Header.Get("Proxy-Authorization"); got != expectedAuth {
					statusCode = http.StatusProxyAuthRequired
				}

				resp := fmt.Sprintf("HTTP/1.1 %d %s\r\n\r\n", statusCode, http.StatusText(statusCode))
				_, _ = c.Write([]byte(resp))
			}(conn)
		}
	}()

	return ln.Addr().String(), func() { _ = ln.Close() }
}

func basicProxyAuth(username, password string) string {
	token := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	return "Basic " + token
}

func mockClosingProxy(t *testing.T) (string, func()) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start mock proxy listener: %v", err)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	return ln.Addr().String(), func() { _ = ln.Close() }
}

// TestProxyProber_Success verifies that probing through a mock proxy that
// returns 200 reports Success=true, Duration>0, and proxy phase timings.
func TestProxyProber_Success(t *testing.T) {
	proxyAddr, cleanup := mockProxy(t, http.StatusOK)
	defer cleanup()

	target := config.TargetConfig{
		Name:      "test-proxy-success",
		Address:   "https://example.com",
		ProbeType: config.ProbeTypeProxy,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ProxyURL: "http://" + proxyAddr,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &ProxyProber{}
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	if result.Duration <= 0 {
		t.Fatalf("expected Duration > 0, got %v", result.Duration)
	}
	if result.Error != "" {
		t.Fatalf("expected empty Error on success, got %q", result.Error)
	}
	if result.Phases == nil {
		t.Fatal("expected Phases map to be non-nil")
	}
	for _, phase := range []string{"proxy_dial", "proxy_connect"} {
		if result.Phases[phase] <= 0 {
			t.Fatalf("expected Phases[%q] > 0, got %v", phase, result.Phases[phase])
		}
	}
	if _, ok := result.Phases["proxy_tls"]; ok {
		t.Fatal("did not expect proxy_tls phase for http proxy")
	}
	phaseSum := result.Phases["proxy_dial"] + result.Phases["proxy_connect"]
	const timingSlack = time.Millisecond
	if phaseSum > result.Duration+timingSlack {
		t.Fatalf("phase sum %v exceeds duration %v plus slack %v", phaseSum, result.Duration, timingSlack)
	}
}

func TestProxyProber_SendsProxyAuthorizationFromURLUserinfo(t *testing.T) {
	proxyAddr, cleanup := mockAuthProxy(t, basicProxyAuth("user", "pass"))
	defer cleanup()

	target := config.TargetConfig{
		Name:      "test-proxy-auth",
		Address:   "https://example.com",
		ProbeType: config.ProbeTypeProxy,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ProxyURL: "http://user:pass@" + proxyAddr,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &ProxyProber{}
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true with proxy credentials, got false; error: %s", result.Error)
	}
}

func TestProxyProber_SendsProxyAuthorizationWithEmptyPassword(t *testing.T) {
	proxyAddr, cleanup := mockAuthProxy(t, basicProxyAuth("user", ""))
	defer cleanup()

	target := config.TargetConfig{
		Name:      "test-proxy-auth-empty-password",
		Address:   "https://example.com",
		ProbeType: config.ProbeTypeProxy,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ProxyURL: "http://user@" + proxyAddr,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &ProxyProber{}
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true with empty proxy password, got false; error: %s", result.Error)
	}
}

func TestProxyProber_DoesNotSendProxyAuthorizationWithoutURLUserinfo(t *testing.T) {
	proxyAddr, cleanup := mockAuthProxy(t, "")
	defer cleanup()

	target := config.TargetConfig{
		Name:      "test-proxy-no-auth",
		Address:   "https://example.com",
		ProbeType: config.ProbeTypeProxy,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ProxyURL: "http://" + proxyAddr,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &ProxyProber{}
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true without proxy credentials, got false; error: %s", result.Error)
	}
}

func TestProxyProber_DoesNotLeakProxyCredentialsInErrors(t *testing.T) {
	proxyAddr, cleanup := mockAuthProxy(t, basicProxyAuth("other-user", "other-pass"))
	defer cleanup()

	target := config.TargetConfig{
		Name:      "test-proxy-auth-error-redaction",
		Address:   "https://example.com",
		ProbeType: config.ProbeTypeProxy,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ProxyURL: "http://user:secret@" + proxyAddr,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &ProxyProber{}
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false when proxy rejects credentials")
	}
	if strings.Contains(result.Error, "user") || strings.Contains(result.Error, "secret") {
		t.Fatalf("expected error not to leak proxy credentials, got %q", result.Error)
	}
}

func TestProxyProber_PreservesPhasesOnHTTPSProxyTLSFailure(t *testing.T) {
	proxyAddr, cleanup := mockClosingProxy(t)
	defer cleanup()

	target := config.TargetConfig{
		Name:      "test-proxy-tls-failure-phases",
		Address:   "https://example.com",
		ProbeType: config.ProbeTypeProxy,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ProxyURL: "https://" + proxyAddr,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &ProxyProber{}
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false for HTTPS proxy TLS failure")
	}
	if !strings.Contains(result.Error, "proxy tls handshake") {
		t.Fatalf("expected TLS handshake error, got %q", result.Error)
	}
	if result.Phases["proxy_dial"] <= 0 {
		t.Fatalf("expected proxy_dial phase to be preserved, got %v", result.Phases)
	}
	if _, ok := result.Phases["proxy_tls"]; ok {
		t.Fatalf("did not expect proxy_tls phase on failed handshake, got %v", result.Phases)
	}
}

// TestProxyProber_ProxyReturnsNon200 verifies that when the proxy returns
// a non-200 status, Success=false and Error contains the status code.
func TestProxyProber_ProxyReturnsNon200(t *testing.T) {
	proxyAddr, cleanup := mockProxy(t, http.StatusForbidden)
	defer cleanup()

	target := config.TargetConfig{
		Name:      "test-proxy-non200",
		Address:   "https://example.com",
		ProbeType: config.ProbeTypeProxy,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ProxyURL: "http://" + proxyAddr,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &ProxyProber{}
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false when proxy returns non-200")
	}
	if result.Error == "" {
		t.Fatal("expected non-empty Error")
	}
	// Error should mention the status code.
	expected := "proxy CONNECT returned status 403"
	if result.Error != expected {
		t.Fatalf("expected Error=%q, got %q", expected, result.Error)
	}
	if result.Phases["proxy_dial"] <= 0 {
		t.Fatalf("expected proxy_dial phase to be preserved, got %v", result.Phases)
	}
	if result.Phases["proxy_connect"] <= 0 {
		t.Fatalf("expected proxy_connect phase to be preserved, got %v", result.Phases)
	}
}

// TestProxyProber_InvalidProxyURL verifies that an invalid proxy URL
// results in Success=false with a descriptive error.
func TestProxyProber_InvalidProxyURL(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-proxy-invalid-url",
		Address:   "https://example.com",
		ProbeType: config.ProbeTypeProxy,
		Timeout:   2 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ProxyURL: "://not-a-valid-url",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &ProxyProber{}
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false for invalid proxy URL")
	}
	if result.Error == "" {
		t.Fatal("expected non-empty Error for invalid proxy URL")
	}
}

// TestProxyProber_InvalidTargetAddress verifies that an invalid target
// address results in Success=false with a descriptive error.
func TestProxyProber_InvalidTargetAddress(t *testing.T) {
	proxyAddr, cleanup := mockProxy(t, http.StatusOK)
	defer cleanup()

	target := config.TargetConfig{
		Name:      "test-proxy-invalid-target",
		Address:   "not-a-url-or-host-port",
		ProbeType: config.ProbeTypeProxy,
		Timeout:   2 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ProxyURL: "http://" + proxyAddr,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &ProxyProber{}
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false for invalid target address")
	}
	if result.Error == "" {
		t.Fatal("expected non-empty Error for invalid target address")
	}
}

// TestProxyProber_ConnectionRefused verifies that when the proxy is not
// reachable, Success=false and Error is non-empty.
func TestProxyProber_ConnectionRefused(t *testing.T) {
	// Get an unused port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	target := config.TargetConfig{
		Name:      "test-proxy-refused",
		Address:   "https://example.com",
		ProbeType: config.ProbeTypeProxy,
		Timeout:   2 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ProxyURL: "http://" + addr,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &ProxyProber{}
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false for connection refused")
	}
	if result.Error == "" {
		t.Fatal("expected non-empty Error for connection refused")
	}
}

// TestParseTunnelDestination_HTTPS verifies that an HTTPS URL extracts
// host:443.
func TestParseTunnelDestination_HTTPS(t *testing.T) {
	dest, err := parseTunnelDestination("https://example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dest != "example.com:443" {
		t.Fatalf("expected %q, got %q", "example.com:443", dest)
	}
}

// TestParseTunnelDestination_HTTP verifies that an HTTP URL extracts
// host:80.
func TestParseTunnelDestination_HTTP(t *testing.T) {
	dest, err := parseTunnelDestination("http://example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dest != "example.com:80" {
		t.Fatalf("expected %q, got %q", "example.com:80", dest)
	}
}

// TestParseTunnelDestination_HostPort verifies that a host:port string
// is returned as-is.
func TestParseTunnelDestination_HostPort(t *testing.T) {
	dest, err := parseTunnelDestination("example.com:8443")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dest != "example.com:8443" {
		t.Fatalf("expected %q, got %q", "example.com:8443", dest)
	}
}

// TestParseTunnelDestination_Invalid verifies that an invalid input
// returns an error.
func TestParseTunnelDestination_Invalid(t *testing.T) {
	_, err := parseTunnelDestination("not-a-url-or-host-port")
	if err == nil {
		t.Fatal("expected error for invalid input, got nil")
	}
}

// TestParseTunnelDestination_IPv6 verifies that IPv6 literals in URLs are
// returned with a single pair of brackets and the scheme-default port.
func TestParseTunnelDestination_IPv6(t *testing.T) {
	cases := []struct {
		name    string
		address string
		want    string
	}{
		{"https no port", "https://[2001:db8::1]", "[2001:db8::1]:443"},
		{"http no port", "http://[::1]", "[::1]:80"},
		{"https explicit port", "https://[2001:db8::1]:8443", "[2001:db8::1]:8443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTunnelDestination(tc.address)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("parseTunnelDestination(%q) = %q, want %q", tc.address, got, tc.want)
			}
		})
	}
}

// TestHostPortForURL covers the scheme-default port and IPv6 literal paths
// used by ProxyProber when resolving the dial address for the proxy itself.
func TestHostPortForURL(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"http ipv4 no port", "http://127.0.0.1", "127.0.0.1:80"},
		{"https ipv4 no port", "https://127.0.0.1", "127.0.0.1:443"},
		{"http ipv6 no port", "http://[2001:db8::1]", "[2001:db8::1]:80"},
		{"https ipv6 no port", "https://[::1]", "[::1]:443"},
		{"http ipv6 explicit port", "http://[::1]:3128", "[::1]:3128"},
		{"http hostname explicit port", "http://proxy.example:3128", "proxy.example:3128"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.raw)
			if err != nil {
				t.Fatalf("url.Parse(%q): %v", tc.raw, err)
			}
			got := hostPortForURL(u)
			if got != tc.want {
				t.Fatalf("hostPortForURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}
