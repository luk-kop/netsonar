// Package probe — HTTPBodyProber implementation.
package probe

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
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

	if proxyURL != "" {
		transport.Proxy = http.ProxyURL(mustProxyURL("NewHTTPBodyProber", proxyURL))
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

	return &HTTPBodyProber{
		client:                 client,
		compiledBodyMatchRegex: compiledRegex,
		regexCompileErr:        regexErr,
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
func (p *HTTPBodyProber) Probe(ctx context.Context, target config.TargetConfig) ProbeResult {
	var result ProbeResult

	if p.regexCompileErr != nil {
		result.Error = fmt.Sprintf("invalid body_match_regex: %s", p.regexCompileErr.Error())
		return result
	}

	method := target.ProbeOpts.Method
	if method == "" {
		method = http.MethodGet
	}

	req, err := http.NewRequestWithContext(ctx, method, target.Address, nil)
	if err != nil {
		result.Error = fmt.Sprintf("creating request: %s", err.Error())
		return result
	}

	for k, v := range target.ProbeOpts.Headers {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		result.Duration = time.Since(start)
		result.Error = fmt.Sprintf("http request: %s", err.Error())
		return result
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBodyBytes+1))
	_ = resp.Body.Close()
	result.Duration = time.Since(start)
	result.StatusCode = resp.StatusCode

	if err != nil {
		result.Error = fmt.Sprintf("reading response body: %s", err.Error())
		return result
	}

	if int64(len(body)) > maxHTTPBodyBytes {
		result.Error = fmt.Sprintf("response body exceeds %d byte limit", maxHTTPBodyBytes)
		return result
	}

	result.BodyMatch = matchBody(string(body), target.ProbeOpts, p.compiledBodyMatchRegex)
	statusMatch := len(target.ProbeOpts.ExpectedStatusCodes) == 0 ||
		slices.Contains(target.ProbeOpts.ExpectedStatusCodes, resp.StatusCode)
	result.Success = result.BodyMatch && statusMatch

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
