// Package probe — proxy-aware HTTP probe helpers for http and http_body
// probe types. These helpers expose explicit per-phase timing for the proxy
// path (proxy_dial, proxy_tls, proxy_connect, target tls_handshake) that
// Go's default http.Transport hides when it routes HTTPS through CONNECT.
package probe

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// proxyPhaseTrace captures the phases accumulated while setting up a
// proxy-routed HTTP connection, plus the moment that setup completed so
// callers can anchor request_write against it. connectResp is populated
// only when the path included a CONNECT exchange (HTTPS targets).
// targetTLS, when non-nil, is the TLS connection state observed from the
// target handshake; it lets callers populate resp.TLS so downstream code
// (setTLSCertificateResult) can extract peer certificates.
type proxyPhaseTrace struct {
	phases      map[string]time.Duration
	setupEnd    time.Time
	connectResp proxyConnectResponse
	targetTLS   *tls.ConnectionState
}

// setupProxyHTTPConn establishes a connection ready for HTTP request
// exchange through the configured proxy.
//
// resolver is used to resolve the proxy hostname before dialing. Callers
// pass net.DefaultResolver (or BuildResolver("")) for the system path or
// BuildResolver("ip:port") to pin lookups to a specific server.
//
// Phase emission depends on the target URL scheme:
//
//   - targetURL.Scheme == "https":
//     proxy_dial -> (proxy_tls) -> proxy_connect -> tls_handshake (target).
//     After this returns, the connection is a *tls.Conn speaking TLS to the
//     target through the CONNECT tunnel. Requests must be written in origin
//     form (req.Write).
//   - targetURL.Scheme == "http":
//     proxy_dial -> (proxy_tls).
//     After this returns, the connection is either a plain TCP conn or a
//     *tls.Conn to the proxy. Requests must be written in proxy form
//     (req.WriteProxy) so the request-target is an absolute URI and the
//     proxy knows where to forward.
//
// On error the returned connection is nil and the trace contains phases
// captured before the failure, including a populated connectResp when a
// CONNECT response was read.
func setupProxyHTTPConn(
	ctx context.Context,
	proxyURL, targetURL *url.URL,
	proxyTLSSkipVerify bool,
	targetTLSSkipVerify bool,
	proxyAuthHeader string,
	resolver *net.Resolver,
) (net.Conn, proxyPhaseTrace, error) {
	switch targetURL.Scheme {
	case "https":
		return setupProxyTLSConn(ctx, proxyURL, targetURL, proxyTLSSkipVerify, targetTLSSkipVerify, proxyAuthHeader, resolver)
	case "http":
		return setupProxyPlainConn(ctx, proxyURL, proxyTLSSkipVerify, resolver)
	default:
		return nil, proxyPhaseTrace{}, fmt.Errorf("unsupported target URL scheme %q for proxy path", targetURL.Scheme)
	}
}

func setupProxyTLSConn(
	ctx context.Context,
	proxyURL, targetURL *url.URL,
	proxyTLSSkipVerify bool,
	targetTLSSkipVerify bool,
	proxyAuthHeader string,
	resolver *net.Resolver,
) (net.Conn, proxyPhaseTrace, error) {
	tunnelDest := hostPortForTargetURL(targetURL)
	tunnel, err := dialProxyTunnel(ctx, proxyURL, tunnelDest, proxyTLSSkipVerify, proxyAuthHeader, resolver)
	trace := proxyPhaseTrace{phases: tunnel.phases, connectResp: tunnel.connectResp}
	if err != nil {
		return nil, trace, err
	}

	// Unset the tunnel deadline for the request exchange that follows.
	// dialProxyTunnel may have installed a deadline to cap CONNECT
	// timing against ctx.Deadline(); callers will re-apply their own
	// deadline for the request write/read.
	_ = tunnel.conn.SetDeadline(time.Time{})

	host, _, err := net.SplitHostPort(tunnelDest)
	if err != nil {
		host = targetURL.Hostname()
	}
	tlsCfg := &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: targetTLSSkipVerify,
	}
	tlsConn := tls.Client(tunnel.conn, tlsCfg)
	tlsStart := time.Now()
	err = tlsConn.HandshakeContext(ctx)
	tlsEnd := time.Now()
	addObservedPhase(trace.phases, PhaseTLSHandshake, tlsEnd, tlsStart)
	if err != nil {
		_ = tunnel.conn.Close()
		return nil, trace, fmt.Errorf("tls handshake: %s", err.Error())
	}
	trace.setupEnd = tlsEnd
	state := tlsConn.ConnectionState()
	trace.targetTLS = &state
	return tlsConn, trace, nil
}

func setupProxyPlainConn(
	ctx context.Context,
	proxyURL *url.URL,
	tlsSkipVerify bool,
	resolver *net.Resolver,
) (net.Conn, proxyPhaseTrace, error) {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	proxyAddr := hostPortForURL(proxyURL)
	phases := make(map[string]time.Duration, 2)
	trace := proxyPhaseTrace{phases: phases}

	d := net.Dialer{Resolver: resolver}
	dialStart := time.Now()
	conn, err := d.DialContext(ctx, "tcp", proxyAddr)
	dialEnd := time.Now()
	addObservedPhase(phases, PhaseProxyDial, dialEnd, dialStart)
	if err != nil {
		return nil, trace, fmt.Errorf("proxy dial: %s", err.Error())
	}
	trace.setupEnd = dialEnd

	if proxyURL.Scheme == "https" {
		host, _, splitErr := net.SplitHostPort(proxyAddr)
		if splitErr != nil {
			host = proxyURL.Hostname()
		}
		tlsCfg := &tls.Config{
			ServerName:         host,
			InsecureSkipVerify: tlsSkipVerify,
		}
		tlsConn := tls.Client(conn, tlsCfg)
		tlsStart := time.Now()
		err := tlsConn.HandshakeContext(ctx)
		tlsEnd := time.Now()
		addObservedPhase(phases, PhaseProxyTLS, tlsEnd, tlsStart)
		if err != nil {
			_ = conn.Close()
			return nil, trace, fmt.Errorf("proxy tls handshake: %s", err.Error())
		}
		trace.setupEnd = tlsEnd
		return tlsConn, trace, nil
	}

	return conn, trace, nil
}

// hostPortForTargetURL returns host:port for a target URL, applying the
// default port (443 for https, 80 for http) when no explicit port is
// present.
func hostPortForTargetURL(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// firstByteReader wraps an io.Reader and records the time of the first
// successful non-zero Read. It is used to observe TTFB on connections
// that the Go httptrace machinery cannot instrument (manual proxy-path
// request exchange).
type firstByteReader struct {
	r             io.Reader
	firstByteTime time.Time
}

func (f *firstByteReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if n > 0 && f.firstByteTime.IsZero() {
		f.firstByteTime = time.Now()
	}
	return n, err
}

// buildProxyPhases finalizes the proxy-path phase map by filling in
// request_write, ttfb, and transfer relative to the recorded anchors.
//
// setupEnd is the end of the last proxy setup phase (proxy_dial for plain
// HTTP through a plain proxy, proxy_tls for plain HTTP through an HTTPS
// proxy, or tls_handshake for HTTPS targets). request_write spans from
// setupEnd to wroteRequest. ttfb spans from wroteRequest to gotFirstByte.
// transfer spans from gotFirstByte to transferEnd.
//
// Zero-valued timestamps are treated as "not observed" and the
// corresponding phase is omitted, matching addObservedPhase semantics.
func buildProxyPhases(
	phases map[string]time.Duration,
	setupEnd, wroteRequest, gotFirstByte, transferEnd time.Time,
) map[string]time.Duration {
	if phases == nil {
		phases = make(map[string]time.Duration, 6)
	}
	addObservedPhase(phases, PhaseRequestWrite, wroteRequest, setupEnd)
	addObservedPhase(phases, PhaseTTFB, gotFirstByte, wroteRequest)
	addObservedPhase(phases, PhaseTransfer, transferEnd, gotFirstByte)
	return phases
}

// proxyHTTPExchangeResult holds the outcome of executeProxyHTTPExchange.
// resp is non-nil only when the response status line was parsed. Callers
// own resp.Body and must close it. When the target was HTTPS, resp.TLS is
// populated from the target handshake so downstream code that reads
// resp.TLS.PeerCertificates works the same as on the direct path.
type proxyHTTPExchangeResult struct {
	resp         *http.Response
	phases       map[string]time.Duration
	setupEnd     time.Time
	wroteRequest time.Time
	gotFirstByte time.Time
	connectResp  proxyConnectResponse
	err          error
}

// executeProxyHTTPExchange performs the full proxy-aware HTTP request up
// to and including response header parse. It does NOT read the response
// body; callers are responsible for that step so they can measure the
// transfer phase and apply their own body-size policy.
//
// resolver is used for DNS lookups when dialing the proxy. Callers pass
// net.DefaultResolver (or BuildResolver("")) for the system path or
// BuildResolver("ip:port") to pin lookups to a specific server.
//
// On success the returned conn is owned by resp.Body and is closed when
// resp.Body is closed. On failure the caller receives a nil resp and any
// transient conn has been closed already.
func executeProxyHTTPExchange(
	ctx context.Context,
	proxyURL, targetURL *url.URL,
	method string,
	headers map[string]string,
	requestBody io.Reader,
	proxyTLSSkipVerify bool,
	targetTLSSkipVerify bool,
	proxyAuthHeader string,
	resolver *net.Resolver,
) proxyHTTPExchangeResult {
	var result proxyHTTPExchangeResult

	conn, trace, err := setupProxyHTTPConn(ctx, proxyURL, targetURL, proxyTLSSkipVerify, targetTLSSkipVerify, proxyAuthHeader, resolver)
	result.phases = trace.phases
	result.setupEnd = trace.setupEnd
	result.connectResp = trace.connectResp
	if err != nil {
		result.err = err
		return result
	}

	closeConnOnError := true
	defer func() {
		if closeConnOnError {
			_ = conn.Close()
		}
	}()

	// Apply the probe deadline to the request/response exchange so a slow
	// or stalled peer does not exceed the scheduler-imposed timeout.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	req, err := http.NewRequestWithContext(ctx, method, targetURL.String(), requestBody)
	if err != nil {
		result.err = fmt.Errorf("creating request: %s", err.Error())
		return result
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	// Force Connection: close so the peer (and any upstream) does not try
	// to keep the connection alive. The prober never reuses proxy
	// connections, so half-life keep-alive state would only confuse the
	// peer.
	req.Close = true

	// For plain HTTP targets through a forward proxy the request-target
	// must be an absolute URI (Request.WriteProxy). Proxy authentication
	// also needs to be surfaced on the forwarded request, since there is
	// no CONNECT step that would carry it.
	var writeFn func(io.Writer) error
	if targetURL.Scheme == "http" {
		setProxyAuthorization(req, proxyAuthHeader)
		writeFn = req.WriteProxy
	} else {
		writeFn = req.Write
	}

	if err := writeFn(conn); err != nil {
		wroteRequest := time.Now()
		result.wroteRequest = wroteRequest
		addObservedPhase(result.phases, PhaseRequestWrite, wroteRequest, trace.setupEnd)
		result.err = fmt.Errorf("writing request: %s", err.Error())
		return result
	}
	wroteRequest := time.Now()
	result.wroteRequest = wroteRequest
	addObservedPhase(result.phases, PhaseRequestWrite, wroteRequest, trace.setupEnd)

	fbReader := &firstByteReader{r: conn}
	reader := bufio.NewReader(fbReader)
	resp, err := http.ReadResponse(reader, req)
	gotFirstByte := fbReader.firstByteTime
	result.gotFirstByte = gotFirstByte
	if err != nil {
		addObservedPhase(result.phases, PhaseTTFB, gotFirstByte, wroteRequest)
		result.err = fmt.Errorf("reading response: %s", err.Error())
		return result
	}
	addObservedPhase(result.phases, PhaseTTFB, gotFirstByte, wroteRequest)

	// Replace resp.Body with a body that closes the underlying conn when
	// the caller closes it. http.ReadResponse sets resp.Body to an
	// io.Reader backed by the bufio.Reader; its Close() does not close
	// our conn.
	resp.Body = &connBackedBody{Reader: resp.Body, conn: conn}

	// Propagate target TLS state so callers can extract peer certificates
	// via resp.TLS exactly like on the direct path. http.ReadResponse
	// leaves resp.TLS nil because we parsed the response off a manually
	// wrapped tls.Conn rather than letting http.Transport do it for us.
	if trace.targetTLS != nil {
		resp.TLS = trace.targetTLS
	}

	result.resp = resp
	closeConnOnError = false
	return result
}

// connBackedBody wraps a response body and ensures the underlying
// connection is closed when the body is closed.
type connBackedBody struct {
	io.Reader
	conn net.Conn
}

func (c *connBackedBody) Close() error {
	// Close the underlying reader if it implements io.Closer.
	if closer, ok := c.Reader.(io.Closer); ok {
		_ = closer.Close()
	}
	return c.conn.Close()
}
