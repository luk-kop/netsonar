// Package probe — TCPProber implementation.
package probe

import (
	"context"
	"net"
	"time"

	"netsonar/internal/config"
)

// TCPProber probes TCP connectivity by dialing the target address.
type TCPProber struct{}

// Probe executes a TCP dial against target.Address with the context timeout.
// It measures the connection establishment duration and ensures the connection
// is closed on all code paths (success, failure, timeout, cancellation).
//
// Preconditions:
//   - target.Address is a valid host:port string
//   - ctx carries the probe timeout (set by the scheduler)
//
// Postconditions:
//   - result.Success is true iff the TCP connection was established
//   - result.Duration reflects wall-clock time from dial start to completion
//   - The TCP connection is always closed before returning
//   - result.Error is non-empty when Success is false
func (p *TCPProber) Probe(ctx context.Context, target config.TargetConfig) ProbeResult {
	var result ProbeResult

	start := time.Now()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", target.Address)
	result.Duration = time.Since(start)

	if err != nil {
		result.Error = err.Error()
		return result
	}
	_ = conn.Close()

	result.Success = true

	return result
}
