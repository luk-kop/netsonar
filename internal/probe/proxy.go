// Package probe — ProxyProber implementation.
package probe

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"time"

	"netsonar/internal/config"
	"netsonar/internal/proxyurl"
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
func (p *ProxyProber) Probe(ctx context.Context, target config.TargetConfig) (result ProbeResult) {
	proxyURL, err := proxyurl.Parse(target.ProbeOpts.ProxyURL)
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

	start := time.Now()
	proxyConn, phases, err := dialProxyTunnel(ctx, proxyURL, tunnelDest)
	result.Duration = time.Since(start)
	if len(phases) > 0 {
		result.Phases = phases
	}
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer func() { _ = proxyConn.Close() }()

	result.Success = true

	return result
}

// parseTunnelDestination extracts a host:port suitable for an HTTP CONNECT
// request from the target address. If the address is a URL, the host and
// port are extracted (defaulting to 443 for HTTPS, 80 for HTTP). If it is
// already a host:port, it is returned as-is.
func parseTunnelDestination(address string) (string, error) {
	u, err := url.Parse(address)
	if err == nil && u.Scheme != "" && u.Host != "" {
		return hostPortForURL(u), nil
	}

	// Try as host:port directly.
	if _, _, splitErr := net.SplitHostPort(address); splitErr == nil {
		return address, nil
	}

	return "", fmt.Errorf("cannot determine host:port from %q", address)
}

// hostPortForURL returns host:port from a URL, applying the scheme default
// (443 for https, 80 otherwise) when the URL has no explicit port. Uses
// url.Hostname/Port so IPv6 literals like http://[::1] round-trip correctly
// without double-bracketing.
func hostPortForURL(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}
