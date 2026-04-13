// Package probe — ProxyProber implementation.
package probe

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"netsonar/internal/config"
)

// ProxyProber probes connectivity through an HTTP CONNECT proxy by
// establishing a tunnel to the target address and measuring the tunnel
// establishment time.
type ProxyProber struct{}

// Probe establishes an HTTP CONNECT tunnel through the configured proxy
// (target.ProbeOpts.ProxyURL) to target.Address and measures the total
// tunnel establishment duration.
//
// Preconditions:
//   - target.ProbeOpts.ProxyURL is a valid HTTP proxy URL
//   - target.Address is a valid HTTPS URL or host:port
//   - ctx carries the probe timeout (set by the scheduler)
//
// Postconditions:
//   - result.Success is true iff the proxy tunnel was established
//   - result.Duration reflects wall-clock time from start to tunnel ready
//   - result.Phases contains proxy_dial, optionally proxy_tls, and proxy_connect
//   - All connections are closed before returning
//   - result.Error is non-empty when Success is false
func (p *ProxyProber) Probe(ctx context.Context, target config.TargetConfig) ProbeResult {
	var result ProbeResult

	proxyURL, err := url.Parse(target.ProbeOpts.ProxyURL)
	if err != nil {
		result.Error = fmt.Sprintf("invalid proxy_url: %s", err.Error())
		return result
	}

	// Determine the tunnel destination from target.Address.
	tunnelDest, err := parseTunnelDestination(target.Address)
	if err != nil {
		result.Error = fmt.Sprintf("invalid target address: %s", err.Error())
		return result
	}

	// Resolve the proxy host:port for dialing.
	proxyAddr := proxyURL.Host
	if _, _, splitErr := net.SplitHostPort(proxyAddr); splitErr != nil {
		// No port specified; default based on scheme.
		if proxyURL.Scheme == "https" {
			proxyAddr = net.JoinHostPort(proxyAddr, "443")
		} else {
			proxyAddr = net.JoinHostPort(proxyAddr, "80")
		}
	}

	start := time.Now()
	phases := make(map[string]time.Duration, 3)

	// Dial the proxy.
	var d net.Dialer
	proxyConn, err := d.DialContext(ctx, "tcp", proxyAddr)
	proxyDialDone := time.Now()
	if err != nil {
		result.Duration = time.Since(start)
		result.Error = fmt.Sprintf("proxy dial: %s", err.Error())
		return result
	}
	phases["proxy_dial"] = proxyDialDone.Sub(start)
	defer func() { _ = proxyConn.Close() }()

	// If the proxy itself is HTTPS, wrap with TLS.
	if proxyURL.Scheme == "https" {
		host, _, _ := net.SplitHostPort(proxyAddr)
		tlsCfg := &tls.Config{ServerName: host}
		tlsConn := tls.Client(proxyConn, tlsCfg)
		tlsStart := time.Now()
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			result.Duration = time.Since(start)
			result.Error = fmt.Sprintf("proxy tls handshake: %s", err.Error())
			return result
		}
		phases["proxy_tls"] = time.Since(tlsStart)
		proxyConn = tlsConn
	}

	// Send HTTP CONNECT request through the proxy. For CONNECT the
	// request-target must be "host:port" (RFC 7231 §4.3.6). Go's
	// http.NewRequest parses a bare "host:port" as scheme:opaque, which
	// produces a malformed request line ("CONNECT 443 HTTP/1.1").
	// Build the URL explicitly so req.Write emits the correct form.
	connectReq := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: tunnelDest},
		Host:   tunnelDest,
		Header: make(http.Header),
	}
	connectReq = connectReq.WithContext(ctx)
	setProxyAuthorization(connectReq, proxyURL)

	connectStart := time.Now()
	if err := connectReq.Write(proxyConn); err != nil {
		result.Duration = time.Since(start)
		result.Error = fmt.Sprintf("writing CONNECT request: %s", err.Error())
		return result
	}

	// Read the proxy's response.
	resp, err := http.ReadResponse(bufio.NewReader(proxyConn), connectReq)
	if err != nil {
		result.Duration = time.Since(start)
		result.Error = fmt.Sprintf("reading CONNECT response: %s", err.Error())
		return result
	}
	// Intentionally not draining resp.Body before Close:
	// - On 200 OK the body is a CONNECT tunnel stream; draining would block
	//   until ctx deadline and corrupt result.Duration.
	// - On non-200 the body is typically short/empty and proxyConn is closed
	//   by defer — there is no connection pool to return a clean conn to.
	_ = resp.Body.Close()

	result.Duration = time.Since(start)
	phases["proxy_connect"] = time.Since(connectStart)
	result.Phases = phases

	if resp.StatusCode != http.StatusOK {
		result.Error = fmt.Sprintf("proxy CONNECT returned status %d", resp.StatusCode)
		return result
	}

	result.Success = true

	return result
}

// setProxyAuthorization applies Basic proxy authentication from proxy URL
// userinfo. A username without a password is encoded as "user:".
func setProxyAuthorization(req *http.Request, proxyURL *url.URL) {
	if proxyURL.User == nil {
		return
	}

	username := proxyURL.User.Username()
	password, _ := proxyURL.User.Password()
	auth := username + ":" + password
	req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(auth)))
}

// parseTunnelDestination extracts a host:port suitable for an HTTP CONNECT
// request from the target address. If the address is a URL, the host and
// port are extracted (defaulting to 443 for HTTPS, 80 for HTTP). If it is
// already a host:port, it is returned as-is.
func parseTunnelDestination(address string) (string, error) {
	u, err := url.Parse(address)
	if err == nil && u.Scheme != "" && u.Host != "" {
		host := u.Host
		if _, _, splitErr := net.SplitHostPort(host); splitErr != nil {
			if u.Scheme == "https" {
				host = net.JoinHostPort(host, "443")
			} else {
				host = net.JoinHostPort(host, "80")
			}
		}
		return host, nil
	}

	// Try as host:port directly.
	if _, _, splitErr := net.SplitHostPort(address); splitErr == nil {
		return address, nil
	}

	return "", fmt.Errorf("cannot determine host:port from %q", address)
}
