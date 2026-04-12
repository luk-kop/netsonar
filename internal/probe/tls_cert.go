// Package probe — TLSCertProber implementation.
package probe

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"netsonar/internal/config"
)

// TLSCertProber probes TLS certificate expiry by performing a TLS handshake
// against the target address and extracting the leaf certificate's NotAfter
// timestamp.
type TLSCertProber struct{}

// Probe executes a TLS handshake against target.Address and extracts the
// leaf certificate expiry (NotAfter) from the peer certificate chain.
//
// Preconditions:
//   - target.Address is a valid host:port string (e.g. "example.com:443")
//   - ctx carries the probe timeout (set by the scheduler)
//
// Postconditions:
//   - result.Success is true iff the TLS handshake completed and at least
//     one peer certificate was presented
//   - result.CertExpiry contains the leaf certificate's NotAfter timestamp
//   - result.Duration reflects wall-clock time from dial start to completion
//   - The TLS connection is always closed before returning
//   - result.Error is non-empty when Success is false
func (p *TLSCertProber) Probe(ctx context.Context, target config.TargetConfig) ProbeResult {
	var result ProbeResult

	host, _, err := net.SplitHostPort(target.Address)
	if err != nil {
		// If no port is specified, assume the address is just a hostname.
		// Default to port 443 for TLS.
		host = target.Address
		target.Address = net.JoinHostPort(target.Address, "443")
	}

	tlsCfg := &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: target.ProbeOpts.TLSSkipVerify,
	}

	start := time.Now()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", target.Address)
	if err != nil {
		result.Duration = time.Since(start)
		result.Error = fmt.Sprintf("tcp dial: %s", err.Error())
		return result
	}

	tlsConn := tls.Client(conn, tlsCfg)
	defer func() { _ = tlsConn.Close() }()

	if err := tlsConn.HandshakeContext(ctx); err != nil {
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
	result.CertExpiry = state.PeerCertificates[0].NotAfter

	return result
}
