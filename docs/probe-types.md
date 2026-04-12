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

## HTTP/HTTPS

Full HTTP request with `httptrace` phase breakdown (DNS resolve, TCP connect, TLS handshake, TTFB, transfer). Extracts TLS certificate expiry for HTTPS targets. Supports optional proxy routing via `proxy_url`.

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
    proxy_url: ""                   # Optional: route through HTTP proxy
```

### expected_status_codes

Controls how `probe_success` is determined from the HTTP response:

| Configuration | Behaviour |
|---|---|
| `expected_status_codes: []` | Any fully received HTTP response is a success (`probe_success=1`). The probe fails if the request or response transfer fails (timeout, DNS error, TLS error, interrupted body transfer, etc.). |
| `expected_status_codes: [200]` | Success only if the response status code is exactly 200. Any other code sets `probe_success=0`. |
| `expected_status_codes: [200, 201, 204]` | Success if the response code matches any value in the list. |

The actual status code is always recorded in the `probe_http_status_code` metric regardless of this setting, so you can see what the target returned even when the probe reports failure. Having both metrics is valuable: `probe_success` drives alerting (is the probe healthy?), while `probe_http_status_code` provides the diagnostic detail (what exactly did the target return?). When a probe starts failing, the status code tells you whether the target returned 403 (auth issue), 502 (upstream down), 503 (overloaded), or something else — without this, you only know it broke but not why.

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

## ICMP

ICMP echo with configurable ping count, average RTT, packet loss ratio, and hop count. Uses unprivileged ICMP sockets (no `CAP_NET_RAW` needed). Requires `net.ipv4.ping_group_range` to include the process GID (default on most Linux distributions).

For ICMP, `probe_duration_seconds` is the wall-clock duration of the full probe execution, including multiple pings and configured `ping_interval` waits. Average echo round-trip time is exposed separately as `probe_icmp_avg_rtt_seconds`.

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

Detects path MTU by sending ICMP echo requests with the IPv4 DF-bit set, stepping down through configured payload sizes. Requires `CAP_NET_RAW`.

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

In the legacy `probe_mtu_path_bytes` metric, a value of `-1` means all sizes failed. In the newer metric contract, `probe_mtu_bytes` is absent when no size was confirmed, and `probe_mtu_state` carries the reason.

New MTU metrics expose the planned status contract:

```text
probe_mtu_state{state="ok|degraded|unreachable|error", detail="..."} 1
probe_mtu_bytes
```

`probe_mtu_path_bytes` is still emitted for compatibility, but new dashboards should use `probe_mtu_state` for alerting and `probe_mtu_bytes` for the confirmed MTU value.

For a detailed explanation of PMTUD, ICMP Destination Unreachable codes, PMTUD black holes, and how the MTU probe interprets results, see [mtu-pmtud.md](mtu-pmtud.md).

## DNS

DNS resolution with optional expected result validation.

```yaml
- name: "rds-dns"
  address: "rds-endpoint.eu-central-1.rds.amazonaws.com"
  probe_type: dns
  timeout: 5s
  probe_opts:
    dns_query_name: "rds-endpoint.eu-central-1.rds.amazonaws.com"
    dns_query_type: A               # A, AAAA, or CNAME
    dns_server: ""                  # Custom resolver (optional)
    dns_expected: []                # Expected IPs or CNAMEs (optional)
```

## TLS Certificate Expiry

Performs a TLS handshake and extracts the leaf certificate's `NotAfter` timestamp.

```yaml
- name: "api-gw-pub-eu-tls"
  address: "api-gw-pub.example.internal:443"
  probe_type: tls_cert
  interval: 300s
  timeout: 5s
```

## HTTP Body Validation

HTTP request with regex or substring match on the response body. The probe succeeds (`probe_success=1`) only when the response body matches the configured pattern. The match result is also reported separately via `probe_http_body_match` for diagnostic visibility. The agent reads at most 1 MiB of response body; larger responses fail the probe. Supports optional proxy routing via `proxy_url`.

`body_match_regex` must be a valid Go regular expression. It is validated when the config is loaded and compiled once when the target prober is created. If both `body_match_regex` and `body_match_string` are set, the regex takes precedence.

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

Successful proxy probes expose phase timings:

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

Use `http` with `proxy_url` when:
- Testing that a forward proxy can reach a target endpoint (the common case)
- Verifying proxy connectivity for HTTP/HTTPS traffic as clients actually use it
- You need full HTTP metrics (status code, phase timing, TLS certificate expiry)

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

### Interpreting Metrics for Proxied HTTP Probes

When an HTTP probe uses `proxy_url`, the phase timing metrics reflect the full proxied path:

| Phase | What It Measures (proxied) |
|---|---|
| `tcp_connect` | TCP dial to proxy + CONNECT tunnel establishment to target |
| `tls_handshake` | TLS handshake with the target (through the tunnel) |
| `ttfb` | Time to first byte from the target (through the tunnel) |
| `transfer` | Response body transfer (through the tunnel) |

The `tcp_connect` phase is notably higher for proxied probes compared to direct ones because it includes both the connection to the proxy and the CONNECT handshake. This is expected and can be used to estimate proxy overhead by comparing `tcp_connect` of a proxied probe against a direct probe to the same target.

### Identifying Proxied Probes on the Dashboard

The agent automatically adds a `proxied` label to every metric: `"true"` when the target has a `proxy_url` configured, `"false"` otherwise. This requires no manual configuration — the agent detects it from `probe_opts.proxy_url`.

On the Grafana dashboard:
- The "All Probes — Status Table" includes a "Proxied" column that shows "YES" (orange) for proxied probes and is blank for direct ones.
- The "Proxied HTTP Probes" section filters by `probe_type="http", proxied="true"`.
- The "HTTP Phase Timing (Proxied)" panel shows HTTP phase timings for proxied HTTP probes.
- The "Proxy CONNECT Probes" section filters by `probe_type="proxy"`.
- The "Proxy CONNECT Phase Timing" panel shows raw CONNECT probe phases: `proxy_dial`, `proxy_tls`, and `proxy_connect`.
- PromQL filter: `probe_success{proxied="true"}` selects all proxied probes regardless of probe type or service name.

For a detailed explanation of how HTTP proxies work, when to use each probe type, and how to interpret proxied metrics, see [proxy-probing.md](proxy-probing.md).
