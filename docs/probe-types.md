# Probe Types

Supported probe types: `tcp`, `http`, `icmp`, `mtu`, `dns`, `tls_cert`, `http_body`, `proxy`.

## TCP

Measures TCP connection establishment time.

```yaml
- name: "rds-postgres"
  address: "rds-endpoint.eu-central-1.rds.amazonaws.com:5432"
  probe_type: tcp
  timeout: 3s
```

No `probe_opts` required.

Emits `probe_phase_duration_seconds` with `phase="tcp_connect"`, plus `phase="dns_resolve"` for hostname targets.

## HTTP/HTTPS

Full HTTP request with `httptrace` phase breakdown (DNS resolve, TCP connect, TLS handshake, request write, TTFB, transfer). Extracts the earliest TLS certificate expiry from the peer chain for HTTPS targets. Supports optional proxy routing via `proxy_url`.

The HTTP probe does not inspect response body content. It discards and reads at
most the effective response body limit: `probe_opts.response_body_limit_bytes` when set,
otherwise 1 MiB. Larger or streaming responses do not fail the probe because
of size; `probe_success` is determined by request/transfer errors and
`expected_status_codes`, while `probe_http_response_truncated` reports whether
the limit was exceeded. The `transfer` phase measures time from first response
byte until either the body ends or the effective response body limit has been read.
Use `http_body` when response content must be validated.

```yaml
- name: "ssm-http"
  address: "https://ssm.eu-central-1.amazonaws.com"
  probe_type: http
  timeout: 5s
  probe_opts:
    method: GET                     # GET, HEAD, POST (default: GET)
    headers:                        # Custom request headers
      X-Custom: "value"
    follow_redirects: false         # Follow HTTP redirects (default: false)
    tls_skip_verify: false          # Skip TLS certificate verification
    expected_status_codes: []       # Empty = accept any status code
    response_body_limit_bytes: 0    # 0/omitted = 1 MiB capped body read
    request_body_bytes: 0           # 0/omitted = no generated request body
    proxy_url: ""                   # Optional: route through HTTP proxy
```

### request_body_bytes

`request_body_bytes` generates an outbound HTTP request body of exactly the
configured size. It is supported only by `probe_type: http`.

Validation and behavior:

- `0` or omitted sends no generated request body.
- Positive values require explicit `method: POST`.
- Positive values are capped at 16 MiB (`16777216` bytes).
- The generated body is sent with `Content-Length`, not chunked transfer
  encoding.
- If no `Content-Type` header is configured, generated bodies use
  `application/octet-stream`.

Upload time appears in `probe_phase_duration_seconds{phase="request_write"}`.
`ttfb` starts after request write completion, so large or slow uploads do not
inflate the server response-time phase.

Use this as an HTTP upload-path stress check, not as an exact MTU measurement.
For the full operational guidance, see
[http-request-payload-probe.md](http-request-payload-probe.md).

### expected_status_codes

Controls how `probe_success` is determined from the HTTP response:

| Configuration | Behaviour |
|---|---|
| `expected_status_codes: []` | Any HTTP response that completes the capped body read is a success (`probe_success=1`). The probe fails if the request or capped response transfer fails (timeout, DNS error, TLS error, interrupted body transfer, etc.). |
| `expected_status_codes: [200]` | Success only if the response status code is exactly 200. Any other code sets `probe_success=0`. |
| `expected_status_codes: [200, 201, 204]` | Success if the response code matches any value in the list. |

When an HTTP response is received, the actual status code is recorded in `probe_http_status_code` regardless of this setting, so you can see what the target returned even when the probe reports failure. Having both metrics is valuable: `probe_success` drives alerting (is the probe healthy?), while `probe_http_status_code` provides the diagnostic detail (what exactly did the target return?). When a probe starts failing after a response is received, the status code tells you whether the target returned 403 (auth issue), 502 (upstream down), 503 (overloaded), or something else — without this, you only know it broke but not why. If no HTTP response is received at all (for example on DNS, TCP, TLS, or timeout failure), `probe_http_status_code` is absent.

Examples:
- AWS service endpoints return 403 without SigV4 signing — use `[]` to test reachability without caring about the status code.
- Health check endpoints that return 200 on success — use `[200]` to detect when the service is unhealthy.
- APIs that may return 200 or 204 — use `[200, 204]` to accept both as valid.

When `proxy_url` is set, the HTTP transport routes all requests through the specified proxy. This is useful for probing public endpoints from private subnets via the infrastructure proxy:

```yaml
- name: "ssm-http-via-proxy"
  address: "https://ssm.eu-central-1.amazonaws.com"
  probe_type: http
  timeout: 5s
  probe_opts:
    method: GET
    follow_redirects: false
    expected_status_codes: []
    proxy_url: "http://infra-proxy.example.internal:8888"
```

For proxies that require Basic authentication, include credentials in the proxy URL:

```yaml
proxy_url: "http://username:password@infra-proxy.example.internal:8888"
```

Skipping TLS verification for HTTPS proxies is not supported. `tls_skip_verify`
applies to the target TLS connection, not to the proxy's own TLS certificate.

## ICMP

ICMP echo with configurable ping count, packet loss ratio, average RTT, and RTT standard deviation. Uses unprivileged ICMP sockets (no `CAP_NET_RAW` needed). Requires `net.ipv4.ping_group_range` to include the process effective or supplementary GID (default on most Linux distributions).

For ICMP, `probe_duration_seconds` is the wall-clock duration of the full probe execution, including multiple pings and configured `ping_interval` waits. Echo round-trip timing is exposed separately as `probe_icmp_avg_rtt_seconds` and `probe_icmp_stddev_rtt_seconds`.

ICMP probes are IPv4-only in the current implementation. Literal IPv6 addresses are rejected at config load time. Hostnames are allowed, but they must resolve to an IPv4 address at runtime; hostnames with only `AAAA` records will fail and log a `resolve IPv4 address` error.

```yaml
- name: "ssm-icmp"
  address: "ssm.eu-central-1.amazonaws.com"
  probe_type: icmp
  timeout: 5s
  probe_opts:
    ping_count: 5                   # Number of ICMP echo requests
    ping_interval: 1.0              # Seconds between pings
```

## MTU/PMTUD

Detects path MTU by sending ICMP echo requests with the IPv4 DF-bit set, stepping down through configured payload sizes. Uses Linux unprivileged ICMP ping sockets and does not require `CAP_NET_RAW`; `net.ipv4.ping_group_range` must include the process effective or supplementary GID.

MTU probes are IPv4-only in the current implementation. Literal IPv6 addresses are rejected at config load time. Hostnames are allowed, but they must resolve to an IPv4 address at runtime; hostnames with only `AAAA` records will fail and log a `resolve IPv4 address` error.

Path MTU is calculated as: `largest_successful_payload + 28` (20 bytes IP header + 8 bytes ICMP header).

```yaml
# Uses agent-level default_icmp_payload_sizes — no probe_opts needed
- name: "api-gw-int-cn-mtu"
  address: "api-gw-int.example.com"
  probe_type: mtu
  interval: 300s
  timeout: 30s

# Override: test jumbo frames beyond standard Ethernet
- name: "cvm-test-mo-mtu"
  address: "10.242.131.36"
  probe_type: mtu
  interval: 300s
  timeout: 30s
  probe_opts:
    icmp_payload_sizes: [1600, 1472, 1392, 1372, 1272, 1172, 1072]
    expected_min_mtu: 1500
    mtu_retries: 3
    mtu_per_attempt_timeout: 2s
```

### Default Payload Sizes

MTU probes use ICMP payload sizes from the following precedence chain:

1. **Target-level** `probe_opts.icmp_payload_sizes` — if specified, used as-is
2. **Agent-level** `agent.default_icmp_payload_sizes` — if specified in config
3. **Built-in default** `[1472, 1392, 1372, 1272, 1172, 1072]` — hardcoded fallback

This means most MTU targets need no `probe_opts` at all — they inherit the agent default. Only targets that need non-standard sizes (e.g. jumbo frame testing) require an explicit override.

| ICMP Payload | Path MTU (payload + 28) |
|--------------|-------------------------|
| 1472         | 1500 (standard Ethernet)|
| 1392         | 1420 (common WireGuard default / IPv6 underlay worst-case) |
| 1372         | 1400 (common with tunnels / VPN) |
| 1272         | 1300                    |
| 1172         | 1200                    |
| 1072         | 1100                    |

MTU metrics expose the status contract:

```text
probe_mtu_state{state="ok|degraded|unreachable|error", detail="..."} 1
probe_mtu_bytes
probe_icmp_avg_rtt_seconds
```

`probe_mtu_bytes` is absent when no size was confirmed, and `probe_mtu_state` carries the reason. `probe_icmp_avg_rtt_seconds` is the average round-trip time across all successful ICMP echo replies during the probe (sanity echo and step-down payloads).

For a detailed explanation of PMTUD, ICMP Destination Unreachable codes, PMTUD black holes, and how the MTU probe interprets results, see [mtu-pmtud.md](mtu-pmtud.md).

## DNS

DNS resolution with optional expected result validation. Measures resolution time and optionally verifies that the returned records match a configured set of expected values.

```yaml
- name: "rds-dns"
  address: "rds-endpoint.eu-central-1.rds.amazonaws.com"
  probe_type: dns
  timeout: 5s
  probe_opts:
    dns_query_name: "rds-endpoint.eu-central-1.rds.amazonaws.com"
    dns_query_type: A               # A, AAAA, or CNAME
    dns_server: ""                  # Custom resolver (optional, default: system resolver)
    dns_expected: []                # Expected IPs or CNAMEs (optional)
```

If `dns_query_name` is omitted, the target's `address` is used as the query name.

### dns_expected (Result Match)

When `dns_expected` is set, the prober compares the actual DNS response against the expected values. The comparison is order-independent, case-insensitive, and strips trailing dots from CNAMEs.

| Configuration | Behaviour |
|---|---|
| `dns_expected: []` (or omitted) | Probe succeeds if DNS returns at least one record. No result validation. `probe_dns_result_match` metric is not emitted. |
| `dns_expected: ["10.0.1.5"]` | Probe succeeds only if DNS returns exactly `10.0.1.5` and nothing else. |
| `dns_expected: ["10.0.1.5", "10.0.1.6"]` | Probe succeeds only if DNS returns exactly these two IPs (in any order) and nothing else. |

When validation is active, two metrics work together:

- `probe_success` — 0 if the result doesn't match (drives alerting)
- `probe_dns_result_match` — 1 if match, 0 if mismatch (shown on the "DNS Result Match" dashboard panel)

On mismatch, the agent logs the actual vs expected values: `dns expected result mismatch: got [10.0.1.7], want [10.0.1.5]`.

### Use Cases

- **Detect DNS hijacking or poisoning** — set `dns_expected` to the known-good IPs and alert when they change unexpectedly.
- **Verify failover** — monitor that a DNS record switches to the DR IP after failover.
- **Validate CNAME chains** — ensure a CNAME points to the expected target (e.g. after a migration).
- **Monitor round-robin changes** — detect when IPs are added or removed from a round-robin A record.

### Examples

```yaml
# Simple resolution monitoring (no expected result validation)
- name: "api-dns"
  address: "api.example.com"
  probe_type: dns
  timeout: 3s

# Validate that the RDS endpoint resolves to the expected IP
- name: "rds-dns-match"
  address: "rds-endpoint.eu-central-1.rds.amazonaws.com"
  probe_type: dns
  timeout: 5s
  probe_opts:
    dns_query_type: A
    dns_expected: ["10.0.1.5"]

# Use a specific DNS server instead of the system resolver
- name: "api-dns-custom-resolver"
  address: "api.example.com"
  probe_type: dns
  timeout: 3s
  probe_opts:
    dns_server: "8.8.8.8:53"
    dns_query_type: A

# Validate CNAME target
- name: "cdn-cname"
  address: "cdn.example.com"
  probe_type: dns
  timeout: 3s
  probe_opts:
    dns_query_type: CNAME
    dns_expected: ["d1234.cloudfront.net"]
```

## TLS Certificate Expiry

Performs a TLS handshake and extracts certificate expiry from the observed peer chain. The alert-oriented `probe_tls_cert_expiry_timestamp_seconds` metric reports the earliest `NotAfter` timestamp in the chain, while `probe_tls_cert_chain_expiry_timestamp_seconds` emits one series per observed certificate with `cert_index` and `cert_role` labels. Supports optional proxy routing via `proxy_url`; for proxy-path targets the agent establishes an HTTP CONNECT tunnel first, then performs the TLS handshake through that tunnel.

```yaml
- name: "api-gw-pub-eu-tls"
  address: "api-gw-pub.example.internal:443"
  probe_type: tls_cert
  interval: 300s
  timeout: 5s
  probe_opts:
    tls_skip_verify: false
    proxy_url: ""                   # Optional: inspect certificate through HTTP proxy
```

The reported expiry is based on the certificate chain observed by NetSonar from that network path. With a normal CONNECT proxy this is the origin chain. With TLS inspection, the observed chain may be proxy-issued; this is useful when the operational question is what workloads behind that proxy actually see.

Direct probes emit `probe_phase_duration_seconds` with `phase="tcp_connect"` and `phase="tls_handshake"`, plus `phase="dns_resolve"` for hostname targets. Proxy-path probes emit `proxy_dial`, optional `proxy_tls` (for `https://` proxies), `proxy_connect`, and `tls_handshake` for the target handshake through the tunnel.

## HTTP Body Validation

HTTP request with regex or substring match on the response body. The probe succeeds (`probe_success=1`) when **both** conditions are met: the response body matches the configured pattern, and the response status code satisfies the `expected_status_codes` rule (empty list accepts any status; non-empty list requires the status code to be present). When body evaluation completes, the match result is also reported separately via `probe_http_body_match` for diagnostic visibility. If no HTTP response is received or the body cannot be evaluated, `probe_http_body_match` is absent. The agent reads at most 1 MiB of response body; larger responses fail the probe. Supports optional proxy routing via `proxy_url`.

At least one of `body_match_regex` or `body_match_string` must be set — a target without either is rejected at config load time. `body_match_regex` must be a valid Go regular expression. It is validated when the config is loaded and compiled once when the target prober is created. If both `body_match_regex` and `body_match_string` are set, the regex takes precedence.

```yaml
- name: "api-health-body"
  address: "https://api.example.com/health"
  probe_type: http_body
  timeout: 5s
  probe_opts:
    method: GET                     # GET, HEAD, POST (default: GET)
    body_match_string: "ok"         # Substring match
    body_match_regex: "status.*ok"  # Regex match (alternative)
    proxy_url: ""                   # Optional: route through HTTP proxy
```

## Proxy Connectivity

Establishes a raw HTTP CONNECT tunnel through a configured proxy and measures tunnel establishment time. This probe type tests the proxy's ability to create TCP tunnels, not regular HTTP forwarding.

```yaml
- name: "egress-proxy-connect"
  address: "https://example.com"
  probe_type: proxy
  timeout: 5s
  probe_opts:
    proxy_url: "http://fwd-proxy.example.internal:8888"
```

If the proxy URL contains credentials, the CONNECT request includes `Proxy-Authorization: Basic ...`.

Proxy probes expose phase timings regardless of whether the CONNECT succeeded or failed, which helps diagnose where time is spent when a proxy rejects the tunnel:

| Phase | What It Measures |
|---|---|
| `proxy_dial` | TCP dial to the proxy |
| `proxy_tls` | TLS handshake with the proxy, only for `https://` proxy URLs |
| `proxy_connect` | CONNECT request write and proxy response read |

### Proxy Probe vs HTTP Probe with `proxy_url`

The agent offers two distinct ways to test proxy connectivity. Choosing the wrong one leads to false failures.

**`probe_type: proxy`** — sends an `HTTP CONNECT` request to the proxy, asking it to open a raw TCP tunnel to the target. The proxy does not see or interpret the traffic inside the tunnel. Many forward proxies (Squid, Tinyproxy) restrict or disable CONNECT to prevent arbitrary protocol tunnelling. If the target host is not on the proxy's CONNECT allowlist, the probe fails even though regular HTTP forwarding through the same proxy works fine.

Use `proxy` when:
- Testing SSH-over-proxy, WebSocket, or other non-HTTP protocols tunnelled through CONNECT
- Verifying the proxy's CONNECT allowlist (positive and negative tests)
- Measuring raw tunnel establishment time without TLS or HTTP overhead

**`probe_type: http` with `proxy_url`** — sends a standard HTTP request routed through the proxy using Go's `http.Transport.Proxy`. For HTTPS targets, the transport internally performs CONNECT + TLS handshake + HTTP request as a single operation, which is the standard way clients (curl, wget, apt) use forward proxies.

**`probe_type: tls_cert` with `proxy_url`** — sends CONNECT to the proxy, then performs only the target TLS handshake through the tunnel and records the observed certificate expiry. It does not send an HTTP request to the target.

Use `http` with `proxy_url` when:
- Testing that a forward proxy can reach a target endpoint (the common case)
- Verifying proxy connectivity for HTTP/HTTPS traffic as clients actually use it
- You need full HTTP metrics (status code, phase timing, TLS certificate expiry)

Use `tls_cert` with `proxy_url` when:
- You only need certificate expiry metrics from `network_path="proxy"`
- The target has no useful HTTP endpoint, or a full HTTP request would be too invasive
- You need to observe certificates as workloads behind an egress proxy see them

```yaml
# Recommended: test proxy connectivity the way clients use it
- name: "egress-proxy-ok"
  address: "https://checkip.amazonaws.com"
  probe_type: http
  timeout: 5s
  probe_opts:
    method: GET
    proxy_url: "http://fwd-proxy.example.internal:8888"
    follow_redirects: false
    expected_status_codes: [200]
  tags:
    service: egress-proxy
    # ...
```

### Interpreting Metrics for Proxy-Path HTTP Probes

When an HTTP probe uses `proxy_url`, the phase timing metrics reflect the full proxy path:

| Phase | What It Measures (`network_path="proxy"`) |
|---|---|
| `tcp_connect` | TCP dial to proxy + CONNECT tunnel establishment to target |
| `tls_handshake` | TLS handshake with the target (through the tunnel) |
| `request_write` | Time from connection ready (after TLS for HTTPS) to request write completion |
| `ttfb` | Time from request write completion to first response byte — does not include TLS handshake or request upload |
| `transfer` | Response body read up to the effective response body limit (through the tunnel) |

The `tcp_connect` phase is notably higher for proxy-path probes compared to direct ones because it includes both the connection to the proxy and the CONNECT handshake. This is expected and can be used to estimate proxy overhead by comparing `tcp_connect` of a proxy-path probe against a direct probe to the same target.

When a `tls_cert` probe uses `proxy_url`, phase timing metrics show the CONNECT tunnel setup and target TLS handshake:

| Phase | What It Measures (`tls_cert` over `network_path="proxy"`) |
|---|---|
| `proxy_dial` | TCP dial to the proxy |
| `proxy_tls` | TLS handshake with the proxy, only for `https://` proxy URLs |
| `proxy_connect` | CONNECT request write and proxy response read |
| `tls_handshake` | TLS handshake with the target through the tunnel |

### Identifying Proxy-Path Probes on the Dashboard

The agent automatically adds a `network_path` label to every probe metric: `"proxy"` when the target has a `proxy_url` configured, `"direct"` otherwise. This requires no manual configuration — the agent detects it from `probe_opts.proxy_url`.

On the Grafana dashboard:
- The "All Probes — Status Table" includes a "Path" column.
- The "Proxy-Path HTTP Probes" section filters by `probe_type="http", network_path="proxy"`.
- The "HTTP Phase Timing (Proxy Path)" panel shows HTTP phase timings for proxy-path HTTP probes.
- The "Proxy CONNECT Probes" section filters by `probe_type="proxy"`.
- The "Proxy CONNECT Phase Timing" panel shows raw CONNECT probe phases: `proxy_dial`, `proxy_tls`, and `proxy_connect`.
- PromQL filter: `probe_success{network_path="proxy"}` selects all proxy-path probes regardless of probe type or service name.

For a detailed explanation of how HTTP proxies work, when to use each probe type, and how to interpret proxy-path metrics, see [proxy-probing.md](proxy-probing.md).
