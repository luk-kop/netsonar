// Package probe defines the Prober interface and probe implementations.
package probe

import (
	"context"
	"crypto/x509"
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

// Phase label values set in ProbeResult.Phases. Constants here are the
// single source of truth: probers use them when recording timings and the
// metrics package uses AllPhases to iterate for bounded series deletion.
const (
	PhaseDNSResolve   = "dns_resolve"
	PhaseTCPConnect   = "tcp_connect"
	PhaseTLSHandshake = "tls_handshake"
	PhaseTTFB         = "ttfb"
	PhaseTransfer     = "transfer"
	PhaseProxyDial    = "proxy_dial"
	PhaseProxyTLS     = "proxy_tls"
	PhaseProxyConnect = "proxy_connect"
)

// AllPhases lists every phase label any prober may emit. Used by the
// metrics package to delete phase series on target removal and to validate
// incoming results. Keep in sync when adding a new PhaseX constant above.
var AllPhases = []string{
	PhaseDNSResolve,
	PhaseTCPConnect,
	PhaseTLSHandshake,
	PhaseTTFB,
	PhaseTransfer,
	PhaseProxyDial,
	PhaseProxyTLS,
	PhaseProxyConnect,
}

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

	// HTTPResponseReceived is true when an HTTP response was received and
	// response metadata such as status code became observable.
	HTTPResponseReceived bool

	// CertExpiry is the earliest TLS certificate NotAfter timestamp in the
	// peer certificate chain (TLS and HTTPS probes only).
	CertExpiry time.Time

	// CertObserved is true when at least one peer certificate was observed.
	CertObserved bool

	// TLSCertificates is the peer certificate chain observed during TLS
	// probes. The metrics package exposes bounded per-cert expiry series
	// from this data without high-cardinality certificate identity labels.
	TLSCertificates []*x509.Certificate

	// PathMTU is the detected path MTU in bytes (MTU probes only).
	// -1 indicates all configured sizes failed.
	PathMTU int

	// MTUState is the operator-facing MTU state (MTU probes only):
	// ok, degraded, unreachable, or error.
	MTUState string

	// MTUDetail is the diagnostic MTU detail label (MTU probes only).
	MTUDetail string

	// PacketLoss is the ratio of lost ICMP echo replies in [0.0, 1.0]
	// (ICMP probes only).
	PacketLoss float64

	// ICMPRepliesObserved is the number of successful ICMP echo replies
	// observed during the probe. For MTU probes this includes the sanity echo
	// and any successful payload probes.
	ICMPRepliesObserved int

	// ICMPAvgRTT is the average round-trip time across successful ICMP echo
	// replies (ICMP and MTU probes). Zero when no replies were received.
	ICMPAvgRTT time.Duration

	// ICMPStddevRTT is the population standard deviation of ICMP echo RTTs
	// across successful replies. Zero when fewer than two replies were received.
	ICMPStddevRTT time.Duration

	// DNSResolveTime is the DNS resolution duration (DNS probes only).
	DNSResolveTime time.Duration

	// DNSMatchEvaluated is true when expected DNS results were configured and
	// the resolver returned a response that could be compared.
	DNSMatchEvaluated bool

	// DNSMatched is true when DNSMatchEvaluated is true and the returned
	// records match the configured expected results.
	DNSMatched bool

	// HTTPResponseTruncated is true when the HTTP probe observed a response
	// body larger than the effective transfer limit. It is not a failure by
	// itself and is only used by probe_type=http.
	HTTPResponseTruncated bool

	// HTTPTruncationEvaluated is true when the HTTP probe received a response
	// and read the body far enough to evaluate whether truncation occurred.
	HTTPTruncationEvaluated bool

	// BodyMatch is the result of body content validation
	// (HTTP body probes only).
	BodyMatch bool

	// HTTPBodyEvaluated is true when an HTTP body probe received a response
	// body and completed body-match evaluation.
	HTTPBodyEvaluated bool

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
