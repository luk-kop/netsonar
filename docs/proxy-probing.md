# Proxy Probing Guide

## Table of Contents

- [Overview](#overview)
- [How HTTP Proxies Work](#how-http-proxies-work)
  - [Forward Proxy (HTTP targets)](#forward-proxy-http-targets)
  - [CONNECT Tunnel (HTTPS targets)](#connect-tunnel-https-targets)
  - [Why CONNECT Is Often Restricted](#why-connect-is-often-restricted)
- [Probe Types for Proxy Testing](#probe-types-for-proxy-testing)
  - [HTTP Probe with proxy_name (Recommended)](#http-probe-with-proxy_name-recommended)
  - [TLS Certificate Probe with proxy_name](#tls-certificate-probe-with-proxy_name)
  - [Proxy CONNECT Probe](#proxy-connect-probe)
  - [When to Use Which](#when-to-use-which)
- [Interpreting Metrics](#interpreting-metrics)
  - [The proxy_name Label](#the-proxy_name-label)
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

### HTTP Probe with proxy_name (Recommended)

`probe_type: http` with `proxy_name` sends a standard HTTP request routed through the proxy. For HTTPS targets the prober performs an explicit CONNECT + target TLS handshake + HTTP exchange, measuring each step independently so CONNECT latency is visible. For plain HTTP targets the prober uses standard HTTP forward proxying (no CONNECT) — the proxy receives the HTTP request in absolute-URI form.

This is the recommended approach for testing proxy connectivity because:

- It tests the proxy the way clients actually use it (curl, wget, apt, application code)
- It provides full HTTP metrics: status code and phase timing breakdown (including explicit proxy-path phases), with TLS certificate expiry when `tls_emit_cert_metrics: true`
- It works with standard forward proxy configurations without requiring special CONNECT allowlists

### TLS Certificate Probe with proxy_name

`probe_type: tls_cert` with `proxy_name` establishes an HTTP CONNECT tunnel, then performs the target TLS handshake through that tunnel. It records certificate expiry without sending an HTTP request to the target.

Use this when the goal is certificate monitoring from the same network path that workloads use behind an egress proxy. The reported expiry is based on whatever peer certificate chain NetSonar observes through that path. With a transparent CONNECT proxy this is the origin chain; with TLS inspection it may be a proxy-issued chain.

TLS certificate probes over `proxy_name!=""` expose phase timings for `proxy_dial`, optional `proxy_tls`, `proxy_connect`, and `tls_handshake`.

### Proxy CONNECT Probe

`probe_type: proxy_connect` sends a raw HTTP CONNECT request to the proxy and measures the tunnel establishment time. It does not perform a TLS handshake or HTTP request through the tunnel — it only tests whether the proxy allows the CONNECT method to the target host and port.

This probe type exists for specific use cases where CONNECT tunnel capability itself needs to be verified, not general proxy connectivity.

Proxy CONNECT probes expose their own phase timings regardless of whether the CONNECT succeeded or failed. This is useful for diagnosing where time is spent when a proxy rejects the tunnel:

| Phase | What It Measures |
|---|---|
| `proxy_dial` | TCP dial to the proxy |
| `proxy_tls` | TLS handshake with the proxy, only for `https://` proxy endpoints |
| `proxy_connect` | CONNECT request write and proxy response read |

When the proxy rejects the CONNECT (e.g. 403), `proxy_dial` and `proxy_connect` are still recorded. Only phases that were not reached (e.g. `proxy_tls` when the dial itself failed) are absent.

### When to Use Which

| Scenario | Probe Type | Why |
|---|---|---|
| Verify a forward proxy can reach an HTTP URL | `http` or `http_body` + `proxy_name` | Tests regular HTTP forwarding through the proxy |
| Verify a forward proxy can reach an HTTPS host | `http` or `http_body` + `proxy_name` | Tests the normal client flow: CONNECT + TLS + HTTP |
| Monitor certificate expiry through a proxy | `tls_cert` + `proxy_name` | Tests CONNECT + TLS only, without sending an HTTP request |
| Verify proxy blocks a plain HTTP URL | `http` or `http_body` + `proxy_name` | Tests HTTP forwarding policy; a 403 can come from the proxy or the origin unless proxy-specific headers are inspected |
| Verify proxy blocks HTTPS CONNECT to a host:port | `proxy_connect` + `expected_proxy_connect_status_codes` | The origin cannot answer if the tunnel was denied, so CONNECT 403 is an unambiguous proxy decision |
| Test SSH-over-proxy or other raw TCP tunnelling | `proxy_connect` | These protocols require raw CONNECT tunnels |
| Verify the proxy's CONNECT allowlist | `proxy_connect` | Directly tests CONNECT acceptance/rejection |
| Measure raw tunnel establishment time | `proxy_connect` | Isolates the CONNECT handshake without TLS/HTTP overhead |

## Interpreting Metrics

### The proxy_name Label

The agent automatically adds a `proxy_name` label to every probe metric:

- `proxy_name!=""` — the target references a proxy with `proxy_name`
- `proxy_name=""` — the target connects directly

This label is derived automatically from the configuration. No manual tags are needed.

`proxy_name` is supported for `http`, `http_body`, and `tls_cert`; it is required for `proxy_connect`; non-empty values are rejected for `tcp`, `icmp`, `mtu`, and `dns`.

For `probe_type="proxy_connect"`, `proxy_name!=""` means the probe explicitly tests CONNECT through `proxy_name` to `target.Address`.

### Phase Timing for Proxy-Path Probes

When an `http` or `http_body` probe uses `proxy_name`, the `probe_phase_duration_seconds` metric exposes explicit proxy-path phases. Phase emission depends on the combination of target URL scheme and proxy endpoint scheme. `tcp_connect` and `proxy_dial` are **mutually exclusive** per probe execution: direct probes emit `tcp_connect`, proxy-path probes emit `proxy_dial` instead. `dns_resolve` is not emitted on the proxy path — proxy hostname resolution is included in `proxy_dial` and the target hostname is resolved by the proxy itself.

| Target URL  | Proxy endpoint scheme | Proxy Phases Emitted                                            | Target Phases Emitted |
|-------------|-----------------------|-----------------------------------------------------------------|-----------------------|
| `http://`   | `http://`   | `proxy_dial`                                                    | `request_write`, `ttfb`, `transfer` |
| `http://`   | `https://`  | `proxy_dial`, `proxy_tls`                                       | `request_write`, `ttfb`, `transfer` |
| `https://`  | `http://`   | `proxy_dial`, `proxy_connect`                                   | `tls_handshake`, `request_write`, `ttfb`, `transfer` |
| `https://`  | `https://`  | `proxy_dial`, `proxy_tls`, `proxy_connect`                      | `tls_handshake`, `request_write`, `ttfb`, `transfer` |

Individual phase meaning when `proxy_name!=""`:

| Phase | Meaning |
|---|---|
| `proxy_dial` | TCP dial to the proxy (includes proxy hostname DNS resolution) |
| `proxy_tls` | TLS handshake with the proxy (only for `https://` proxies) |
| `proxy_connect` | CONNECT request write and proxy response read (only for HTTPS targets through a proxy) |
| `tls_handshake` | TLS handshake with the target through the tunnel (only for HTTPS targets) |
| `request_write` | Time from connection ready to request write completion |
| `ttfb` | Time from request write completion to first response byte |
| `transfer` | Response body read up to the effective response body limit (through the tunnel) |

For `tls_cert` probes over `proxy_name!=""`, phases are split more explicitly:

| Phase | Meaning |
|---|---|
| `proxy_dial` | TCP dial to the proxy |
| `proxy_tls` | TLS handshake with the proxy, only for `https://` proxy endpoints |
| `proxy_connect` | CONNECT request write and proxy response read |
| `tls_handshake` | TLS handshake with the target through the tunnel |

### Comparing Direct vs Proxy Path

To estimate proxy overhead, compare `proxy_dial + proxy_connect` of a proxy-path probe against `tcp_connect` of a direct probe to the same or similar target:

```promql
# Direct tcp_connect (baseline)
probe_phase_duration_seconds{target_name="ssm-http", phase="tcp_connect"}

# Proxy-path proxy hop time (dial + CONNECT)
sum by (target_name) (
  probe_phase_duration_seconds{target_name="ssm-http-via-proxy", phase=~"proxy_dial|proxy_connect"}
)
```

The difference approximates the proxy's processing time for CONNECT establishment.

## Dashboard Layout

The Grafana dashboard separates direct and proxy-path probes to avoid misleading comparisons:

| Section | Content | Filter |
|---|---|---|
| Overview | All Probes — Status Table with Path and Target columns, plus Skipped Probe Cycles scheduler health | None (shows everything) |
| HTTP Probes (Direct) | Direct HTTP duration, status codes, truncation, and phase panels | `probe_type="http", proxy_name=""` |
| HTTP Probes (Proxy) | Proxy-path HTTP duration and phase panels | `probe_type="http", proxy_name!=""` |
| HTTP Response Body Probes | Response-body status snapshot, body match, HTTP status codes, duration, and phase panels | `probe_type="http_body"` |
| Proxy CONNECT Probes | Raw CONNECT success, duration, status, and proxy phase panels | `probe_type="proxy_connect"` |
| TCP Connectivity | TCP duration and phase panels | `probe_type="tcp"` |
| DNS Resolution | DNS status snapshot, duration, and result-match panels | `probe_type="dns"` |
| TLS Certificate Expiry | Certificate expiry, chain details, and TLS cert phase panels | `probe_type="tls_cert"` |
| ICMP Ping | ICMP status snapshot, packet loss, RTT, and RTT standard deviation panels | `probe_type="icmp"` |

This separation prevents proxy-path probes (with inherently higher latency due to the proxy hop) from distorting the Y-axis scale of direct probe charts.

## Local Lab Coverage

The local labs cover both proxy modes:

- `lab/e2e` includes `http-via-proxy`, which uses `probe_type: http` with
  `proxy_name` and expects HTTP 200 through the fake forward proxy.
- `lab/e2e` also includes `proxy-connect-ok` and `proxy-connect-denied`, which
  use `probe_type: proxy_connect` to verify raw CONNECT acceptance and rejection.
- `lab/e2e` includes `tls-cert-via-proxy`, which uses `probe_type: tls_cert`
  with `proxy_name` and expects the fake TLS endpoint certificate through the
  CONNECT tunnel.
- `lab/dev-stack` mirrors those scenarios for interactive Prometheus and
  Grafana dashboard work.

The fake proxy is deliberately narrow. It forwards only
`GET http://fake-targets:8080/...` and handles CONNECT only for the controlled
fake TCP and fake TLS targets. It is a regression fixture, not a general-purpose
open proxy.

## Configuration Examples

The target examples below assume a top-level proxy registry entry like:

Migrating to `v0.8.0` from per-target proxy URLs is covered in
[Proxy Registry Migration](migrations/proxy-registry.md).

```yaml
proxies:
  fwd-egress:
    url: "http://proxy.example.internal:8888"
```

### Test That a Proxy Can Reach an Endpoint

```yaml
- name: egress-proxy-ok
  address: "https://checkip.amazonaws.com"
  probe_type: http
  timeout: 5s
  proxy_name: fwd-egress
  probe_opts:
    method: GET
    follow_redirects: false
    expected_status_codes: [200]
  tags:
    scope: external
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
  proxy_name: fwd-egress
  probe_opts:
    method: GET
    follow_redirects: false
    expected_status_codes: [200]
  tags:
    scope: external
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
  proxy_name: fwd-egress
  tags:
    scope: external
    service: egress-proxy
    impact: high
```

This sends only an HTTP CONNECT request. Success means the proxy allowed the tunnel. No TLS handshake or HTTP request is performed through the tunnel.

The `address` value is `host:port` because that is the request target form used by HTTP CONNECT, for example `CONNECT example.com:443 HTTP/1.1`. Do not use `http://example.com:80` or `https://example.com:443` here: URL syntax belongs to `http` and `http_body` probes, which make an HTTP request through the proxy. The `proxy_connect` probe tests only whether the proxy will open a raw tunnel to the named host and port.

For a negative CONNECT policy test, make the expected proxy response explicit:

```yaml
- name: proxy-connect-denied
  address: "blocked.example.com:443"
  probe_type: proxy_connect
  timeout: 5s
  proxy_name: fwd-egress
  probe_opts:
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
  proxy_name: fwd-egress
  probe_opts:
    tls_skip_verify: false
```

Success means the proxy allowed CONNECT and the TLS handshake completed through the tunnel. The expiry metric reports the certificate observed from that proxy path.

### Proxy Authentication

Proxy credentials are configured on the top-level proxy entry, not in the URL:

```yaml
proxies:
  fwd-egress:
    url: "http://proxy.example.internal:8888"
    username_env: "NETSONAR_PROXY_USER"
    password_file: "/run/secrets/netsonar_proxy_pass"
```

For `http` and `http_body` probes, the agent sends those credentials as a `Proxy-Authorization: Basic ...` header on the forwarded request (plain `http://` target) or on the `CONNECT` request (HTTPS target). For `proxy_connect` and `tls_cert` probes, the agent sends them on the CONNECT request. URL userinfo is rejected.

Avoid committing real proxy passwords to shared configuration files. Prefer deployment-time secret injection or file permissions that limit access to the agent configuration.

For `http`, `http_body`, `proxy_connect`, and `tls_cert` probes,
`probe_opts.tls_skip_verify` applies only to the target TLS connection. HTTPS
proxy TLS verification is controlled on the selected proxy entry with
`proxies.<name>.tls_skip_verify`; setting it to `true` is valid only for
`https://` proxy endpoints.

## Troubleshooting

| Symptom | Likely Cause | Fix |
|---|---|---|
| `probe_type: proxy_connect` fails but `probe_type: http` with same `proxy_name` works | Proxy allows CONNECT for standard HTTPS clients but the raw CONNECT probe triggers a different code path or allowlist | Switch to `probe_type: http` with `proxy_name` — it tests the real client flow |
| Proxy-path probe gets `407 Proxy Authentication Required` | Proxy requires credentials and the selected proxy entry has no credential sources, or the credentials are wrong | Set `username_env`/`username_file` and `password_env`/`password_file`, or fix the deployed secret |
| `probe_type: http` with `proxy_name` fails, direct HTTP to same target works | Proxy blocks the target domain or the proxy is unreachable | Check proxy allowlist; verify proxy host:port is reachable from the agent |
| `probe_type: tls_cert` with `proxy_name` reports a different certificate than direct probing | TLS inspection or a proxy-specific trust path is in use | Treat the metric as the certificate observed from the proxy-path workload path; compare proxy policy and issuer details |
| `tcp_connect` is very high for proxy-path probes | Expected on older NetSonar releases that hid CONNECT latency; newer releases emit `proxy_dial` + `proxy_connect` instead and do not emit `tcp_connect` on the proxy path | Use the proxy-path phase matrix above to interpret per-phase timing |
| `proxy_dial` is high for `probe_type: proxy_connect` | Slow or congested path to the proxy | Check network path and proxy listener saturation |
| `proxy_connect` is high for `probe_type: proxy_connect` | Proxy is slow to accept or authorize CONNECT | Check proxy policy, authentication backend, and proxy logs |
| Proxy-path probe shows `probe_success=0` | Proxy closed the connection, returned a non-200 CONNECT response, rejected authentication, or blocked the target | Check agent logs for `probe failed`, check proxy logs, and try `curl -x proxy:port https://target` from the agent host |
