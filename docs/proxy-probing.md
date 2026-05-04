# Proxy Probing Guide

## Table of Contents

- [Overview](#overview)
- [How HTTP Proxies Work](#how-http-proxies-work)
  - [Forward Proxy (HTTP targets)](#forward-proxy-http-targets)
  - [CONNECT Tunnel (HTTPS targets)](#connect-tunnel-https-targets)
  - [Why CONNECT Is Often Restricted](#why-connect-is-often-restricted)
- [Probe Types for Proxy Testing](#probe-types-for-proxy-testing)
  - [HTTP Probe with proxy_url (Recommended)](#http-probe-with-proxy_url-recommended)
  - [TLS Certificate Probe with proxy_url](#tls-certificate-probe-with-proxy_url)
  - [Proxy CONNECT Probe](#proxy-connect-probe)
  - [When to Use Which](#when-to-use-which)
- [Interpreting Metrics](#interpreting-metrics)
  - [The network_path Label](#the-network_path-label)
  - [Phase Timing for Proxy-Path Probes](#phase-timing-for-proxy-path-probes)
  - [Comparing Direct vs Proxy Path](#comparing-direct-vs-proxy-path)
- [Dashboard Layout](#dashboard-layout)
- [Local Lab Coverage](#local-lab-coverage)
- [Configuration Examples](#configuration-examples)
  - [Test That a Proxy Can Reach an Endpoint](#test-that-a-proxy-can-reach-an-endpoint)
  - [Test That an HTTPS Flow Through a Proxy Fails](#test-that-an-https-flow-through-a-proxy-fails)
  - [Test CONNECT Tunnel Capability](#test-connect-tunnel-capability)
  - [Inspect a Certificate Through a Proxy](#inspect-a-certificate-through-a-proxy)
  - [Proxy Authentication](#proxy-authentication)
- [Troubleshooting](#troubleshooting)

## Overview

The NetSonar agent supports two distinct ways to test proxy connectivity. Choosing the wrong one leads to false failures. This document explains the difference, when to use each, and how to interpret the resulting metrics.

## How HTTP Proxies Work

HTTP proxies handle traffic differently depending on whether the target is plain HTTP or HTTPS.

### Forward Proxy (HTTP targets)

```mermaid
sequenceDiagram
    participant C as Client
    participant P as Proxy
    participant T as Target Server
    C->>P: GET http://example.com
    P->>T: GET /
    T-->>P: response
    P-->>C: response
    Note over P: Proxy sees URL,<br/>headers, body
```

The client sends the full URL in the request. The proxy reads the request, forwards it to the target, and returns the response. The proxy has full visibility into the traffic: URL, headers, body. It can filter, log, and cache.

### CONNECT Tunnel (HTTPS targets)

```mermaid
sequenceDiagram
    participant C as Client
    participant P as Proxy
    participant T as Target Server
    C->>P: CONNECT example.com:443
    P->>T: open TCP connection
    P-->>C: HTTP/1.1 200 OK
    Note over C,T: TCP tunnel established
    C-->>T: TLS handshake (forwarded as opaque bytes)
    C-->>T: HTTPS request (encrypted)
    T-->>C: HTTPS response (encrypted)
```

The client asks the proxy to open a raw TCP connection to the target host and port. `443` is the common HTTPS case, but CONNECT is a generic TCP tunnelling mechanism: clients can use it for TLS-wrapped protocols, WebSocket over TLS, SSH-over-proxy where allowed, or any other protocol the proxy policy permits. Once the tunnel is established, the proxy forwards bytes. For normal HTTPS it cannot see the HTTP path, headers, or body inside the TLS session unless the proxy performs TLS inspection.

When a standard HTTP client (curl, wget, Go's `http.Transport`) is configured with a proxy and the target is HTTPS, it automatically uses CONNECT to establish the tunnel, then performs the TLS handshake and HTTP request through it. This is the standard behaviour. RFC 9110 defines a successful CONNECT as any 2xx response; a non-2xx response means tunnel mode did not start and the response is a regular proxy HTTP response.

Common CONNECT responses have different operational meanings:

| Code | Meaning | Expected block? |
|---|---|---|
| `2xx` | Tunnel established; `200` is the common real-world response | No, this is allow |
| `403` | Proxy ACL denied the CONNECT, common with Squid `TCP_DENIED/403` | Yes, canonical explicit block |
| `407` | Proxy authentication required or credentials rejected | No, usually config or secret issue |
| `502` | Proxy could not connect to upstream | No, infrastructure failure |
| `503`/`504` | Proxy overloaded or timed out | No, infrastructure failure |

This is why NetSonar uses explicit `expected_proxy_connect_status_codes` for negative CONNECT tests instead of treating every non-2xx CONNECT response as success.

### Why CONNECT Is Often Restricted

From a security perspective, CONNECT is a potential risk. The client can tunnel any protocol (SSH, VPN, arbitrary TCP) through port 443, and the proxy has no visibility into the traffic. For this reason, many forward proxies:

- Restrict CONNECT to port 443 only (or a small set of allowed ports)
- Require an explicit allowlist of domains for CONNECT
- Disable CONNECT entirely

This means a proxy can successfully forward regular HTTP traffic to a domain while simultaneously rejecting a raw CONNECT request to the same domain.

## Probe Types for Proxy Testing

### HTTP Probe with proxy_url (Recommended)

`probe_type: http` with `proxy_url` in `probe_opts` sends a standard HTTP request routed through the proxy using Go's `http.Transport.Proxy`. For HTTPS targets, the transport internally performs CONNECT + TLS handshake + HTTP request as a single operation — exactly how real clients use forward proxies.

This is the recommended approach for testing proxy connectivity because:

- It tests the proxy the way clients actually use it (curl, wget, apt, application code)
- It provides full HTTP metrics: status code, phase timing breakdown, and TLS certificate expiry
- It works with standard forward proxy configurations without requiring special CONNECT allowlists

### TLS Certificate Probe with proxy_url

`probe_type: tls_cert` with `proxy_url` establishes an HTTP CONNECT tunnel, then performs the target TLS handshake through that tunnel. It records certificate expiry without sending an HTTP request to the target.

Use this when the goal is certificate monitoring from the same network path that workloads use behind an egress proxy. The reported expiry is based on whatever peer certificate chain NetSonar observes through that path. With a transparent CONNECT proxy this is the origin chain; with TLS inspection it may be a proxy-issued chain.

TLS certificate probes over `network_path="proxy"` expose phase timings for `proxy_dial`, optional `proxy_tls`, `proxy_connect`, and `tls_handshake`.

### Proxy CONNECT Probe

`probe_type: proxy_connect` sends a raw HTTP CONNECT request to the proxy and measures the tunnel establishment time. It does not perform a TLS handshake or HTTP request through the tunnel — it only tests whether the proxy allows the CONNECT method to the target host and port.

This probe type exists for specific use cases where CONNECT tunnel capability itself needs to be verified, not general proxy connectivity.

Proxy CONNECT probes expose their own phase timings regardless of whether the CONNECT succeeded or failed. This is useful for diagnosing where time is spent when a proxy rejects the tunnel:

| Phase | What It Measures |
|---|---|
| `proxy_dial` | TCP dial to the proxy |
| `proxy_tls` | TLS handshake with the proxy, only for `https://` proxy URLs |
| `proxy_connect` | CONNECT request write and proxy response read |

When the proxy rejects the CONNECT (e.g. 403), `proxy_dial` and `proxy_connect` are still recorded. Only phases that were not reached (e.g. `proxy_tls` when the dial itself failed) are absent.

### When to Use Which

| Scenario | Probe Type | Why |
|---|---|---|
| Verify a forward proxy can reach an HTTP URL | `http` or `http_body` + `proxy_url` | Tests regular HTTP forwarding through the proxy |
| Verify a forward proxy can reach an HTTPS host | `http` or `http_body` + `proxy_url` | Tests the normal client flow: CONNECT + TLS + HTTP |
| Monitor certificate expiry through a proxy | `tls_cert` + `proxy_url` | Tests CONNECT + TLS only, without sending an HTTP request |
| Verify proxy blocks a plain HTTP URL | `http` or `http_body` + `proxy_url` | Tests HTTP forwarding policy; a 403 can come from the proxy or the origin unless proxy-specific headers are inspected |
| Verify proxy blocks HTTPS CONNECT to a host:port | `proxy_connect` + `expected_proxy_connect_status_codes` | The origin cannot answer if the tunnel was denied, so CONNECT 403 is an unambiguous proxy decision |
| Test SSH-over-proxy or other raw TCP tunnelling | `proxy_connect` | These protocols require raw CONNECT tunnels |
| Verify the proxy's CONNECT allowlist | `proxy_connect` | Directly tests CONNECT acceptance/rejection |
| Measure raw tunnel establishment time | `proxy_connect` | Isolates the CONNECT handshake without TLS/HTTP overhead |

## Interpreting Metrics

### The network_path Label

The agent automatically adds a `network_path` label to every probe metric:

- `network_path="proxy"` — the target has a `proxy_url` configured in `probe_opts`
- `network_path="direct"` — the target connects directly

This label is derived automatically from the configuration. No manual tags are needed.

`proxy_url` is supported for `http`, `http_body`, and `tls_cert`; it is required for `proxy_connect`; non-empty values are rejected for `tcp`, `icmp`, `mtu`, and `dns`.

For `probe_type="proxy_connect"`, `network_path="proxy"` means the probe explicitly tests CONNECT through `proxy_url` to `target.Address`.

### Phase Timing for Proxy-Path Probes

When an HTTP probe uses `proxy_url`, the `probe_phase_duration_seconds` metric reflects the full proxy path:

| Phase | Direct Probe | Proxy-Path Probe |
|---|---|---|
| `tcp_connect` | TCP handshake with target | TCP dial to proxy + CONNECT tunnel to target |
| `tls_handshake` | TLS handshake with target | TLS handshake with target (through tunnel) |
| `request_write` | Time from connection ready (after TLS) to request write completion | Same, through tunnel |
| `ttfb` | Time from request write completion to first response byte; excludes TLS handshake and request upload | Same, through tunnel |
| `transfer` | Response body read up to the effective response body limit | Response body read up to the effective response body limit (through tunnel) |

The key difference is `tcp_connect`: for proxy-path probes it includes both the connection to the proxy server and the CONNECT handshake to establish the tunnel. This phase is notably higher for proxy-path probes and represents the proxy overhead.

For `tls_cert` probes over `network_path="proxy"`, phases are split more explicitly:

| Phase | Meaning |
|---|---|
| `proxy_dial` | TCP dial to the proxy |
| `proxy_tls` | TLS handshake with the proxy, only for `https://` proxy URLs |
| `proxy_connect` | CONNECT request write and proxy response read |
| `tls_handshake` | TLS handshake with the target through the tunnel |

### Comparing Direct vs Proxy Path

To estimate proxy overhead, compare the `tcp_connect` phase of a proxy-path probe against a direct probe to the same or similar target:

```promql
# Direct tcp_connect (baseline)
probe_phase_duration_seconds{target_name="ssm-http", phase="tcp_connect"}

# Proxy-path tcp_connect (includes proxy overhead)
probe_phase_duration_seconds{target_name="ssm-http-via-proxy", phase="tcp_connect"}
```

The difference approximates the proxy's processing time for CONNECT establishment.

## Dashboard Layout

The Grafana dashboard separates direct and proxy-path probes to avoid misleading comparisons:

| Section | Content | Filter |
|---|---|---|
| All Probes — Status Table | All probes with a "Path" column | None (shows everything) |
| HTTP/HTTPS Probes (Direct) | Direct HTTP duration, status codes, and phase timing | `probe_type="http", network_path="direct"` |
| HTTP Body Probes | Body match, HTTP status codes, and duration | `probe_type="http_body"` |
| Proxy-Path HTTP Probes | Proxy-path HTTP status, duration, and HTTP phase timing | `probe_type="http", network_path="proxy"` |
| TLS Certificate Probes | Certificate expiry, including proxy-path certificate checks | `probe_type="tls_cert"` |
| Proxy CONNECT Probes | Raw CONNECT success, duration, and proxy phase timing | `probe_type="proxy_connect"` |

This separation prevents proxy-path probes (with inherently higher latency due to the proxy hop) from distorting the Y-axis scale of direct probe charts.

## Local Lab Coverage

The local labs cover both proxy modes:

- `lab/e2e` includes `http-via-proxy`, which uses `probe_type: http` with
  `proxy_url` and expects HTTP 200 through the fake forward proxy.
- `lab/e2e` also includes `proxy-connect-ok` and `proxy-connect-denied`, which
  use `probe_type: proxy_connect` to verify raw CONNECT acceptance and rejection.
- `lab/e2e` includes `tls-cert-via-proxy`, which uses `probe_type: tls_cert`
  with `proxy_url` and expects the fake TLS endpoint certificate through the
  CONNECT tunnel.
- `lab/dev-stack` mirrors those scenarios for interactive Prometheus and
  Grafana dashboard work.

The fake proxy is deliberately narrow. It forwards only
`GET http://fake-targets:8080/...` and handles CONNECT only for the controlled
fake TCP and fake TLS targets. It is a regression fixture, not a general-purpose
open proxy.

## Configuration Examples

### Test That a Proxy Can Reach an Endpoint

```yaml
- name: egress-proxy-ok
  address: "https://checkip.amazonaws.com"
  probe_type: http
  timeout: 5s
  probe_opts:
    method: GET
    proxy_url: "http://fwd-proxy.example.internal:8888"
    follow_redirects: false
    expected_status_codes: [200]
  tags:
    scope: same-region
    service: egress-proxy
    impact: high
```

Success means: proxy accepted the connection, established a CONNECT tunnel to the target, TLS handshake completed, and the target returned HTTP 200.

### Test That an HTTPS Flow Through a Proxy Fails

```yaml
- name: egress-proxy-fail
  address: "https://example.com"
  probe_type: http
  timeout: 5s
  probe_opts:
    method: GET
    proxy_url: "http://fwd-proxy.example.internal:8888"
    follow_redirects: false
    expected_status_codes: [200]
  tags:
    scope: same-region
    service: egress-proxy
    impact: high
```

If the proxy blocks CONNECT to `example.com:443`, the probe fails (`probe_success=0`) before any target HTTP response is received. If the CONNECT succeeds and the origin returns a non-200 status, this probe also fails because `expected_status_codes` is about the target HTTP response.

For strict "did the proxy ACL deny CONNECT?" checks, prefer the `proxy_connect` negative-test form below. For plain `http://` URLs, a 403 response may come from the proxy or from the origin server unless proxy-specific headers such as `Proxy-Status`, `Via`, or `X-Squid-Error` are inspected outside NetSonar.

### Test CONNECT Tunnel Capability

```yaml
- name: proxy-connect-test
  address: "example.com:443"
  probe_type: proxy_connect
  timeout: 5s
  probe_opts:
    proxy_url: "http://fwd-proxy.example.internal:8888"
  tags:
    scope: same-region
    service: egress-proxy
    impact: high
```

This sends only an HTTP CONNECT request. Success means the proxy allowed the tunnel. No TLS handshake or HTTP request is performed through the tunnel.

For a negative CONNECT policy test, make the expected proxy response explicit:

```yaml
- name: proxy-connect-denied
  address: "blocked.example.com:443"
  probe_type: proxy_connect
  timeout: 5s
  probe_opts:
    proxy_url: "http://fwd-proxy.example.internal:8888"
    expected_proxy_connect_status_codes: [403]
```

In that form `probe_success=1` means "the proxy denied CONNECT with 403 as expected", and `probe_proxy_connect_status_code=403` carries the diagnostic status. The resulting phase metrics stop at `proxy_dial`, optional `proxy_tls`, and `proxy_connect`; there is no target TLS handshake or HTTP request because the tunnel was not established.

> **Important — `probe_success=1` does not always mean "tunnel established".** When `expected_proxy_connect_status_codes` is set on a `proxy_connect` target, success means "the proxy returned one of the expected statuses". A scrape can therefore show `probe_success=1` paired with a non-2xx `probe_proxy_connect_status_code` (typically `403` for an explicit ACL deny). Dashboards and alerts that need to distinguish "tunnel up" from "expected denial" must inspect `probe_proxy_connect_status_code` alongside `probe_success` for these targets. Targets without `expected_proxy_connect_status_codes` keep the default semantics: `probe_success=1` only when the CONNECT returned 2xx and the tunnel was established.

### Inspect a Certificate Through a Proxy

```yaml
- name: tls-cert-via-proxy
  address: "api.example.com:443"
  probe_type: tls_cert
  timeout: 5s
  probe_opts:
    proxy_url: "http://fwd-proxy.example.internal:8888"
    tls_skip_verify: false
```

Success means the proxy allowed CONNECT and the TLS handshake completed through the tunnel. The expiry metric reports the certificate observed from that proxy path.

### Proxy Authentication

Proxy credentials can be embedded in `proxy_url` using standard URL userinfo:

```yaml
probe_opts:
  proxy_url: "http://username:password@proxy.example.internal:8888"
```

For `http` and `http_body` probes, Go's HTTP transport uses those credentials when routing requests through the proxy. For `proxy_connect` and `tls_cert` probes, the agent sends them on the CONNECT request as a `Proxy-Authorization: Basic ...` header. A username without a password is encoded as an empty password (`username:`).

Avoid committing real proxy passwords to shared configuration files. Prefer deployment-time secret injection or file permissions that limit access to the agent configuration.

NetSonar does not support skipping TLS verification for HTTPS proxies. The
`tls_skip_verify` option applies to the target TLS connection, not to the
proxy's own TLS certificate.

## Troubleshooting

| Symptom | Likely Cause | Fix |
|---|---|---|
| `probe_type: proxy_connect` fails but `probe_type: http` with same `proxy_url` works | Proxy allows CONNECT for standard HTTPS clients but the raw CONNECT probe triggers a different code path or allowlist | Switch to `probe_type: http` with `proxy_url` — it tests the real client flow |
| Proxy-path probe gets `407 Proxy Authentication Required` | Proxy requires credentials and `proxy_url` has no `user:pass@` userinfo, or the credentials are wrong | Set `proxy_url` to `http://user:pass@proxy:port` or fix the deployed secret |
| `probe_type: http` with `proxy_url` fails, direct HTTP to same target works | Proxy blocks the target domain or the proxy is unreachable | Check proxy allowlist; verify proxy host:port is reachable from the agent |
| `probe_type: tls_cert` with `proxy_url` reports a different certificate than direct probing | TLS inspection or a proxy-specific trust path is in use | Treat the metric as the certificate observed from the proxy-path workload path; compare proxy policy and issuer details |
| `tcp_connect` phase is very high for proxy-path probes | Expected — includes proxy dial + CONNECT handshake | Compare against direct probes to estimate proxy overhead |
| `proxy_dial` is high for `probe_type: proxy_connect` | Slow or congested path to the proxy | Check network path and proxy listener saturation |
| `proxy_connect` is high for `probe_type: proxy_connect` | Proxy is slow to accept or authorize CONNECT | Check proxy policy, authentication backend, and proxy logs |
| Proxy-path probe shows `probe_success=0` | Proxy closed the connection, returned a non-200 CONNECT response, rejected authentication, or blocked the target | Check agent logs for `probe failed`, check proxy logs, and try `curl -x proxy:port https://target` from the agent host |
