// Package probe — HTTPBodyProber implementation.
package probe

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"netsonar/internal/config"
)

// HTTPBodyProber probes an HTTP endpoint and validates the response body
// against a configured regex or substring pattern.
type HTTPBodyProber struct {
	client                 *http.Client
	compiledBodyMatchRegex *regexp.Regexp
	regexCompileErr        error
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

// maxHTTPBodyBytes is the maximum response body size the prober will read.
// Responses larger than this fail the probe to prevent unbounded memory use.
const maxHTTPBodyBytes int64 = 1 << 20 // 1 MiB

// NewHTTPBodyProber creates an HTTPBodyProber with a transport configured
// for single-use connections and the given TLS/redirect settings. If proxyURL
// is non-empty, all requests are routed through the specified HTTP proxy.
// bodyMatchRegex is compiled once at construction time and reused for every
// probe execution.
func NewHTTPBodyProber(tlsSkipVerify bool, followRedirects bool, proxyURL string, bodyMatchRegex string) *HTTPBodyProber {
	transport := &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: tlsSkipVerify,
		},
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   0, // Enforced via context.
	}

	if !followRedirects {
		client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	var compiledRegex *regexp.Regexp
	var regexErr error
	if bodyMatchRegex != "" {
		compiledRegex, regexErr = regexp.Compile(bodyMatchRegex)
	}

	var parsedProxy *url.URL
	if proxyURL != "" {
		parsedProxy = mustProxyURL("NewHTTPBodyProber", proxyURL)
	}

	return &HTTPBodyProber{
		client:                 client,
		compiledBodyMatchRegex: compiledRegex,
		regexCompileErr:        regexErr,
		tlsSkipVerify:          tlsSkipVerify,
		followRedirects:        followRedirects,
		proxyURL:               parsedProxy,
	}
}

// Probe executes an HTTP request against target.Address, reads the response
// body, and validates it against the configured body_match_regex or
// body_match_string pattern.
//
// Preconditions:
//   - target.Address is a valid HTTP or HTTPS URL
//   - ctx carries the probe timeout (set by the scheduler)
//   - At least one of body_match_regex or body_match_string is configured
//
// Postconditions:
//   - result.BodyMatch is true when the response body matches the pattern
//   - result.StatusCode contains the HTTP response status code
//   - result.Success is true when the HTTP request succeeded, the body
//     matches the configured pattern, and the status code matches
//     expected_status_codes when configured
//   - The response body is always fully read and closed before returning
//   - result.Error is non-empty when Success is false
func (p *HTTPBodyProber) Probe(ctx context.Context, target config.TargetConfig) (result ProbeResult) {
	start := time.Now()
	defer func() { result.Duration = time.Since(start) }()

	if p.regexCompileErr != nil {
		result.Error = fmt.Sprintf("invalid body_match_regex: %s", p.regexCompileErr.Error())
		return result
	}

	if p.proxyURL != nil {
		return p.probeViaProxy(ctx, target, start)
	}
	return p.probeDirect(ctx, target)
}

// probeDirect performs a direct HTTP probe using the standard
// http.Transport with httptrace instrumentation. Duration is set by the
// deferred assignment in Probe.
func (p *HTTPBodyProber) probeDirect(ctx context.Context, target config.TargetConfig) (result ProbeResult) {
	method := target.ProbeOpts.Method
	if method == "" {
		method = http.MethodGet
	}

	// Phase timing anchors. Zero-valued times indicate the phase did not
	// occur (e.g. tls_handshake on plain HTTP, or dns_resolve for literal
	// IP hosts). httptrace callbacks may fire concurrently from net/http
	// transport goroutines, so the timings are guarded by a mutex and read
	// via snapshot after client.Do returns.
	timings := &httpTraceTimings{}

	reqCtx := httptrace.WithClientTrace(ctx, timings.trace())
	req, err := http.NewRequestWithContext(reqCtx, method, target.Address, nil)
	if err != nil {
		result.Error = fmt.Sprintf("creating request: %s", err.Error())
		return result
	}

	for k, v := range target.ProbeOpts.Headers {
		req.Header.Set(k, v)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		result.Error = fmt.Sprintf("http request: %s", err.Error())
		result.Phases = buildPhasesFromSnapshot(timings.snapshot(), time.Time{})
		return result
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBodyBytes+1))
	_ = resp.Body.Close()
	transferEnd := time.Now()
	result.HTTPResponseReceived = true
	result.StatusCode = resp.StatusCode

	if err != nil {
		result.Error = fmt.Sprintf("reading response body: %s", err.Error())
		result.Phases = buildPhasesFromSnapshot(timings.snapshot(), transferEnd)
		return result
	}

	if int64(len(body)) > maxHTTPBodyBytes {
		result.Error = fmt.Sprintf("response body exceeds %d byte limit", maxHTTPBodyBytes)
		result.Phases = buildPhasesFromSnapshot(timings.snapshot(), transferEnd)
		return result
	}

	result.HTTPBodyEvaluated = true
	result.BodyMatch = matchBody(string(body), target.ProbeOpts, p.compiledBodyMatchRegex)
	statusMatch := len(target.ProbeOpts.ExpectedStatusCodes) == 0 ||
		slices.Contains(target.ProbeOpts.ExpectedStatusCodes, resp.StatusCode)
	result.Success = result.BodyMatch && statusMatch
	result.Phases = buildPhasesFromSnapshot(timings.snapshot(), transferEnd)

	if !result.Success {
		switch {
		case !result.BodyMatch && !statusMatch:
			result.Error = fmt.Sprintf("body match failed and unexpected status code %d (body length %d)", resp.StatusCode, len(body))
		case !result.BodyMatch:
			result.Error = fmt.Sprintf("body match failed (status %d, body length %d)", resp.StatusCode, len(body))
		default:
			result.Error = fmt.Sprintf("unexpected status code %d", resp.StatusCode)
		}
	}

	return result
}

// probeViaProxy performs an HTTP body probe through p.proxyURL using an
// explicit proxy-aware code path that measures proxy_dial, optional
// proxy_tls, proxy_connect (for HTTPS targets), and the target
// tls_handshake separately. start is the moment Probe was entered so
// transferEnd-relative durations can be reported without relying on the
// deferred assignment.
//
// When p.followRedirects is true, 3xx responses trigger a follow-up
// exchange through the same proxy (up to proxyRedirectLimit hops). The
// reported Phases reflect the final hop only. The reported StatusCode is
// the final hop's status. When p.followRedirects is false, a 3xx response
// is returned as-is and body validation proceeds against it.
func (p *HTTPBodyProber) probeViaProxy(ctx context.Context, target config.TargetConfig, _ time.Time) (result ProbeResult) {
	currentURL, err := url.Parse(target.Address)
	if err != nil {
		result.Error = fmt.Sprintf("parse target URL: %s", err.Error())
		return result
	}

	method := target.ProbeOpts.Method
	if method == "" {
		method = http.MethodGet
	}

	var exchange proxyHTTPExchangeResult
	currentMethod := method
	currentHeaders := target.ProbeOpts.Headers
	hop := 0
	for {
		exchange = executeProxyHTTPExchange(ctx, p.proxyURL, currentURL, currentMethod, currentHeaders, nil, p.tlsSkipVerify)

		if exchange.connectResp.Observed && !result.ProxyConnectResponseReceived {
			// Preserve the first CONNECT status observed across redirect
			// hops so diagnostic visibility is not silently overwritten.
			result.ProxyConnectResponseReceived = true
			result.ProxyConnectStatusCode = exchange.connectResp.StatusCode
		}

		if exchange.err != nil {
			result.Error = fmt.Sprintf("http request: %s", exchange.err.Error())
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
			result.Error = fmt.Sprintf("following redirect: %s", err.Error())
			return result
		}
		if hop >= proxyRedirectLimit-1 {
			result.Error = "stopped after 10 redirects"
			result.Phases = buildProxyPhases(exchange.phases, exchange.setupEnd,
				exchange.wroteRequest, exchange.gotFirstByte, time.Time{})
			return result
		}
		currentMethod, _, currentHeaders = redirectedRequest(
			currentMethod,
			exchange.resp.StatusCode,
			target.ProbeOpts.Headers,
			0,
		)
		currentURL = next
		hop++
	}

	resp := exchange.resp
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBodyBytes+1))
	_ = resp.Body.Close()
	transferEnd := time.Now()
	result.HTTPResponseReceived = true
	result.StatusCode = resp.StatusCode

	if err != nil {
		result.Error = fmt.Sprintf("reading response body: %s", err.Error())
		result.Phases = buildProxyPhases(exchange.phases, exchange.setupEnd,
			exchange.wroteRequest, exchange.gotFirstByte, transferEnd)
		return result
	}

	if int64(len(body)) > maxHTTPBodyBytes {
		result.Error = fmt.Sprintf("response body exceeds %d byte limit", maxHTTPBodyBytes)
		result.Phases = buildProxyPhases(exchange.phases, exchange.setupEnd,
			exchange.wroteRequest, exchange.gotFirstByte, transferEnd)
		return result
	}

	result.HTTPBodyEvaluated = true
	result.BodyMatch = matchBody(string(body), target.ProbeOpts, p.compiledBodyMatchRegex)
	statusMatch := len(target.ProbeOpts.ExpectedStatusCodes) == 0 ||
		slices.Contains(target.ProbeOpts.ExpectedStatusCodes, resp.StatusCode)
	result.Success = result.BodyMatch && statusMatch
	result.Phases = buildProxyPhases(exchange.phases, exchange.setupEnd,
		exchange.wroteRequest, exchange.gotFirstByte, transferEnd)

	if !result.Success {
		switch {
		case !result.BodyMatch && !statusMatch:
			result.Error = fmt.Sprintf("body match failed and unexpected status code %d (body length %d)", resp.StatusCode, len(body))
		case !result.BodyMatch:
			result.Error = fmt.Sprintf("body match failed (status %d, body length %d)", resp.StatusCode, len(body))
		default:
			result.Error = fmt.Sprintf("unexpected status code %d", resp.StatusCode)
		}
	}

	return result
}

// matchBody checks the body against the configured regex or substring pattern.
// Regex takes precedence when both are configured. Returns false if neither
// pattern is configured or if a regex is configured without a compiled regex.
func matchBody(body string, opts config.ProbeOptions, compiledRegex *regexp.Regexp) bool {
	if compiledRegex != nil {
		return compiledRegex.MatchString(body)
	}
	if opts.BodyMatchRegex != "" {
		return false
	}

	if opts.BodyMatchString != "" {
		return strings.Contains(body, opts.BodyMatchString)
	}

	return false
}
