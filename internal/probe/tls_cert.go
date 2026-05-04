// Package probe — TLSCertProber implementation.
package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"time"

	"netsonar/internal/config"
	"netsonar/internal/proxyurl"
)

// TLSCertProber probes TLS certificate expiry by performing a TLS handshake
// against the target address and extracting the peer certificate chain expiry.
type TLSCertProber struct{}

// Probe executes a TLS handshake against target.Address and extracts the
// earliest certificate expiry (NotAfter) from the peer certificate chain.
//
// Preconditions:
//   - target.Address is a valid host:port string (e.g. "example.com:443")
//     or a bare host, which defaults to port 443
//   - ctx carries the probe timeout (set by the scheduler)
//
// Postconditions:
//   - result.Success is true iff the TLS handshake completed and at least
//     one peer certificate was presented
//   - result.CertExpiry contains the earliest peer certificate NotAfter timestamp
//   - result.Duration reflects wall-clock time from dial start to completion
//   - The TLS connection is always closed before returning
//   - result.Error is non-empty when Success is false
func (p *TLSCertProber) Probe(ctx context.Context, target config.TargetConfig) ProbeResult {
	host, addr := tlsCertHostPort(target.Address)
	tlsCfg := &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: target.ProbeOpts.TLSSkipVerify,
	}

	if target.ProbeOpts.ProxyURL != "" {
		return p.probeViaProxy(ctx, target.ProbeOpts.ProxyURL, addr, tlsCfg)
	}

	return p.probeDirect(ctx, addr, tlsCfg)
}

func tlsCertHostPort(address string) (host, addr string) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		// tls_cert intentionally does not use parseTunnelDestination: proxy
		// probes accept URLs, while tls_cert keeps its bare-host => :443
		// contract for both direct and proxy paths.
		host = address
		addr = net.JoinHostPort(address, "443")
		return host, addr
	}

	return host, address
}

func (p *TLSCertProber) probeDirect(ctx context.Context, addr string, tlsCfg *tls.Config) ProbeResult {
	var result ProbeResult
	start := time.Now()
	phases := make(map[string]time.Duration, 3)

	dialResult, err := dialTCPWithSplitPhases(ctx, addr)
	addObservedPhase(phases, PhaseDNSResolve, dialResult.dnsEnd, dialResult.dnsStart)
	addObservedPhase(phases, PhaseTCPConnect, dialResult.tcpEnd, dialResult.tcpStart)
	if len(phases) > 0 {
		result.Phases = phases
	}
	if err != nil {
		result.Duration = time.Since(start)
		result.Error = fmt.Sprintf("tcp dial: %s", err.Error())
		return result
	}

	tlsConn := tls.Client(dialResult.conn, tlsCfg)
	defer func() { _ = tlsConn.Close() }()

	tlsStart := time.Now()
	err = tlsConn.HandshakeContext(ctx)
	tlsEnd := time.Now()
	if result.Phases == nil {
		result.Phases = make(map[string]time.Duration, 1)
	}
	addObservedPhase(result.Phases, PhaseTLSHandshake, tlsEnd, tlsStart)
	if err != nil {
		result.Duration = time.Since(start)
		result.Error = fmt.Sprintf("tls handshake: %s", err.Error())
		return result
	}

	result.Duration = time.Since(start)

	state := tlsConn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		result.Error = "tls handshake: no peer certificates presented"
		return result
	}

	result.Success = true
	setTLSCertificateResult(&result, state.PeerCertificates)

	return result
}

func (p *TLSCertProber) probeViaProxy(ctx context.Context, rawProxyURL, addr string, tlsCfg *tls.Config) ProbeResult {
	var result ProbeResult

	proxyURL, err := proxyurl.Parse(rawProxyURL)
	if err != nil {
		result.Error = fmt.Sprintf("invalid proxy_url: %s", err.Error())
		return result
	}

	start := time.Now()
	conn, phases, connectResp, err := dialProxyTunnel(ctx, proxyURL, addr)
	if len(phases) > 0 {
		result.Phases = phases
	}
	if connectResp.Observed {
		result.ProxyConnectResponseReceived = true
		result.ProxyConnectStatusCode = connectResp.StatusCode
	}
	if err != nil {
		result.Duration = time.Since(start)
		result.Error = err.Error()
		return result
	}

	tlsConn := tls.Client(conn, tlsCfg)
	defer func() { _ = tlsConn.Close() }()

	tlsStart := time.Now()
	if result.Phases == nil {
		result.Phases = make(map[string]time.Duration, 1)
	}
	err = tlsConn.HandshakeContext(ctx)
	tlsEnd := time.Now()
	addObservedPhase(result.Phases, PhaseTLSHandshake, tlsEnd, tlsStart)
	if err != nil {
		result.Duration = time.Since(start)
		result.Error = fmt.Sprintf("tls handshake: %s", err.Error())
		return result
	}
	result.Duration = time.Since(start)

	state := tlsConn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		result.Error = "tls handshake: no peer certificates presented"
		return result
	}

	result.Success = true
	setTLSCertificateResult(&result, state.PeerCertificates)

	return result
}

func setTLSCertificateResult(result *ProbeResult, certs []*x509.Certificate) {
	result.TLSCertificates = certs
	if len(certs) == 0 {
		result.CertObserved = false
		result.CertExpiry = time.Time{}
		return
	}
	result.CertObserved = true
	earliest := certs[0].NotAfter
	for _, cert := range certs[1:] {
		if cert.NotAfter.Before(earliest) {
			earliest = cert.NotAfter
		}
	}
	result.CertExpiry = earliest
}
