// Package probe — ProxyProber implementation.
package probe

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"netsonar/internal/config"
)

// ProxyProber probes connectivity through an HTTP CONNECT proxy by
// establishing a tunnel to the target address and measuring the tunnel
// establishment time.
type ProxyProber struct{}

// Probe establishes an HTTP CONNECT tunnel through the configured proxy
// (target.ProxyName / target.ResolvedProxy) to target.Address and measures the total
// tunnel establishment duration.
//
// Preconditions:
//   - target.ResolvedProxy is populated by config loading
//   - target.Address is a valid host:port tunnel destination
//   - ctx carries the probe timeout (set by the scheduler)
//
// Postconditions:
//   - result.Success is true iff the proxy tunnel status matched the
//     configured expectation, or the tunnel was established when no
//     explicit expectation was configured
//   - result.Duration reflects wall-clock time from start to tunnel ready
//   - result.Phases contains proxy_dial, optionally proxy_tls, and proxy_connect
//   - All connections are closed before returning
//   - result.Error is non-empty when Success is false
func (p *ProxyProber) Probe(ctx context.Context, target config.TargetConfig) (result ProbeResult) {
	if target.ResolvedProxy == nil {
		result.Error = "missing resolved proxy"
		return result
	}
	proxyURL := mustProxyEndpoint("ProxyProber", target.ResolvedProxy.Endpoint)
	proxyAuthHeader, _ := target.ResolvedProxy.Credentials.BasicAuthHeader()

	// Determine the tunnel destination from target.Address.
	tunnelDest, err := parseTunnelDestination(target.Address)
	if err != nil {
		result.Error = fmt.Sprintf("invalid target address: %s", err.Error())
		return result
	}

	start := time.Now()
	tunnel, err := dialProxyTunnel(ctx, proxyURL, tunnelDest, target.ResolvedProxy.TLSSkipVerify, proxyAuthHeader, resolverFor(target.DNSResolver))
	result.Duration = time.Since(start)
	if len(tunnel.phases) > 0 {
		result.Phases = tunnel.phases
	}
	if tunnel.connectResp.Observed {
		result.ProxyConnectResponseReceived = true
		result.ProxyConnectStatusCode = tunnel.connectResp.StatusCode
	}
	if err != nil {
		if tunnel.connectResp.Observed && slices.Contains(target.ProbeOpts.ExpectedProxyConnectStatusCodes, tunnel.connectResp.StatusCode) {
			result.Success = true
			return result
		}
		result.Error = err.Error()
		return result
	}
	defer func() { _ = tunnel.conn.Close() }()

	if len(target.ProbeOpts.ExpectedProxyConnectStatusCodes) > 0 {
		if slices.Contains(target.ProbeOpts.ExpectedProxyConnectStatusCodes, tunnel.connectResp.StatusCode) {
			result.Success = true
		} else {
			result.Error = fmt.Sprintf("unexpected proxy CONNECT status %d", tunnel.connectResp.StatusCode)
		}
		return result
	}

	result.Success = true

	return result
}

// parseTunnelDestination validates a host:port suitable for an HTTP CONNECT
// request. URL syntax is intentionally rejected: CONNECT probes test tunneling
// policy for a host and port, not HTTP forwarding policy for a URL.
func parseTunnelDestination(address string) (string, error) {
	if strings.Contains(address, "://") {
		return "", fmt.Errorf("address must be host:port (URL syntax not supported for proxy_connect), got %q", address)
	}
	if _, _, splitErr := net.SplitHostPort(address); splitErr == nil {
		return address, nil
	}

	return "", fmt.Errorf("address must be host:port, got %q", address)
}
