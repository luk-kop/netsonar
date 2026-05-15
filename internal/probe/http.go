// Package probe — HTTPProber implementation.
package probe

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"slices"
	"sync"
	"time"

	"netsonar/internal/config"
)

// defaultResponseBodyLimit is the default number of response body bytes the HTTP
// prober reads during the transfer phase. The body is discarded — this limit
// only caps bandwidth and time spent on large or streaming responses. The
// probe succeeds regardless of body size; status code determines success.
const defaultResponseBodyLimit int64 = 1 << 20 // 1 MiB

var (
	httpRequestBodyPatternOnce sync.Once
	httpRequestBodyPattern     []byte
)

func requestBodyReader(n int64) io.Reader {
	httpRequestBodyPatternOnce.Do(func() {
		httpRequestBodyPattern = bytes.Repeat([]byte("n"), int(config.MaxHTTPRequestBodyBytes))
	})
	return bytes.NewReader(httpRequestBodyPattern[:n])
}

// HTTPProber probes HTTP/HTTPS endpoints with per-phase timing breakdown
// using net/http/httptrace.ClientTrace.
type HTTPProber struct {
	// client is the HTTP client used for probing. It is configured with
	// DisableKeepAlives to ensure each probe creates a fresh connection
	// for accurate connection-establishment measurements.
	client *http.Client
	// tlsSkipVerify is retained so the proxy-path code can re-derive a
	// tls.Config per probe without reading back into the transport.
	tlsSkipVerify bool
	// followRedirects is retained so the proxy-path code can match the
	// direct-path CheckRedirect contract (follow 3xx like http.Client's
	// default, or stop at first response when false).
	followRedirects bool
	// proxyURL, when non-nil, routes every probe through an explicit
	// proxy-aware code path that measures proxy phases (proxy_dial,
	// proxy_tls, proxy_connect) instead of hiding them inside Go's
	// default http.Transport.
	proxyURL *url.URL
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

	var parsedProxy *url.URL
	if proxyURL != "" {
		parsedProxy = mustProxyURL("NewHTTPProber", proxyURL)
	}

	return &HTTPProber{
		client:          client,
		tlsSkipVerify:   tlsSkipVerify,
		followRedirects: followRedirects,
		proxyURL:        parsedProxy,
	}
}

// Probe executes a full HTTP request against target.Address with httptrace
// instrumentation to capture per-phase timing (dns_resolve, tcp_connect,
// tls_handshake, request_write, ttfb, transfer).
//
// Preconditions:
//   - target.Address is a valid HTTP or HTTPS URL
//   - ctx carries the probe timeout (set by the scheduler)
//
// Postconditions:
//   - result.Phases contains dns_resolve, tcp_connect, tls_handshake,
//     request_write, ttfb, and transfer durations (tls_handshake is zero for
//     plain HTTP)
//   - result.StatusCode contains the HTTP response status code
//   - result.CertExpiry is populated for HTTPS targets with valid TLS state
//   - If expected_status_codes is non-empty, Success reflects whether the
//     response code is in the list; if empty, any response is a success
//   - The response body is read up to defaultResponseBodyLimit (1 MiB) and closed
//   - result.Error is non-empty when Success is false
func (p *HTTPProber) Probe(ctx context.Context, target config.TargetConfig) ProbeResult {
	if p.proxyURL != nil {
		return p.probeViaProxy(ctx, target)
	}
	return p.probeDirect(ctx, target)
}

func (p *HTTPProber) probeDirect(ctx context.Context, target config.TargetConfig) ProbeResult {
	var result ProbeResult

	method := target.ProbeOpts.Method
	if method == "" {
		method = http.MethodGet
	}

	// Phase timing anchors. Zero-valued times indicate the phase did not
	// occur (e.g. tls_handshake on plain HTTP). httptrace callbacks may fire
	// concurrently from net/http transport goroutines, so the timings are
	// guarded by a mutex and read via snapshot after client.Do returns.
	timings := &httpTraceTimings{}

	var body io.Reader
	if target.ProbeOpts.RequestBodyBytes > 0 {
		body = requestBodyReader(target.ProbeOpts.RequestBodyBytes)
	}
	reqCtx := httptrace.WithClientTrace(ctx, timings.trace())

	req, err := http.NewRequestWithContext(
		reqCtx,
		method,
		target.Address,
		body,
	)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	// Apply custom headers from probe options.
	for k, v := range target.ProbeOpts.Headers {
		req.Header.Set(k, v)
	}
	if target.ProbeOpts.RequestBodyBytes > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/octet-stream")
	}

	start := time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		result.Duration = time.Since(start)
		result.Error = err.Error()
		result.Phases = buildPhasesFromSnapshot(timings.snapshot(), time.Time{})
		return result
	}
	defer func() { _ = resp.Body.Close() }()

	result.HTTPResponseReceived = true
	result.StatusCode = resp.StatusCode
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		setTLSCertificateResult(&result, resp.TLS.PeerCertificates)
	}

	effectiveLimit := target.ProbeOpts.ResponseBodyLimitBytes
	if effectiveLimit <= 0 {
		effectiveLimit = defaultResponseBodyLimit
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
		result.Phases = buildPhasesFromSnapshot(timings.snapshot(), transferEnd)
		return result
	}
	result.HTTPTruncationEvaluated = true
	if bytesRead > effectiveLimit {
		result.HTTPResponseTruncated = true
	}
	transferEnd := time.Now()

	result.Duration = transferEnd.Sub(start)
	result.Phases = buildPhasesFromSnapshot(timings.snapshot(), transferEnd)

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

// probeViaProxy executes an HTTP probe routed through p.proxyURL using an
// explicit proxy-aware code path. This is required so per-phase timing
// includes proxy_dial, optional proxy_tls, and for HTTPS targets
// proxy_connect and tls_handshake, rather than hiding CONNECT latency
// inside Go's default http.Transport.
//
// When p.followRedirects is true, 3xx responses trigger a follow-up
// exchange through the same proxy (up to proxyRedirectLimit hops). The
// reported Phases reflect the final hop only, matching the direct path
// where httptrace fires on every hop and the last hop wins. When
// p.followRedirects is false, a 3xx response is returned as-is and
// success is evaluated against expected_status_codes.
func (p *HTTPProber) probeViaProxy(ctx context.Context, target config.TargetConfig) ProbeResult {
	var result ProbeResult
	start := time.Now()

	currentURL, err := url.Parse(target.Address)
	if err != nil {
		result.Duration = time.Since(start)
		result.Error = fmt.Sprintf("parse target URL: %s", err.Error())
		return result
	}

	method := target.ProbeOpts.Method
	if method == "" {
		method = http.MethodGet
	}

	headers := target.ProbeOpts.Headers
	var requestBody io.Reader
	if target.ProbeOpts.RequestBodyBytes > 0 {
		requestBody = requestBodyReader(target.ProbeOpts.RequestBodyBytes)
		if headers == nil || headers["Content-Type"] == "" {
			merged := make(map[string]string, len(headers)+1)
			for k, v := range headers {
				merged[k] = v
			}
			merged["Content-Type"] = "application/octet-stream"
			headers = merged
		}
	}

	var exchange proxyHTTPExchangeResult
	currentMethod := method
	currentHeaders := headers
	currentBody := requestBody
	hop := 0
	for {
		exchange = executeProxyHTTPExchange(ctx, p.proxyURL, currentURL, currentMethod, currentHeaders, currentBody, p.tlsSkipVerify)

		if exchange.connectResp.Observed && !result.ProxyConnectResponseReceived {
			// Preserve the first observed CONNECT status — subsequent
			// hops may not go through CONNECT (e.g. redirect from
			// https:// to http://) and overwriting with zero would hide
			// the initial CONNECT diagnostic.
			result.ProxyConnectResponseReceived = true
			result.ProxyConnectStatusCode = exchange.connectResp.StatusCode
		}

		if exchange.err != nil {
			result.Duration = time.Since(start)
			result.Error = exchange.err.Error()
			result.Phases = buildProxyPhases(exchange.phases, exchange.setupEnd,
				exchange.wroteRequest, exchange.gotFirstByte, time.Time{})
			return result
		}

		if !p.followRedirects || !isRedirect(exchange.resp.StatusCode) {
			break
		}
		next, shouldFollow, err := resolveRedirect(currentURL, exchange.resp)
		if !shouldFollow {
			break
		}
		_ = exchange.resp.Body.Close()
		if err != nil {
			result.Duration = time.Since(start)
			result.Error = fmt.Sprintf("following redirect: %s", err.Error())
			return result
		}
		if hop >= proxyRedirectLimit-1 {
			result.Duration = time.Since(start)
			result.Error = "stopped after 10 redirects"
			result.Phases = buildProxyPhases(exchange.phases, exchange.setupEnd,
				exchange.wroteRequest, exchange.gotFirstByte, time.Time{})
			return result
		}
		currentMethod, currentBody, currentHeaders = redirectedRequest(
			currentMethod,
			exchange.resp.StatusCode,
			headers,
			target.ProbeOpts.RequestBodyBytes,
		)
		currentURL = next
		hop++
	}

	resp := exchange.resp
	defer func() { _ = resp.Body.Close() }()

	result.HTTPResponseReceived = true
	result.StatusCode = resp.StatusCode
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		setTLSCertificateResult(&result, resp.TLS.PeerCertificates)
	}

	effectiveLimit := target.ProbeOpts.ResponseBodyLimitBytes
	if effectiveLimit <= 0 {
		effectiveLimit = defaultResponseBodyLimit
	}

	bytesRead, err := io.Copy(io.Discard, io.LimitReader(resp.Body, effectiveLimit+1))
	transferEnd := time.Now()
	if err != nil {
		result.Duration = transferEnd.Sub(start)
		result.Error = fmt.Sprintf("reading response body: %s", err.Error())
		result.Phases = buildProxyPhases(exchange.phases, exchange.setupEnd,
			exchange.wroteRequest, exchange.gotFirstByte, transferEnd)
		return result
	}
	result.HTTPTruncationEvaluated = true
	if bytesRead > effectiveLimit {
		result.HTTPResponseTruncated = true
	}

	result.Duration = transferEnd.Sub(start)
	result.Phases = buildProxyPhases(exchange.phases, exchange.setupEnd,
		exchange.wroteRequest, exchange.gotFirstByte, transferEnd)

	if len(target.ProbeOpts.ExpectedStatusCodes) > 0 {
		if slices.Contains(target.ProbeOpts.ExpectedStatusCodes, resp.StatusCode) {
			result.Success = true
		} else {
			result.Error = "unexpected status code"
		}
	} else {
		result.Success = true
	}

	return result
}

// proxyRedirectLimit caps redirect following on the proxy path. Go's
// default http.Client uses 10; we match that for parity with direct-path
// behavior.
const proxyRedirectLimit = 10

func isRedirect(code int) bool {
	switch code {
	case http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect:
		return true
	}
	return false
}

// resolveRedirect resolves a 3xx response's Location header against the
// previous request URL. An empty Location means "do not follow"; Go's
// net/http client treats such 3xx responses as final responses.
func resolveRedirect(base *url.URL, resp *http.Response) (*url.URL, bool, error) {
	loc := resp.Header.Get("Location")
	if loc == "" {
		return nil, false, nil
	}
	parsed, err := base.Parse(loc)
	if err != nil {
		return nil, true, fmt.Errorf("parse Location %q: %s", loc, err.Error())
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, true, fmt.Errorf("unsupported redirect scheme %q", parsed.Scheme)
	}
	return parsed, true, nil
}

func redirectedRequest(
	currentMethod string,
	statusCode int,
	initialHeaders map[string]string,
	requestBodyBytes int64,
) (string, io.Reader, map[string]string) {
	switch statusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther:
		if currentMethod != http.MethodGet && currentMethod != http.MethodHead {
			return http.MethodGet, nil, stripRequestBodyHeaders(initialHeaders)
		}
		return currentMethod, nil, stripRequestBodyHeaders(initialHeaders)
	case http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		if currentMethod == http.MethodPost && requestBodyBytes > 0 {
			return currentMethod, requestBodyReader(requestBodyBytes), initialHeaders
		}
		if currentMethod == http.MethodGet || currentMethod == http.MethodHead {
			return currentMethod, nil, stripRequestBodyHeaders(initialHeaders)
		}
		return currentMethod, nil, initialHeaders
	}
	return currentMethod, nil, initialHeaders
}

// stripRequestBodyHeaders returns a copy of headers with body-specific
// entries removed. Redirects are followed with GET and no body, so any
// Content-Type/Content-Length originally added for a POST should not be
// carried along.
func stripRequestBodyHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return headers
	}
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		switch http.CanonicalHeaderKey(k) {
		case "Content-Type", "Content-Length":
			continue
		}
		out[k] = v
	}
	return out
}

// buildPhases constructs the phase duration map from the recorded timestamps.
// Unobserved phases are omitted rather than exported as zero placeholders.
//
// request_write is anchored at the later of connectEnd and tlsEnd so that for
// HTTPS the TLS handshake is not double-counted. TTFB is anchored at
// WroteRequest so request upload time is not attributed to server response
// latency. Phases are non-overlapping and sum to total duration (within
// scheduling jitter).
func buildPhases(
	dnsStart, dnsEnd,
	connectStart, connectEnd,
	tlsStart, tlsEnd,
	wroteRequest,
	gotFirstByte, transferEnd time.Time,
) map[string]time.Duration {
	phases := make(map[string]time.Duration, 6)
	requestReady := laterOf(tlsEnd, connectEnd)
	ttfbStart := wroteRequest
	if ttfbStart.IsZero() {
		ttfbStart = requestReady
	}
	addObservedPhase(phases, PhaseDNSResolve, dnsEnd, dnsStart)
	addObservedPhase(phases, PhaseTCPConnect, connectEnd, connectStart)
	addObservedPhase(phases, PhaseTLSHandshake, tlsEnd, tlsStart)
	addObservedPhase(phases, PhaseRequestWrite, wroteRequest, requestReady)
	addObservedPhase(phases, PhaseTTFB, gotFirstByte, ttfbStart)
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
