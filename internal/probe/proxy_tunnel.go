// Package probe contains shared HTTP CONNECT tunnel helpers.
package probe

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// proxyConnectResponse captures the proxy's response to a CONNECT request.
// Observed is true when the proxy returned a parseable HTTP response (success
// or failure); StatusCode is only meaningful when Observed is true.
type proxyConnectResponse struct {
	StatusCode int
	Observed   bool
}

// dialProxyTunnel dials proxyURL and issues HTTP CONNECT to tunnelDest.
// On success it returns a byte-stream net.Conn to tunnelDest plus recorded
// phases and the observed CONNECT response. On error it returns phases
// accumulated up to the failure point, the observed CONNECT response (if
// any), closes any connection it opened, and a non-nil error. The caller
// owns the returned connection on success and must close it.
func dialProxyTunnel(ctx context.Context, proxyURL *url.URL, tunnelDest string) (net.Conn, map[string]time.Duration, proxyConnectResponse, error) {
	proxyAddr := hostPortForURL(proxyURL)
	start := time.Now()
	phases := make(map[string]time.Duration, 3)
	var connectResp proxyConnectResponse

	var d net.Dialer
	proxyConn, err := d.DialContext(ctx, "tcp", proxyAddr)
	proxyDialDone := time.Now()
	addObservedPhase(phases, PhaseProxyDial, proxyDialDone, start)
	if err != nil {
		return nil, phases, connectResp, fmt.Errorf("proxy dial: %s", err.Error())
	}

	closeOnError := true
	defer func() {
		if closeOnError {
			_ = proxyConn.Close()
		}
	}()

	if proxyURL.Scheme == "https" {
		host, _, _ := net.SplitHostPort(proxyAddr)
		tlsCfg := &tls.Config{ServerName: host}
		tlsConn := tls.Client(proxyConn, tlsCfg)
		tlsStart := time.Now()
		err := tlsConn.HandshakeContext(ctx)
		tlsEnd := time.Now()
		addObservedPhase(phases, PhaseProxyTLS, tlsEnd, tlsStart)
		if err != nil {
			return nil, phases, connectResp, fmt.Errorf("proxy tls handshake: %s", err.Error())
		}
		proxyConn = tlsConn
	}

	// Set a deadline on the proxy connection so the CONNECT write/read
	// cannot hang past the probe timeout. tls.Conn.SetDeadline propagates
	// to the underlying net.Conn, so this covers both operations.
	if deadline, ok := ctx.Deadline(); ok {
		if err := proxyConn.SetDeadline(deadline); err != nil {
			return nil, phases, connectResp, fmt.Errorf("set deadline: %s", err.Error())
		}
	}

	// For CONNECT the request-target must be "host:port" (RFC 7231
	// section 4.3.6). Build the URL explicitly so req.Write emits the
	// correct form instead of treating a bare "host:port" as scheme:opaque.
	connectReq := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: tunnelDest},
		Host:   tunnelDest,
		Header: make(http.Header),
	}
	connectReq = connectReq.WithContext(ctx)
	setProxyAuthorization(connectReq, proxyURL)

	connectStart := time.Now()
	if err := connectReq.Write(proxyConn); err != nil {
		addObservedPhase(phases, PhaseProxyConnect, time.Now(), connectStart)
		return nil, phases, connectResp, fmt.Errorf("writing CONNECT request: %s", err.Error())
	}

	resp, err := http.ReadResponse(bufio.NewReader(proxyConn), connectReq)
	addObservedPhase(phases, PhaseProxyConnect, time.Now(), connectStart)
	if err != nil {
		return nil, phases, connectResp, fmt.Errorf("reading CONNECT response: %s", err.Error())
	}
	connectResp = proxyConnectResponse{StatusCode: resp.StatusCode, Observed: true}
	// Intentionally not draining resp.Body before Close:
	// - On 200 OK the body is a CONNECT tunnel stream; draining would block
	//   until ctx deadline and corrupt result.Duration.
	// - On non-200 the body is typically short/empty and proxyConn is closed
	//   by defer; there is no connection pool to return a clean conn to.
	_ = resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, phases, connectResp, fmt.Errorf("proxy CONNECT returned status %d", resp.StatusCode)
	}

	closeOnError = false
	return proxyConn, phases, connectResp, nil
}

// hostPortForURL returns host:port from a proxy URL, applying the scheme
// default (443 for https, 80 otherwise) when the URL has no explicit port.
func hostPortForURL(u *url.URL) string {
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

// setProxyAuthorization applies Basic proxy authentication from proxy URL
// userinfo. A username without a password is encoded as "user:".
func setProxyAuthorization(req *http.Request, proxyURL *url.URL) {
	if proxyURL.User == nil {
		return
	}

	username := proxyURL.User.Username()
	password, _ := proxyURL.User.Password()
	auth := username + ":" + password
	req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(auth)))
}
