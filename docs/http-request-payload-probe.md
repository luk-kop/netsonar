# HTTP Request Payload Probe

## Purpose

NetSonar's `http` probe can generate an outbound request body with a configured
size. This is useful for exercising upload paths, for example to detect failures
that only appear once TCP has to carry more than a small request.

This is an HTTP upload stress signal, not an exact MTU measurement. Use the
`mtu` probe for explicit ICMP/DF path MTU threshold monitoring.

## Configuration

Set `probe_opts.request_body_bytes` on a `probe_type: http` target:

```yaml
targets:
  - name: "api-upload-path"
    address: "https://api.example.com/upload-test"
    probe_type: http
    timeout: 10s
    probe_opts:
      method: POST
      request_body_bytes: 65536
      response_body_limit_bytes: 1048576
      expected_status_codes: [200, 204, 400, 401, 403, 404, 405]
```

Semantics:

- `0` or omitted sends no generated request body.
- Positive values generate a deterministic request body of exactly that size.
- Positive values require explicit `method: POST`.
- The maximum generated request body is 16 MiB (`16777216` bytes).
- `request_body_bytes` is supported only by `probe_type: http`; it is rejected
  for `http_body` and all other probe types.
- Generated payloads use `Content-Length`; they do not use chunked transfer
  encoding.
- When a generated body is configured and no `Content-Type` header is supplied,
  the prober sets `Content-Type: application/octet-stream`.

`request_body_bytes` controls the exact upload size. By contrast,
`response_body_limit_bytes` controls only how much response body the probe reads
and discards before reporting truncation.

## What This Detects

A larger HTTP request body can detect application-path failures such as:

- Upload requests hanging or timing out on specific routes.
- PMTUD black-hole style problems on the client-to-server direction.
- Paths where small HTTP requests succeed but larger uploads fail.
- Proxy, firewall, load balancer, gateway, WAF, or application behavior that
  only appears for larger request bodies.

The result answers this question:

```text
Can this real HTTP path successfully carry an upload of size N?
```

It does not answer:

```text
What is the exact path MTU?
```

## Caveats

A large HTTP request body does not directly create one large IP packet. HTTP
runs over TCP, and TCP splits the byte stream into segments according to MSS and
the operating system's PMTU state.

This probe may not fail even when an MTU problem exists if:

- TCP MSS clamping is working somewhere in the path.
- The local OS already knows the smaller path MTU.
- Packetization Layer PMTUD or TCP retransmission behavior recovers.
- The problematic direction is server-to-client rather than client-to-server.
- TLS, proxy, or load balancer termination means the tested TCP path ends before
  the constrained segment.
- The target application responds before fully reading the request body.

It may fail for reasons unrelated to MTU, including:

- Request body size limits.
- Authentication or authorization behavior.
- WAF or proxy policy.
- Slow application request-body reads.
- Server-side timeout settings.

## Metrics

The standard HTTP metrics remain the primary signal:

- `probe_success`
- `probe_duration_seconds`
- `probe_phase_duration_seconds{phase="request_write"}`
- `probe_phase_duration_seconds{phase="ttfb"}`
- `probe_phase_duration_seconds{phase="transfer"}`
- `probe_http_status_code`
- `probe_http_response_truncated`

Generated upload time is attributed to the `request_write` phase. `ttfb` starts
after request write completion, so slow or blocked uploads do not appear as
server response latency.

The response body is still read only up to the effective
`response_body_limit_bytes` cap. If the response exceeds that cap,
`probe_http_response_truncated` is set to `1`; truncation does not fail an
`http` probe by itself.

## Examples

### Detect Upload Path Black Holes

```yaml
- name: "public-api-upload-64k"
  address: "https://api.example.com/upload-health"
  probe_type: http
  interval: 30s
  timeout: 10s
  probe_opts:
    method: POST
    request_body_bytes: 65536
    expected_status_codes: []
```

This checks whether the path can complete a 64 KiB HTTP upload and receive any
HTTP response.

### Test Through an HTTP Proxy

```yaml
- name: "egress-proxy-upload-256k"
  address: "https://service.example.com/upload-health"
  probe_type: http
  interval: 60s
  timeout: 15s
  probe_opts:
    method: POST
    request_body_bytes: 262144
    expected_status_codes: []
    proxy_url: "http://infra-proxy.example.internal:8888"
```

This checks the full configured HTTP proxy path, not only direct connectivity.

## Relationship to the MTU Probe

Use `mtu` when you need an explicit ICMP/DF payload checkpoint:

```text
What configured ICMP/DF payload checkpoint can this IPv4 path carry?
```

Use `http` with `request_body_bytes` when you need to test the HTTP path that
real clients use, including proxy routing, TLS termination, load balancers, and
application request handling:

```text
Can this application path complete an upload of size N?
```

Success of the HTTP upload probe does not prove the whole path MTU is healthy.
Failure does not prove the cause is MTU. Interpret it alongside HTTP phase
timings, status codes, agent logs, `mtu` probe results, and network path
context.
