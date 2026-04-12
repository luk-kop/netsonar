// Package probe defines the Prober interface and probe implementations.
package probe

import (
	"context"
	"time"

	"netsonar/internal/config"
)

const (
	MTUStateOK          = "ok"
	MTUStateDegraded    = "degraded"
	MTUStateUnreachable = "unreachable"
	MTUStateError       = "error"

	MTUDetailLargestSizeConfirmed = "largest_size_confirmed"
	MTUDetailFragmentationNeeded  = "fragmentation_needed"
	MTUDetailLargerSizesTimedOut  = "larger_sizes_timed_out"
	MTUDetailBelowMinTested       = "below_min_tested"
	MTUDetailAllSizesTimedOut     = "all_sizes_timed_out"
	MTUDetailSanityCheckFailed    = "sanity_check_failed"
	MTUDetailDestinationUnreach   = "destination_unreachable"
	MTUDetailLocalMessageTooLarge = "local_message_too_large"
	MTUDetailPermissionDenied     = "permission_denied"
	MTUDetailResolveError         = "resolve_error"
	MTUDetailInternalError        = "internal_error"
	MTUDetailInconclusive         = "inconclusive"
)

// ProbeResult holds the outcome of a single probe execution.
type ProbeResult struct {
	// Success is true when the probe completed without error and any
	// probe-specific validation (e.g. expected status codes) passed.
	Success bool

	// Duration is the wall-clock time of the entire probe execution.
	Duration time.Duration

	// Phases contains per-phase timing for probes that expose a sub-phase
	// breakdown. HTTP uses dns_resolve, tcp_connect, tls_handshake, ttfb,
	// and transfer. Proxy uses proxy_dial, proxy_tls, and proxy_connect.
	// Nil for probe types without meaningful sub-phases.
	Phases map[string]time.Duration

	// StatusCode is the HTTP response status code (HTTP probes only).
	StatusCode int

	// CertExpiry is the TLS leaf certificate NotAfter timestamp
	// (TLS and HTTPS probes only).
	CertExpiry time.Time

	// PathMTU is the detected path MTU in bytes (MTU probes only).
	// -1 indicates all configured sizes failed.
	PathMTU int

	// MTUState is the operator-facing MTU state (MTU probes only):
	// ok, degraded, unreachable, or error.
	MTUState string

	// MTUDetail is the diagnostic MTU detail label (MTU probes only).
	MTUDetail string

	// MTU diagnostic counters accumulated during a single MTU probe execution.
	MTUFragNeededCount int
	MTUTimeoutCount    int
	MTURetryCount      int
	MTULocalErrorCount int

	// PacketLoss is the ratio of lost ICMP echo replies in [0.0, 1.0]
	// (ICMP probes only).
	PacketLoss float64

	// ICMPAvgRTT is the average round-trip time across successful ICMP echo
	// replies (ICMP probes only). Zero when no replies were received.
	ICMPAvgRTT time.Duration

	// HopCount is the TTL / hop count from the ICMP echo reply
	// (ICMP probes only).
	HopCount int

	// DNSResolveTime is the DNS resolution duration (DNS probes only).
	DNSResolveTime time.Duration

	// BodyMatch is the result of body content validation
	// (HTTP body probes only).
	BodyMatch bool

	// Error contains a descriptive message when Success is false.
	// Empty when Success is true.
	Error string
}

// Prober is the interface that all probe types implement.
type Prober interface {
	// Probe executes a single probe against the target and returns the
	// result. Implementations must respect ctx cancellation and the
	// target's configured timeout. All network resources (sockets,
	// connections, response bodies) must be closed before returning.
	Probe(ctx context.Context, target config.TargetConfig) ProbeResult
}
