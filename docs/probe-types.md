# Probe Types

Supported probe types: `tcp`, `http`, `icmp`, `mtu`, `dns`, `tls_cert`, `http_body`, `proxy_connect`.

## Resolver selection

NetSonar uses Go's standard `net.Resolver` for hostname → IP lookups in all
probe types. By default this goes through the host's system resolver path
(NSS, systemd-resolved, /etc/resolv.conf, local dnsmasq, etc.), which means
its behavior — including caching — depends on host configuration.

The `dns_resolver` option lets you override this and direct NetSonar to send
DNS queries to a specific server, bypassing the system resolver entirely.

### DNS caching behavior — what's in NetSonar's control and what isn't

This is the single most common source of confusion, so read this section
before configuring anything.

**NetSonar itself does NOT maintain any DNS cache.** The agent process keeps
no internal map of hostname → IP results between probes. Every probe
performs a fresh DNS lookup via the configured resolver path.

**However, DNS caching almost certainly still happens** — just not inside
NetSonar. It happens in layers below the agent that NetSonar does not
control:

| Layer                                  | Caches?                                  | Active when                                                                          |
|----------------------------------------|------------------------------------------|--------------------------------------------------------------------------------------|
| NetSonar (application)                 | **No**                                   | Always — there is no app-level cache to disable                                      |
| `nscd` (NSS cache daemon)              | Yes (if installed)                       | `dns_resolver` unset → uses system path                                              |
| `systemd-resolved` stub (127.0.0.53)   | Yes (in-memory, honors record TTL)       | `dns_resolver` unset on most Linux distros                                           |
| Local forwarder (`dnsmasq`, `unbound`) | Yes                                      | `dns_resolver` unset and host is configured to use one                               |
| Upstream recursor (8.8.8.8, corp DNS)  | Yes (this is its job)                    | Always — even when `dns_resolver` is set, the resolver itself may cache              |
| Authoritative nameservers              | No (by design)                           | Always                                                                               |

Practical implications:

1. **Default (`dns_resolver` not set) — system cache active.** NetSonar uses
   the host's normal DNS path. Whatever caches the host has
   (`systemd-resolved`, `nscd`, local dnsmasq) will serve repeated lookups
   from cache. Probe timings reflect cache hits most of the time, with
   occasional fresh lookups when TTL expires.
2. **Custom `dns_resolver: "x:y"` — system cache bypassed, but resolver
   cache still applies.** Setting `dns_resolver` makes NetSonar send DNS
   queries directly via UDP/TCP to the specified address, skipping
   NSS/resolved/`/etc/resolv.conf` entirely. But the resolver you point to
   is itself probably a caching server. If you want truly fresh lookups
   every probe, point `dns_resolver` at a recursor you control with caching
   disabled (e.g. your own `unbound` with `cache-min-ttl: 0`,
   `cache-max-ttl: 0`).
3. **There is no protocol-level "no-cache" flag in DNS.** You cannot ask an
   arbitrary upstream resolver to bypass its cache for a single query. The
   only way to guarantee fresh lookups is to control the resolver yourself.
4. **`probe_dns_resolve_seconds` reflects whatever caching is in effect.**
   If you're seeing suspiciously fast/consistent DNS phase timings, you're
   most likely measuring cache hits — not the actual end-to-end DNS path.
   To measure real resolution latency, point `dns_resolver` at a
   non-caching recursor or use `probe_type: dns` with `dns_server` set
   directly.
5. **`GODEBUG=netdns=*` does nothing in NetSonar.** Binaries are built with
   `CGO_ENABLED=0`, which means the cgo-based resolver code is not compiled
   into the binary at all. The env var has no resolver to switch to —
   pure-Go is the only available path.

Summary: if you need control over DNS caching for your probes, the
mechanism is **`dns_resolver` pointing at a recursor you control**, not any
NetSonar-side option. NetSonar's contract is "use the resolver you tell me
to"; everything past that is operator-owned infrastructure.

### Field reference

| Field                  | Where                                                            | Role                                                            | When to use                                                                                  |
|------------------------|------------------------------------------------------------------|-----------------------------------------------------------------|----------------------------------------------------------------------------------------------|
| `agent.dns_resolver`   | top-level `agent:` block                                         | Global default — applies to all targets unless overridden       | "All my probes should use a specific DNS server (e.g. own unbound, corporate resolver)"     |
| `target.dns_resolver`  | per `targets[]` entry                                            | Per-target override (3-state, see below)                        | "This target should use a different resolver than the global default"                        |
| `probe_opts.dns_server`| per `targets[]` entry, **only for `probe_type: dns`**            | **Subject of measurement** — the DNS server being probed        | "I want to measure latency/correctness of a specific DNS server"                             |

`dns_server` and `dns_resolver` are different concepts:

- `dns_server` is what the DNS probe **measures** — the DNS query goes there.
- `dns_resolver` is what NetSonar **uses internally** to turn hostnames into IPs
  before establishing connections. For `probe_type: dns`, `dns_resolver` is
  unused because `dns_server` is required to be an IP literal (see below).

### Three-state semantics for `target.dns_resolver`

`target.dns_resolver` has three distinct meanings depending on how it's
written in YAML:

```yaml
# Case 1 — field omitted: inherit agent.dns_resolver (most common)
targets:
  - name: api
    probe_type: tcp
    address: api.example.com:443
    # dns_resolver not set → uses agent.dns_resolver

# Case 2 — explicit value: override to a specific resolver
  - name: public-api
    probe_type: tcp
    address: api.example.com:443
    dns_resolver: "8.8.8.8:53"
    # → uses 8.8.8.8:53, ignores agent.dns_resolver

# Case 3 — explicit empty string: opt out of agent.dns_resolver, use system path
  - name: baseline-via-system
    probe_type: tcp
    address: api.example.com:443
    dns_resolver: ""
    # → uses host system resolver (NSS/resolved/etc.) even if agent.dns_resolver is set
```

Effective resolver matrix:

| `agent.dns_resolver` | `target.dns_resolver` (YAML) | Effective resolver used   |
|----------------------|------------------------------|---------------------------|
| not set / `""`       | not set                      | system                    |
| `"X"`                | not set                      | `X`                       |
| `"X"`                | `"Y"`                        | `Y`                       |
| `"X"`                | `""`                         | system (explicit opt-out) |
| not set              | `"Y"`                        | `Y`                       |

### Validation rules

`dns_resolver` (both at `agent` and `target` level) must be either:

- empty (`""`) — meaning "system resolver", or
- an IP literal with port — `"8.8.8.8:53"`, `"[2001:db8::1]:53"`

Hostnames (`"dns.google:53"`) and bare IPs without port (`"8.8.8.8"`) are
**rejected at config load time**. The reason: a hostname-as-resolver would
itself require resolution via the system path, defeating the purpose of the
override.

`dns_server` (under `probe_opts`, only for `probe_type: dns`) must be either:

- empty (`""`) — meaning "use system resolver to perform the query", or
- an IP literal (`"8.8.8.8:53"`, `"8.8.8.8"` — port `:53` is auto-appended)

**Hostnames are rejected** to keep `probe_dns_resolve_seconds` measurements
honest — see "DNS probe caveats" below.

### Common usage patterns

#### Pattern 1 — All probes via a local cache-less recursor

```yaml
agent:
  dns_resolver: "127.0.0.1:5353"   # your own unbound with cache disabled

targets:
  - name: api
    probe_type: tcp
    address: api.example.com:443
  - name: web
    probe_type: http
    address: https://web.example.com
```

All targets inherit `agent.dns_resolver`. Every probe's hostname lookup
goes to `127.0.0.1:5353`. Useful when you want deterministic DNS behavior
independent of host config drift.

#### Pattern 2 — Global default with per-target override

```yaml
agent:
  dns_resolver: "127.0.0.1:5353"

targets:
  - name: internal-api
    probe_type: tcp
    address: api.internal.corp:443
    # uses agent.dns_resolver (127.0.0.1:5353) — has internal zone

  - name: external-api
    probe_type: tcp
    address: api.example.com:443
    dns_resolver: "8.8.8.8:53"   # external API: use public DNS instead
```

#### Pattern 3 — Per-target system-resolver opt-out

```yaml
agent:
  dns_resolver: "127.0.0.1:5353"

targets:
  - name: most-targets
    probe_type: tcp
    address: api.example.com:443
    # uses 127.0.0.1:5353

  - name: system-baseline
    probe_type: tcp
    address: api.example.com:443
    dns_resolver: ""             # opt out — measure system DNS path as baseline
```

Useful when you want one probe to measure the "real" system resolver
behavior (with its NSS/cache/resolved layers) as a baseline, while
everything else uses a controlled resolver.

#### Pattern 4 — DNS probe targeting a specific server

```yaml
targets:
  - name: measure-8888
    probe_type: dns
    address: example.com
    probe_opts:
      dns_server: "8.8.8.8:53"      # subject — the DNS server we measure
      dns_query_name: example.com
    # dns_resolver is unused for DNS probes (dns_server is required to be IP literal)
```

For DNS probes, `dns_resolver` is **not used** because `dns_server` must be
an IP literal (validator enforces this). There is no pre-resolution step
for DNS probes, so the agent-level resolver is irrelevant here.

### What does NOT happen

- **`dns_resolver` is not a DNS cache toggle.** See "DNS caching behavior"
  above — NetSonar has no application-level cache, and `dns_resolver` only
  redirects queries; cache behavior is determined by whatever server you
  point it at.
- **`dns_resolver` does not affect `probe_type: dns`.** Because `dns_server`
  is required to be an IP literal, there is no pre-resolution where
  `dns_resolver` could intervene.
- **`GODEBUG=netdns=*` has no effect in NetSonar.** Binaries are built with
  `CGO_ENABLED=0`, so the cgo resolver code is not compiled in. Setting
  the env var has no alternative resolver to switch to — pure-Go is the
  only available path.

### DNS probe caveats (`probe_type: dns`)

The DNS probe's `dns_server` field must be an **IP literal** (e.g.
`"8.8.8.8:53"`, or just `"8.8.8.8"` — port `:53` is auto-appended). The
validator rejects hostnames.

Why this restriction: if `dns_server` were a hostname, NetSonar would have
to resolve it first (via system path or `dns_resolver`) before sending the
actual measured query. `probe_dns_resolve_seconds` would then contain
**two** DNS lookups — pre-resolution of the server name, plus the test
query — which contaminates the metric and makes it inconsistent across
probes (cache hit vs miss for the pre-resolution).

If you need to monitor a named DNS server (e.g. `dns.google`):

1. Resolve its IP manually once: `dig +short dns.google`
2. Configure the IP directly: `dns_server: "8.8.8.8:53"` (or whichever IP)
3. If you want to alert when the name → IP mapping itself changes, set up
   a separate `probe_type: dns` target that queries for `dns.google`
   against a known recursor.

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

The TCP probe is intentionally connect-only: it opens a TCP connection and then
stops. It does not perform a TLS handshake, send protocol payload bytes, wait
for a banner, or read an application response. Because of that, TCP phase
breakdowns have no `tls_handshake`, `request_write`, `ttfb`, or `transfer`
phases.

## HTTP/HTTPS

Full HTTP request with `httptrace` phase breakdown (DNS resolve, TCP connect, TLS handshake, request write, TTFB, transfer). For HTTPS targets, can optionally emit TLS certificate expiry metrics from the observed peer chain. Supports optional proxy routing via `proxy_url`.

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
    tls_emit_cert_metrics: false    # Emit TLS certificate metrics for HTTPS targets
    expected_status_codes: []       # Empty = accept any status code
    response_body_limit_bytes: 0    # 0/omitted = 1 MiB capped body read
    request_body_bytes: 0           # 0/omitted = no generated request body
    proxy_url: ""                   # Optional: route through HTTP proxy
```

### HTTP Options

| Option                      | Type                | Default       | Description                                                                                                          |
|-----------------------------|---------------------|---------------|----------------------------------------------------------------------------------------------------------------------|
| `method`                    | string              | `GET`         | HTTP method. One of `GET`, `HEAD`, `POST`. Required to be `POST` when `request_body_bytes > 0`.                      |
| `headers`                   | `map[string]string` | `{}`          | Custom request headers. Sent on every probe request.                                                                 |
| `follow_redirects`          | bool                | `false`       | Follow 3xx redirects. When `false`, the redirect response is treated as the final response.                          |
| `tls_skip_verify`           | bool                | `false`       | Skip TLS certificate verification on the **target** connection (HTTPS only). For `http` and `http_body`, it also applies to the proxy TLS connection when `proxy_url` uses `https://`. For `proxy_connect` and `tls_cert`, it applies only to the target; the proxy's TLS certificate is always validated. |
| `tls_emit_cert_metrics`     | bool                | `false`       | Emit `probe_tls_cert_*` metrics for HTTPS targets. This does not affect TLS handshake, certificate verification, or `tls_handshake` phase metrics. |
| `expected_status_codes`     | `[]int`             | `[]`          | Allow-list of HTTP status codes that count as success. Empty list accepts any status. See subsection below.          |
| `response_body_limit_bytes` | int                 | `0` (= 1 MiB) | Cap on response bytes read and discarded. `0` or omitted falls back to the 1 MiB built-in default.                   |
| `request_body_bytes`        | int                 | `0`           | Generate an outbound request body of exactly this size. Requires `method: POST`. Capped at 16 MiB. See below.        |
| `proxy_url`                 | string              | `""`          | Route the request through an HTTP forward proxy (`http://` or `https://`). Basic-auth credentials may be embedded.   |

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

For `http` and `http_body`, `tls_skip_verify` applies to both the target TLS
connection and the proxy TLS connection when `proxy_url` uses `https://`. For
`proxy_connect` and `tls_cert`, it applies only to the target TLS connection;
the proxy's own TLS certificate is always validated.

## ICMP

ICMP echo with configurable ping count, packet loss ratio, average RTT, and RTT standard deviation. Uses unprivileged ICMP sockets (no `CAP_NET_RAW` needed). Requires `net.ipv4.ping_group_range` to include the process effective or supplementary GID (default on most Linux distributions).

For ICMP, `probe_duration_seconds` is the wall-clock duration of the full probe execution, including multiple pings and configured `ping_interval` waits. Echo round-trip timing is exposed separately as `probe_icmp_avg_rtt_seconds` and `probe_icmp_stddev_rtt_seconds`.

ICMP probes are IPv4-only in the current implementation. Literal IPv6 addresses are rejected at config load time. Hostnames are allowed, but they must resolve to an IPv4 address at runtime; hostnames with only `AAAA` records will fail and log a `resolve IPv4 address` error.

```yaml
- name: "ssm-icmp"
  address: "ssm.eu-central-1.amazonaws.com"
  probe_type: icmp
  timeout: 10s
  probe_opts:
    ping_count: 5                   # Number of ICMP echo requests
    ping_interval: 1.0              # Seconds between pings
```

### ICMP Options

| Option          | Type           | Default | Description                                                                                                  |
|-----------------|----------------|---------|--------------------------------------------------------------------------------------------------------------|
| `ping_count`    | int            | `1`     | Number of ICMP echo requests to send. Higher values increase RTT and loss accuracy at the cost of duration.  |
| `ping_interval` | float seconds  | `1.0`   | Seconds between consecutive echo requests. Only meaningful when `ping_count > 1`.                            |

### Sizing `target.timeout` for multi-ping sequences

The full ICMP probe takes at least `(ping_count - 1) × ping_interval + Σ RTT`
wall-clock seconds. The probe's context deadline is shared across all pings in
the sequence, so set `target.timeout` with comfortable headroom over this
minimum. If the deadline fires mid-sequence, only the early echoes complete
and `ICMPRepliesObserved` ends up below `ping_count`. This affects two
metrics, both of which use **current-observation semantics** (see
[docs/metrics.md](./metrics.md)):

- `probe_icmp_avg_rtt_seconds` requires `ICMPRepliesObserved ≥ 1` — deleted
  from `/metrics` when no echo replies were received.
- `probe_icmp_stddev_rtt_seconds` requires `ICMPRepliesObserved ≥ 2` —
  deleted from `/metrics` when fewer than two replies were received. A common
  symptom is the metric flickering in and out as borderline `target.timeout`
  cuts off the last ping or two.

Rule of thumb: `target.timeout ≥ (ping_count - 1) × ping_interval + 2s` for
typical LAN/VPC paths. Add more for high-RTT or cross-region targets, and
remember that `target.timeout` must also be `≤ target.interval` (validated at
config load time).

#### Troubleshooting: `probe_icmp_stddev_rtt_seconds` not in `/metrics`

If the metric is missing for a target that is otherwise healthy
(`probe_success = 1`, `probe_icmp_packet_loss_ratio = 0`), the cause is
almost always `ICMPRepliesObserved < 2`. Check in this order:

1. **Default `ping_count = 1`.** This is the most common cause. If
   `probe_opts.ping_count` is not set explicitly for the target, the agent
   sends a single echo per probe — and a stddev over one sample is undefined,
   so the series is intentionally not emitted. Cross-check from `/metrics`
   without reading the config: `probe_duration_seconds ≈ probe_icmp_avg_rtt_seconds`
   confirms a single ping was sent. With `ping_count = 5, ping_interval = 1s`,
   `probe_duration_seconds` would be at least ~4 s.
2. **`probe_opts` indentation.** YAML silently ignores `ping_count` placed
   outside the `probe_opts:` block. If raising `ping_count` does not change
   `probe_duration_seconds` after a config reload, indentation is the
   prime suspect.
3. **`target.timeout` too tight for the sequence.** See the sizing rule
   above. The probe completes early with `ICMPRepliesObserved` below
   `ping_count`, and if it falls to 1 or 0 the stddev series disappears.
4. **Packet loss across the sequence.** If `probe_icmp_packet_loss_ratio`
   is high enough that fewer than two replies came back, stddev cannot be
   computed. The avg_rtt and stddev metrics will toggle independently as
   conditions change.

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

### MTU Options

| Option                    | Type     | Default                | Description                                                                                                                                          |
|---------------------------|----------|------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------|
| `icmp_payload_sizes`      | `[]int`  | see precedence below   | ICMP echo payload sizes in bytes, tested largest-first with the IPv4 DF bit set. Must be sorted descending. See "Default Payload Sizes" below.       |
| `expected_min_mtu`        | int      | `largest_payload + 28` | Minimum acceptable path MTU in bytes. The probe reports `degraded` when the discovered MTU is below this threshold.                                  |
| `mtu_retries`             | int      | `3`                    | Number of retries per payload size before giving up on that size.                                                                                    |
| `mtu_per_attempt_timeout` | duration | `2s`                   | Per-attempt timeout for each ICMP echo request. Must be ≤ target `timeout`.                                                                          |

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

Each DNS probe execution performs a resolver lookup. NetSonar does not keep an
application-level DNS result cache for `probe_type: dns`. When `dns_server` is
empty, the lookup uses the system resolver path, so operating-system, local
forwarder, or upstream recursive resolver caching may still affect the observed
latency and returned records. When `dns_server` is set, NetSonar sends the
lookup through that configured resolver, but caching by that resolver remains
operator-controlled.

`dns_server` must be an IP literal (e.g. `"8.8.8.8:53"`, or `"8.8.8.8"` —
port `:53` is auto-appended). Hostnames are rejected at config load time
to keep `probe_dns_resolve_seconds` measurements honest — see
[Resolver selection](#resolver-selection) for the full rationale and
migration guidance.

`dns_resolver` (agent or target level) does not affect this probe type:
`dns_server` is required to be an IP literal, so there is no pre-resolution
step where a resolver override could intervene.

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

### DNS Options

| Option           | Type       | Default     | Description                                                                                                            |
|------------------|------------|-------------|------------------------------------------------------------------------------------------------------------------------|
| `dns_query_name` | string     | `address`   | DNS name to query. Falls back to the target's `address` when omitted.                                                  |
| `dns_query_type` | string     | `A`         | DNS record type. One of `A`, `AAAA`, `CNAME`.                                                                          |
| `dns_server`     | string     | `""`        | Resolver to query in `host:port` form. **Must be an IP literal**; hostnames are rejected. Empty uses the system resolver (`/etc/resolv.conf`). |
| `dns_expected`   | `[]string` | `[]`        | Expected result set. When non-empty, the response must match exactly (order-independent). See subsection below.        |

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

### TLS Cert Options

| Option            | Type   | Default | Description                                                                                                                 |
|-------------------|--------|---------|-----------------------------------------------------------------------------------------------------------------------------|
| `tls_skip_verify` | bool   | `false` | Skip TLS certificate verification on the target connection. Useful for monitoring expiry of self-signed or untrusted certs. |
| `proxy_url`       | string | `""`    | Route the TLS handshake through an HTTP CONNECT tunnel. The recorded chain is whatever NetSonar observes from that path.    |

The reported expiry is based on the certificate chain observed by NetSonar from that network path. With a normal CONNECT proxy this is the origin chain. With TLS inspection, the observed chain may be proxy-issued; this is useful when the operational question is what workloads behind that proxy actually see.

Direct probes emit `probe_phase_duration_seconds` with `phase="tcp_connect"` and `phase="tls_handshake"`, plus `phase="dns_resolve"` for hostname targets. Proxy-path probes emit `proxy_dial`, optional `proxy_tls` (for `https://` proxies), `proxy_connect`, and `tls_handshake` for the target handshake through the tunnel.

## HTTP Body Validation

HTTP request with regex or substring match on the response body. The probe succeeds (`probe_success=1`) when **both** conditions are met: the response body matches the configured pattern, and the response status code satisfies the `expected_status_codes` rule (empty list accepts any status; non-empty list requires the status code to be present). When body evaluation completes, the match result is also reported separately via `probe_http_body_match` for diagnostic visibility. If no HTTP response is received or the body cannot be evaluated, `probe_http_body_match` is absent. The agent reads at most 1 MiB of response body; larger responses fail the probe. Supports custom request headers via `headers` and optional proxy routing via `proxy_url`.

`http_body` probes emit the same per-phase timings as `http` through `probe_phase_duration_seconds` (`dns_resolve` for hostname targets, `tcp_connect`, `tls_handshake` for HTTPS targets, `request_write`, `ttfb`, `transfer`). Use the HTTP Response Body Status per Target panel for the current status, HTTP code, and body-match result, and use the phase panels to distinguish request-write, TTFB, transfer, and proxy setup time. The main Grafana status table intentionally stays cross-probe and shows status, timeout, total probe time, timeout limit, limit-used ratio, and target identity labels.

The 1 MiB body cap is fixed for `http_body` (unlike `http`, which exposes the configurable `response_body_limit_bytes` and reports truncation via `probe_http_response_truncated` without failing). For `http_body` the `transfer` phase still measures body-read time, but a response body exceeding 1 MiB fails the probe instead of being truncated.

At least one of `body_match_regex` or `body_match_string` must be set — a target without either is rejected at config load time. `body_match_regex` must be a valid Go regular expression. It is validated when the config is loaded and compiled once when the target prober is created. If both `body_match_regex` and `body_match_string` are set, the regex takes precedence.

```yaml
- name: "api-health-body"
  address: "https://api.example.com/health"
  probe_type: http_body
  timeout: 5s
  probe_opts:
    method: GET                     # GET, HEAD, POST (default: GET)
    headers:                        # Custom request headers
      X-Custom: "value"
    body_match_string: "ok"         # Substring match
    body_match_regex: "status.*ok"  # Regex match (alternative)
    proxy_url: ""                   # Optional: route through HTTP proxy
```

### HTTP Body Options

| Option                  | Type                | Default | Description                                                                                                                          |
|-------------------------|---------------------|---------|--------------------------------------------------------------------------------------------------------------------------------------|
| `method`                | string              | `GET`   | HTTP method. One of `GET`, `HEAD`, `POST`.                                                                                           |
| `headers`               | `map[string]string` | `{}`    | Custom request headers. Sent on every probe request.                                                                                 |
| `body_match_string`     | string              | `""`    | Substring that must appear in the response body. At least one of `body_match_string` / `body_match_regex` is required.               |
| `body_match_regex`      | string              | `""`    | Go regular expression matched against the response body. Takes precedence over `body_match_string` when both are set.                |
| `expected_status_codes` | `[]int`             | `[]`    | Allow-list of HTTP status codes that count as success in addition to the body match. Empty list accepts any status.                  |
| `proxy_url`             | string              | `""`    | Route the request through an HTTP forward proxy.                                                                                     |

## Proxy Connectivity

Establishes a raw HTTP CONNECT tunnel through a configured proxy and measures tunnel establishment time. This probe type tests the proxy's ability to create TCP tunnels, not regular HTTP forwarding.

```yaml
- name: "egress-proxy-connect"
  address: "example.com:443"
  probe_type: proxy_connect
  timeout: 5s
  probe_opts:
    proxy_url: "http://fwd-proxy.example.internal:8888"
    # Optional negative test: success means the proxy returned CONNECT 403.
    # expected_proxy_connect_status_codes: [403]
```

### Proxy CONNECT Options

| Option                                | Type    | Default      | Description                                                                                                                |
|---------------------------------------|---------|--------------|----------------------------------------------------------------------------------------------------------------------------|
| `proxy_url`                           | string  | **required** | HTTP forward proxy URL (`http://` or `https://`). May contain Basic-auth credentials.                                      |
| `expected_proxy_connect_status_codes` | `[]int` | `[]`         | Allow-list of CONNECT response codes that count as success. Empty list means any 2xx is success. Use for negative tests.   |

If the proxy URL contains credentials, the CONNECT request includes `Proxy-Authorization: Basic ...`.

Proxy CONNECT probes expose `probe_proxy_connect_status_code` when the proxy returns a CONNECT response. With no explicit expectation, any 2xx CONNECT response is success. When `expected_proxy_connect_status_codes` is set, success means the proxy's CONNECT response status is in that list. This is intended for explicit negative tests such as Squid ACL denials returning 403.

Proxy CONNECT probes expose phase timings regardless of whether the CONNECT succeeded or failed, which helps diagnose where time is spent when a proxy rejects the tunnel:

| Phase | What It Measures |
|---|---|
| `proxy_dial` | TCP dial to the proxy |
| `proxy_tls` | TLS handshake with the proxy, only for `https://` proxy URLs |
| `proxy_connect` | CONNECT request write and proxy response read |

### Proxy CONNECT Probe vs HTTP Probe with `proxy_url`

The agent offers two distinct ways to test proxy connectivity. Choosing the wrong one leads to false failures.

**`probe_type: proxy_connect`** — sends an `HTTP CONNECT` request to the proxy, asking it to open a raw TCP tunnel to the target. The proxy does not see or interpret the traffic inside the tunnel. Many forward proxies (Squid, Tinyproxy) restrict or disable CONNECT to prevent arbitrary protocol tunnelling. If the target host is not on the proxy's CONNECT allowlist, the probe fails even though regular HTTP forwarding through the same proxy works fine.

Use `proxy_connect` when:

- Testing SSH-over-proxy, WebSocket, or other non-HTTP protocols tunnelled through CONNECT
- Verifying the proxy's CONNECT allowlist (positive and negative tests)
- Measuring raw tunnel establishment time without TLS or HTTP overhead

**`probe_type: http` with `proxy_url`** — sends a standard HTTP request routed through the configured proxy. For HTTPS targets the prober performs an explicit CONNECT + target TLS handshake + HTTP exchange and measures each step independently so CONNECT latency is visible. For plain HTTP targets the prober uses standard HTTP forward proxying (no CONNECT) — the proxy receives the HTTP request in absolute-URI form.

For `http` and `http_body`, `expected_status_codes` always refers to the target HTTP response, not the proxy's CONNECT response. If an HTTPS proxy CONNECT happens, `probe_proxy_connect_status_code` is diagnostic only and does not change `probe_success`.

**`probe_type: tls_cert` with `proxy_url`** — sends CONNECT to the proxy, then performs only the target TLS handshake through the tunnel and records the observed certificate expiry. It does not send an HTTP request to the target.

Use `http` with `proxy_url` when:

- Testing that a forward proxy can reach a target endpoint (the common case)
- Verifying proxy connectivity for HTTP/HTTPS traffic as clients actually use it
- You need full HTTP metrics (status code and phase timing, with TLS certificate expiry when `tls_emit_cert_metrics: true`)

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

When an `http` or `http_body` probe uses `proxy_url`, the phase timing metrics reflect the full proxy path. Phase emission depends on the combination of target URL scheme and proxy URL scheme. `tcp_connect` and `proxy_dial` are **mutually exclusive** per probe execution: direct probes emit `tcp_connect`, proxy-path probes emit `proxy_dial` instead. `dns_resolve` is **not** emitted for proxy-path `http`/`http_body` because proxy hostname resolution is included in `proxy_dial` and the target hostname is resolved by the proxy itself.

| Target URL  | Proxy URL   | Proxy Phases Emitted                                            | Target Phases Emitted |
|-------------|-------------|-----------------------------------------------------------------|-----------------------|
| `http://`   | `http://`   | `proxy_dial`                                                    | `request_write`, `ttfb`, `transfer` |
| `http://`   | `https://`  | `proxy_dial`, `proxy_tls`                                       | `request_write`, `ttfb`, `transfer` |
| `https://`  | `http://`   | `proxy_dial`, `proxy_connect`                                   | `tls_handshake`, `request_write`, `ttfb`, `transfer` |
| `https://`  | `https://`  | `proxy_dial`, `proxy_tls`, `proxy_connect`                      | `tls_handshake`, `request_write`, `ttfb`, `transfer` |

Individual phase meaning when `network_path="proxy"`:

| Phase            | What It Measures (`network_path="proxy"`) |
|------------------|-------------------------------------------|
| `proxy_dial`     | TCP dial to the proxy (includes proxy hostname DNS resolution) |
| `proxy_tls`      | TLS handshake with the proxy (only for `https://` proxies) |
| `proxy_connect`  | CONNECT request write + proxy response read (only for HTTPS targets through a proxy) |
| `tls_handshake`  | TLS handshake with the target through the tunnel (only for HTTPS targets) |
| `request_write`  | Time from connection ready to request write completion |
| `ttfb`           | Time from request write completion to first response byte |
| `transfer`       | Response body read up to the effective response body limit (through the tunnel) |

CONNECT request/response latency is reported in `proxy_connect`, not `tcp_connect` — compare a proxy-path probe's `proxy_connect` against a `probe_type: proxy_connect` probe's `proxy_connect` to isolate target-side CONNECT handling from pure proxy overhead.

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
- The "HTTP Probes (Proxy)" section filters by `probe_type="http", network_path="proxy"`.
- The "HTTP Phase Timing (Proxy)" panel shows HTTP phase timings for proxy-path HTTP probes.
- The "HTTP Response Body Probes" section shows `probe_type="http_body"` status, body-match, duration, and phase panels.
- The "Proxy CONNECT Probes" section filters by `probe_type="proxy_connect"`.
- The "Proxy CONNECT Phase Timing" panel shows raw CONNECT probe phases: `proxy_dial`, `proxy_tls`, and `proxy_connect`.
- PromQL filter: `probe_success{network_path="proxy"}` selects all proxy-path probes regardless of probe type or service name.

For a detailed explanation of how HTTP proxies work, when to use each probe type, and how to interpret proxy-path metrics, see [proxy-probing.md](proxy-probing.md).
