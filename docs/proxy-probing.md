# Proxy Probing Guide

## Table of Contents

- [Overview](#overview)
- [How HTTP Proxies Work](#how-http-proxies-work)
  - [Forward Proxy (HTTP targets)](#forward-proxy-http-targets)
  - [CONNECT Tunnel (HTTPS targets)](#connect-tunnel-https-targets)
  - [Why CONNECT Is Often Restricted](#why-connect-is-often-restricted)
- [Probe Types for Proxy Testing](#probe-types-for-proxy-testing)
  - [HTTP Probe with proxy\_url (Recommended)](#http-probe-with-proxy_url-recommended)
  - [Proxy Probe (CONNECT Tunnel)](#proxy-probe-connect-tunnel)
  - [When to Use Which](#when-to-use-which)
- [Interpreting Metrics](#interpreting-metrics)
  - [The proxied Label](#the-proxied-label)
  - [Phase Timing for Proxied Probes](#phase-timing-for-proxied-probes)
  - [Comparing Direct vs Proxied](#comparing-direct-vs-proxied)
- [Dashboard Layout](#dashboard-layout)
- [Configuration Examples](#configuration-examples)
  - [Test That a Proxy Can Reach an Endpoint](#test-that-a-proxy-can-reach-an-endpoint)
  - [Test That a Proxy Blocks a Domain](#test-that-a-proxy-blocks-a-domain)
  - [Test CONNECT Tunnel Capability](#test-connect-tunnel-capability)
  - [Proxy Authentication](#proxy-authentication)
- [Troubleshooting](#troubleshooting)

## Overview

The NetSonar agent supports two distinct ways to test proxy connectivity. Choosing the wrong one leads to false failures. This document explains the difference, when to use each, and how to interpret the resulting metrics.

[Back to Table of Contents](#table-of-contents)

## How HTTP Proxies Work

HTTP proxies handle traffic differently depending on whether the target is plain HTTP or HTTPS.

### Forward Proxy (HTTP targets)

```
Client ── GET http://example.com ──► Proxy ── GET ──► Target Server
Client ◄── response ──────────────── Proxy ◄── response ── Target Server
```

The client sends the full URL in the request. The proxy reads the request, forwards it to the target, and returns the response. The proxy has full visibility into the traffic: URL, headers, body. It can filter, log, and cache.

### CONNECT Tunnel (HTTPS targets)

```
Client ── CONNECT example.com:443 ──► Proxy ── [opens TCP tunnel] ──► Target Server
Client ◄── HTTP/1.1 200 OK ───────── Proxy

Client ◄════ encrypted TLS directly with target ════► Target Server
```

The client asks the proxy to open a raw TCP connection to the target on port 443. Once the tunnel is established, the proxy cannot see the traffic inside — it is encrypted TLS end-to-end between the client and the target. The proxy acts as a transparent pipe.

When a standard HTTP client (curl, wget, Go's `http.Transport`) is configured with a proxy and the target is HTTPS, it automatically uses CONNECT to establish the tunnel, then performs the TLS handshake and HTTP request through it. This is the standard behaviour.

### Why CONNECT Is Often Restricted

From a security perspective, CONNECT is a potential risk. The client can tunnel any protocol (SSH, VPN, arbitrary TCP) through port 443, and the proxy has no visibility into the traffic. For this reason, many forward proxies:

- Restrict CONNECT to port 443 only (or a small set of allowed ports)
- Require an explicit allowlist of domains for CONNECT
- Disable CONNECT entirely

This means a proxy can successfully forward regular HTTP traffic to a domain while simultaneously rejecting a raw CONNECT request to the same domain.

[Back to Table of Contents](#table-of-contents)

## Probe Types for Proxy Testing

### HTTP Probe with proxy_url (Recommended)

`probe_type: http` with `proxy_url` in `probe_opts` sends a standard HTTP request routed through the proxy using Go's `http.Transport.Proxy`. For HTTPS targets, the transport internally performs CONNECT + TLS handshake + HTTP request as a single operation — exactly how real clients use forward proxies.

This is the recommended approach for testing proxy connectivity because:

- It tests the proxy the way clients actually use it (curl, wget, apt, application code)
- It provides full HTTP metrics: status code, phase timing breakdown, TLS certificate expiry
- It works with standard forward proxy configurations without requiring special CONNECT allowlists

### Proxy Probe (CONNECT Tunnel)

`probe_type: proxy` sends a raw HTTP CONNECT request to the proxy and measures the tunnel establishment time. It does not perform a TLS handshake or HTTP request through the tunnel — it only tests whether the proxy allows the CONNECT method to the target host and port.

This probe type exists for specific use cases where CONNECT tunnel capability itself needs to be verified, not general proxy connectivity.

Proxy probes expose their own phase timings regardless of whether the CONNECT succeeded or failed. This is useful for diagnosing where time is spent when a proxy rejects the tunnel:

| Phase | What It Measures |
|---|---|
| `proxy_dial` | TCP dial to the proxy |
| `proxy_tls` | TLS handshake with the proxy, only for `https://` proxy URLs |
| `proxy_connect` | CONNECT request write and proxy response read |

When the proxy rejects the CONNECT (e.g. 403), `proxy_dial` and `proxy_connect` are still recorded. Only phases that were not reached (e.g. `proxy_tls` when the dial itself failed) are absent.

### When to Use Which

| Scenario | Probe Type | Why |
|---|---|---|
| Verify a forward proxy can reach an endpoint | `http` + `proxy_url` | Tests the full client flow (CONNECT + TLS + HTTP) |
| Verify proxy blocks a domain (negative test) | `http` + `proxy_url` | Proxy returns an error status or rejects the connection |
| Test SSH-over-proxy or WebSocket tunnelling | `proxy` | These protocols require raw CONNECT tunnels |
| Verify the proxy's CONNECT allowlist | `proxy` | Directly tests CONNECT acceptance/rejection |
| Measure raw tunnel establishment time | `proxy` | Isolates the CONNECT handshake without TLS/HTTP overhead |

[Back to Table of Contents](#table-of-contents)

## Interpreting Metrics

### The proxied Label

The agent automatically adds a `proxied` label to every metric:

- `proxied="true"` — the target has a `proxy_url` configured in `probe_opts`
- `proxied="false"` — the target connects directly

This label is derived automatically from the configuration. No manual tags are needed.

### Phase Timing for Proxied Probes

When an HTTP probe uses `proxy_url`, the `probe_phase_duration_seconds` metric reflects the full proxied path:

| Phase | Direct Probe | Proxied Probe |
|---|---|---|
| `tcp_connect` | TCP handshake with target | TCP dial to proxy + CONNECT tunnel to target |
| `tls_handshake` | TLS handshake with target | TLS handshake with target (through tunnel) |
| `ttfb` | Time to first byte from target | Time to first byte (through tunnel) |
| `transfer` | Response body transfer | Response body transfer (through tunnel) |

The key difference is `tcp_connect`: for proxied probes it includes both the connection to the proxy server and the CONNECT handshake to establish the tunnel. This phase is notably higher for proxied probes and represents the proxy overhead.

### Comparing Direct vs Proxied

To estimate proxy overhead, compare the `tcp_connect` phase of a proxied probe against a direct probe to the same or similar target:

```promql
# Direct tcp_connect (baseline)
probe_phase_duration_seconds{target_name="ssm-http", phase="tcp_connect"}

# Proxied tcp_connect (includes proxy overhead)
probe_phase_duration_seconds{target_name="infra-proxy-ok", phase="tcp_connect"}
```

The difference approximates the proxy's processing time for CONNECT establishment.

[Back to Table of Contents](#table-of-contents)

## Dashboard Layout

The Grafana dashboard separates direct and proxied probes to avoid misleading comparisons:

| Section | Content | Filter |
|---|---|---|
| All Probes — Status Table | All probes with a "Proxied" column | None (shows everything) |
| HTTP/HTTPS Probes (Direct) | Direct HTTP duration, status codes, and phase timing | `probe_type="http", proxied="false"` |
| HTTP Body Probes | Body match, HTTP status codes, and duration | `probe_type="http_body"` |
| Proxied HTTP Probes | Proxied HTTP status, duration, and HTTP phase timing | `probe_type="http", proxied="true"` |
| Proxy CONNECT Probes | Raw CONNECT success, duration, and proxy phase timing | `probe_type="proxy"` |

This separation prevents proxied probes (with inherently higher latency due to the proxy hop) from distorting the Y-axis scale of direct probe charts.

[Back to Table of Contents](#table-of-contents)

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
    criticality: high
```

Success means: proxy accepted the connection, established a CONNECT tunnel to the target, TLS handshake completed, and the target returned HTTP 200.

### Test That a Proxy Blocks a Domain

```yaml
- name: egress-proxy-fail
  address: "https://example.com"
  probe_type: http
  timeout: 5s
  probe_opts:
    method: GET
    proxy_url: "http://fwd-proxy.example.internal:8888"
    follow_redirects: false
    expected_status_codes: []    # accept any response — we expect failure
  tags:
    scope: same-region
    service: egress-proxy
    criticality: high
```

If the proxy blocks `example.com`, the probe fails (`probe_success=0`) because the proxy rejects the CONNECT or returns an error. This is the expected behaviour for a negative test.

### Test CONNECT Tunnel Capability

```yaml
- name: proxy-connect-test
  address: "https://example.com"
  probe_type: proxy
  timeout: 5s
  probe_opts:
    proxy_url: "http://fwd-proxy.example.internal:8888"
  tags:
    scope: same-region
    service: egress-proxy
    criticality: high
```

This sends only an HTTP CONNECT request. Success means the proxy allowed the tunnel. No TLS handshake or HTTP request is performed through the tunnel.

### Proxy Authentication

Proxy credentials can be embedded in `proxy_url` using standard URL userinfo:

```yaml
probe_opts:
  proxy_url: "http://username:password@proxy.example.internal:8888"
```

For `http` and `http_body` probes, Go's HTTP transport uses those credentials when routing requests through the proxy. For `proxy` probes, the agent sends them on the raw CONNECT request as a `Proxy-Authorization: Basic ...` header. A username without a password is encoded as an empty password (`username:`).

Avoid committing real proxy passwords to shared configuration files. Prefer deployment-time secret injection or file permissions that limit access to the agent configuration.

[Back to Table of Contents](#table-of-contents)

## Troubleshooting

| Symptom | Likely Cause | Fix |
|---|---|---|
| `probe_type: proxy` fails but `probe_type: http` with same `proxy_url` works | Proxy allows CONNECT for standard HTTPS clients but the raw CONNECT probe triggers a different code path or allowlist | Switch to `probe_type: http` with `proxy_url` — it tests the real client flow |
| Proxied probe gets `407 Proxy Authentication Required` | Proxy requires credentials and `proxy_url` has no `user:pass@` userinfo, or the credentials are wrong | Set `proxy_url` to `http://user:pass@proxy:port` or fix the deployed secret |
| `probe_type: http` with `proxy_url` fails, direct HTTP to same target works | Proxy blocks the target domain or the proxy is unreachable | Check proxy allowlist; verify proxy host:port is reachable from the agent |
| `tcp_connect` phase is very high for proxied probes | Expected — includes proxy dial + CONNECT handshake | Compare against direct probes to estimate proxy overhead |
| `proxy_dial` is high for `probe_type: proxy` | Slow or congested path to the proxy | Check network path and proxy listener saturation |
| `proxy_connect` is high for `probe_type: proxy` | Proxy is slow to accept or authorize CONNECT | Check proxy policy, authentication backend, and proxy logs |
| Proxied probe shows `probe_success=0` | Proxy closed the connection, returned a non-200 CONNECT response, rejected authentication, or blocked the target | Check agent logs for `probe failed`, check proxy logs, and try `curl -x proxy:port https://target` from the agent host |

[Back to Table of Contents](#table-of-contents)
