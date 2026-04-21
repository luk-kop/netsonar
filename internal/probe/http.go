// Package probe — HTTPProber implementation.
package probe

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"slices"
	"time"

	"netsonar/internal/config"
)

// maxHTTPTransferBytes is the maximum number of response body bytes the HTTP
// prober reads during the transfer phase. The body is discarded — this limit
// only caps bandwidth and time spent on large or streaming responses. The
// probe succeeds regardless of body size; status code determines success.
const maxHTTPTransferBytes int64 = 1 << 20 // 1 MiB

// HTTPProber probes HTTP/HTTPS endpoints with per-phase timing breakdown
// using net/http/httptrace.ClientTrace.
type HTTPProber struct {
	// client is the HTTP client used for probing. It is configured with
	// DisableKeepAlives to ensure each probe creates a fresh connection
	// for accurate connection-establishment measurements.
	client *http.Client
}

// NewHTTPProber creates an HTTPProber with a transport configured for
// single-use connections and the given TLS/redirect settings. If proxyURL
// is non-empty, all requests are routed through the specified HTTP proxy.
func NewHTTPProber(tlsSkipVerify bool, followRedirects bool, proxyURL string) *HTTPProber {
	transport := &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: tlsSkipVerify,
		},
	}

	if proxyURL != "" {
		transport.Proxy = http.ProxyURL(mustProxyURL("NewHTTPProber", proxyURL))
	}

	client := &http.Client{
		Transport: transport,
		// Timeout is enforced via context, not the client.
		Timeout: 0,
	}

	if !followRedirects {
		client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	return &HTTPProber{client: client}
}

// Probe executes a full HTTP request against target.Address with httptrace
// instrumentation to capture per-phase timing (dns_resolve, tcp_connect,
// tls_handshake, ttfb, transfer).
//
// Preconditions:
//   - target.Address is a valid HTTP or HTTPS URL
//   - ctx carries the probe timeout (set by the scheduler)
//
// Postconditions:
//   - result.Phases contains dns_resolve, tcp_connect, tls_handshake, ttfb,
//     and transfer durations (tls_handshake is zero for plain HTTP)
//   - result.StatusCode contains the HTTP response status code
//   - result.CertExpiry is populated for HTTPS targets with valid TLS state
//   - If expected_status_codes is non-empty, Success reflects whether the
//     response code is in the list; if empty, any response is a success
//   - The response body is read up to maxHTTPTransferBytes (1 MiB) and closed
//   - result.Error is non-empty when Success is false
func (p *HTTPProber) Probe(ctx context.Context, target config.TargetConfig) ProbeResult {
	var result ProbeResult

	method := target.ProbeOpts.Method
	if method == "" {
		method = http.MethodGet
	}

	// Phase timing anchors. Zero-valued times indicate the phase did not
	// occur (e.g. tls_handshake on plain HTTP).
	var (
		dnsStart, dnsEnd         time.Time
		connectStart, connectEnd time.Time
		tlsStart, tlsEnd         time.Time
		gotFirstByte             time.Time
	)

	trace := &httptrace.ClientTrace{
		DNSStart:             func(_ httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone:              func(_ httptrace.DNSDoneInfo) { dnsEnd = time.Now() },
		ConnectStart:         func(_, _ string) { connectStart = time.Now() },
		ConnectDone:          func(_, _ string, _ error) { connectEnd = time.Now() },
		TLSHandshakeStart:    func() { tlsStart = time.Now() },
		TLSHandshakeDone:     func(_ tls.ConnectionState, _ error) { tlsEnd = time.Now() },
		GotFirstResponseByte: func() { gotFirstByte = time.Now() },
	}

	req, err := http.NewRequestWithContext(
		httptrace.WithClientTrace(ctx, trace),
		method,
		target.Address,
		nil,
	)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	// Apply custom headers from probe options.
	for k, v := range target.ProbeOpts.Headers {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		result.Duration = time.Since(start)
		result.Error = err.Error()
		result.Phases = buildPhases(dnsStart, dnsEnd, connectStart, connectEnd,
			tlsStart, tlsEnd, gotFirstByte, time.Time{})
		return result
	}
	defer func() { _ = resp.Body.Close() }()

	result.HTTPResponseReceived = true
	result.StatusCode = resp.StatusCode
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		setTLSCertificateResult(&result, resp.TLS.PeerCertificates)
	}

	effectiveLimit := target.ProbeOpts.MaxTransferBytes
	if effectiveLimit <= 0 {
		effectiveLimit = maxHTTPTransferBytes
	}

	// Read up to effectiveLimit+1 bytes so truncation is observable without
	// draining arbitrarily large or streaming responses.
	// The body is discarded; the limit prevents probe goroutines from being
	// held on large or streaming responses until the context timeout.
	bytesRead, err := io.Copy(io.Discard, io.LimitReader(resp.Body, effectiveLimit+1))
	if err != nil {
		transferEnd := time.Now()
		result.Duration = transferEnd.Sub(start)
		result.Error = fmt.Sprintf("reading response body: %s", err.Error())
		result.Phases = buildPhases(dnsStart, dnsEnd, connectStart, connectEnd,
			tlsStart, tlsEnd, gotFirstByte, transferEnd)
		return result
	}
	result.HTTPTruncationEvaluated = true
	if bytesRead > effectiveLimit {
		result.HTTPResponseTruncated = true
	}
	transferEnd := time.Now()

	result.Duration = transferEnd.Sub(start)
	result.Phases = buildPhases(dnsStart, dnsEnd, connectStart, connectEnd,
		tlsStart, tlsEnd, gotFirstByte, transferEnd)

	// Determine success based on expected status codes.
	if len(target.ProbeOpts.ExpectedStatusCodes) > 0 {
		if slices.Contains(target.ProbeOpts.ExpectedStatusCodes, resp.StatusCode) {
			result.Success = true
		} else {
			result.Error = "unexpected status code"
		}
	} else {
		// Empty expected list means any response is a success.
		result.Success = true
	}

	return result
}

// buildPhases constructs the phase duration map from the recorded timestamps.
// Unobserved phases are omitted rather than exported as zero placeholders.
//
// TTFB is anchored at the later of connectEnd and tlsEnd so that for HTTPS
// the TLS handshake is not double-counted in both tls_handshake and ttfb.
// Phases are non-overlapping and sum to total duration (within scheduling
// jitter), matching the W3C Navigation Timing / Blackbox Exporter convention.
func buildPhases(
	dnsStart, dnsEnd,
	connectStart, connectEnd,
	tlsStart, tlsEnd,
	gotFirstByte, transferEnd time.Time,
) map[string]time.Duration {
	phases := make(map[string]time.Duration, 5)
	addObservedPhase(phases, PhaseDNSResolve, dnsEnd, dnsStart)
	addObservedPhase(phases, PhaseTCPConnect, connectEnd, connectStart)
	addObservedPhase(phases, PhaseTLSHandshake, tlsEnd, tlsStart)
	addObservedPhase(phases, PhaseTTFB, gotFirstByte, laterOf(tlsEnd, connectEnd))
	addObservedPhase(phases, PhaseTransfer, transferEnd, gotFirstByte)
	return phases
}

// laterOf returns the later of two timestamps. A zero time.Time compares
// before any real timestamp, so laterOf(zero, t) == t — this lets callers
// pass a zero tlsEnd for plain HTTP and still get connectEnd as the anchor.
func laterOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
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
