// Package probe defines the Prober interface and probe implementations.
package probe

import (
	"context"
	"crypto/x509"
	"fmt"
	"net"
	"strings"
	"sync"
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

	happyEyeballsFallbackDelay = 300 * time.Millisecond
)

// Phase label values set in ProbeResult.Phases. Constants here are the
// single source of truth: probers use them when recording timings and the
// metrics package uses AllPhases to iterate for bounded series deletion.
const (
	PhaseDNSResolve   = "dns_resolve"
	PhaseTCPConnect   = "tcp_connect"
	PhaseTLSHandshake = "tls_handshake"
	PhaseRequestWrite = "request_write"
	PhaseTTFB         = "ttfb"
	PhaseTransfer     = "transfer"
	PhaseProxyDial    = "proxy_dial"
	PhaseProxyTLS     = "proxy_tls"
	PhaseProxyConnect = "proxy_connect"
)

// AllPhases lists every phase label any prober may emit. Used by the
// metrics package to delete phase series on target removal and to validate
// incoming results. Keep in sync when adding a new PhaseX constant above.
//
// Phase emission follows one rule across probers: a phase is exported when
// execution reached that phase and elapsed time for it was observed, even if
// the sub-operation failed. A phase is absent only when execution never
// reached that phase.
var AllPhases = []string{
	PhaseDNSResolve,
	PhaseTCPConnect,
	PhaseTLSHandshake,
	PhaseRequestWrite,
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
	// breakdown. TCP and direct TLS cert emit dns_resolve for hostname
	// targets, plus tcp_connect; direct TLS cert also emits tls_handshake.
	// HTTP uses dns_resolve, tcp_connect, tls_handshake, request_write,
	// ttfb, and transfer. Proxy and TLS cert via proxy use proxy_dial,
	// proxy_tls, and proxy_connect; TLS cert via proxy also adds the target
	// tls_handshake.
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
	// body larger than the effective response body limit. It is not a failure
	// by itself and is only used by probe_type=http.
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

// safeSub returns a non-negative end - start duration.
func safeSub(end, start time.Time) time.Duration {
	d := end.Sub(start)
	if d < 0 {
		return 0
	}
	return d
}

func addObservedPhase(phases map[string]time.Duration, phase string, end, start time.Time) {
	if start.IsZero() || end.IsZero() {
		return
	}
	phases[phase] = safeSub(end, start)
}

type splitDialResult struct {
	conn     net.Conn
	dnsStart time.Time
	dnsEnd   time.Time
	tcpStart time.Time
	tcpEnd   time.Time
}

func dialTCPWithSplitPhases(ctx context.Context, address string) (splitDialResult, error) {
	var result splitDialResult

	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return result, err
	}

	var dialTargets []string
	if isLiteralIPHost(host) {
		dialTargets = []string{address}
	} else {
		result.dnsStart = time.Now()
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		result.dnsEnd = time.Now()
		if err != nil {
			return result, fmt.Errorf("dns resolve: %w", err)
		}
		if len(ips) == 0 {
			return result, fmt.Errorf("dns resolve: no results returned")
		}
		dialTargets = joinDialTargets(ips, port)
	}

	result.tcpStart = time.Now()
	conn, err := dialTCPHappyEyeballs(ctx, dialTargets)
	result.tcpEnd = time.Now()
	if err != nil {
		return result, err
	}
	result.conn = conn
	return result, nil
}

func isLiteralIPHost(host string) bool {
	if i := strings.LastIndex(host, "%"); i >= 0 {
		host = host[:i]
	}
	return net.ParseIP(host) != nil
}

func joinDialTargets(ips []net.IPAddr, port string) []string {
	targets := make([]string, 0, len(ips))
	for _, ip := range ips {
		targets = append(targets, net.JoinHostPort(ip.String(), port))
	}
	return targets
}

func dialTCPHappyEyeballs(ctx context.Context, dialTargets []string) (net.Conn, error) {
	if len(dialTargets) == 0 {
		return nil, fmt.Errorf("no dial targets")
	}

	primaryTargets, fallbackTargets := splitDialFamilies(dialTargets)
	if len(fallbackTargets) == 0 {
		return dialTCPAddresses(ctx, primaryTargets)
	}

	dialCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type dialOutcome struct {
		conn net.Conn
		err  error
	}

	results := make(chan dialOutcome, 2)
	var wg sync.WaitGroup
	startWorker := func(targets []string, delay time.Duration) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if delay > 0 {
				timer := time.NewTimer(delay)
				defer timer.Stop()
				select {
				case <-dialCtx.Done():
					results <- dialOutcome{err: dialCtx.Err()}
					return
				case <-timer.C:
				}
			}
			conn, err := dialTCPAddresses(dialCtx, targets)
			results <- dialOutcome{conn: conn, err: err}
		}()
	}

	startWorker(primaryTargets, 0)
	startWorker(fallbackTargets, happyEyeballsFallbackDelay)

	var lastErr error
	for i := 0; i < 2; i++ {
		outcome := <-results
		if outcome.err == nil {
			cancel()
			go func(winner net.Conn) {
				wg.Wait()
				close(results)
				for loser := range results {
					if loser.conn != nil && loser.conn != winner {
						_ = loser.conn.Close()
					}
				}
			}(outcome.conn)
			return outcome.conn, nil
		}
		lastErr = outcome.err
	}

	wg.Wait()
	return nil, lastErr
}

func dialTCPAddresses(ctx context.Context, targets []string) (net.Conn, error) {
	var (
		d       net.Dialer
		lastErr error
	)
	for _, target := range targets {
		conn, err := d.DialContext(ctx, "tcp", target)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func splitDialFamilies(dialTargets []string) (primary []string, fallback []string) {
	if len(dialTargets) == 0 {
		return nil, nil
	}

	var ipv4Targets []string
	var ipv6Targets []string
	for _, target := range dialTargets {
		host, _, err := net.SplitHostPort(target)
		if err != nil {
			ipv4Targets = append(ipv4Targets, target)
			continue
		}
		if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
			ipv6Targets = append(ipv6Targets, target)
			continue
		}
		ipv4Targets = append(ipv4Targets, target)
	}

	if len(ipv4Targets) == 0 || len(ipv6Targets) == 0 {
		return dialTargets, nil
	}

	firstHost, _, err := net.SplitHostPort(dialTargets[0])
	if err == nil {
		if ip := net.ParseIP(firstHost); ip != nil && ip.To4() == nil {
			return ipv6Targets, ipv4Targets
		}
	}
	return ipv4Targets, ipv6Targets
}
