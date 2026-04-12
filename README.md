# NetSonar

[![CI](https://github.com/luk-kop/netsonar/actions/workflows/ci.yml/badge.svg)](https://github.com/luk-kop/netsonar/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/luk-kop/netsonar)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

A purpose-built Go binary that probes a YAML-configured list of network targets and exposes results as Prometheus metrics on a `/metrics` endpoint. It replaces both the Telegraf-native probe plugins and the Blackbox Exporter PoC with a single, lightweight agent.

Telegraf scrapes the agent with a single `inputs.prometheus` block and forwards metrics to Kafka (primary) and VictoriaMetrics (ICE fallback). The existing pipeline remains unchanged.

## Table of Contents

- [Overview](#overview)
- [Build](#build)
- [Usage](#usage)
- [Configuration Reference](#configuration-reference)
- [Probe Types](#probe-types)
- [Metrics Reference](#metrics-reference)
- [Deployment](#deployment)
- [Telegraf Scrape Configuration](#telegraf-scrape-configuration)
- [Operations](#operations)

## Overview

The agent runs as a single static binary (~20-30 MB RSS for up to 100 targets), requires no external dependencies, and uses `CAP_NET_RAW` only for MTU probes (ICMP uses unprivileged sockets). It owns its target list internally and exposes pre-labelled metrics, unlike the Blackbox Exporter's multi-target `/probe?target=X&module=Y` pattern.

Supported probe types: TCP, HTTP/HTTPS, ICMP, MTU/PMTUD, DNS, TLS certificate expiry, HTTP body validation, proxy connectivity.

## Build

Requires Go 1.26+.

```bash
cd netsonar

# Build static binary (version injected from git tags)
make build

# Run all checks: fmt, vet, lint, test, build
make all
```

The binary is written to `bin/netsonar`.

### Make Targets

| Target      | Description                                              |
|-------------|----------------------------------------------------------|
| `build`     | Static binary with `CGO_ENABLED=0` and version injection |
| `test`      | Run all tests (`go test ./...`)                          |
| `test-race` | Run tests with race detector                             |
| `test-pbt`  | Run property-based tests only (`-run Property`)          |
| `lint`      | Run `golangci-lint`                                      |
| `fmt`       | Format code with `gofmt -s -w`                           |
| `vet`       | Run `go vet`                                             |
| `clean`     | Remove `bin/` directory                                  |
| `all`       | `fmt` + `vet` + `lint` + `test` + `build`               |

## Usage

```bash
# Start with default config path
./bin/netsonar --config /etc/netsonar/config.yaml

# Override listen address
./bin/netsonar --config config.yaml --listen-addr :9275
```

### CLI Flags

| Flag            | Default                       | Description                        |
|-----------------|-------------------------------|------------------------------------|
| `--config`      | `/etc/netsonar/config.yaml`   | Path to YAML configuration file    |
| `--listen-addr` | (from config, or `:9275`)     | Override `agent.listen_addr`       |

## Configuration Reference

See `config.example.yaml` for a complete working example.

### Agent Settings

```yaml
agent:
  listen_addr: ":9275"          # HTTP listen address for /metrics
  metrics_path: "/metrics"      # Metrics endpoint path
  default_interval: 30s         # Default probe interval (applied when target omits interval)
  default_timeout: 5s           # Default probe timeout (applied when target omits timeout)
  default_icmp_payload_sizes:   # Default ICMP payload sizes for MTU probes (descending)
    [1472, 1392, 1372, 1272, 1172, 1072]
  log_level: info               # Log level: debug, info, warn, error
  allowed_tag_keys:             # Optional: restrict tag keys to this allowlist
    - service
    - scope
    - provider
    - target_region
    - target_partition
    - visibility
    - port
    - criticality
```

When `allowed_tag_keys` contains entries, targets may only use tag keys from this list. When absent or empty, the agent collects tag keys dynamically from all targets (limited to 30 unique keys).

### Target Definition

```yaml
targets:
  - name: "api-gw-pub-eu"                                              # Unique identifier (required)
    address: "api-gw-pub.example.internal:443"                               # Target address (required)
    probe_type: tcp                                                     # Probe type (required)
    interval: 30s                                                       # Override agent default_interval
    timeout: 3s                                                         # Override agent default_timeout (must be ≤ interval)
    tags:                                                               # Prometheus labels (dynamic, max 20)
      scope: same-region
      service: api-gw-pub
      provider: aws
      target_region: eu-central-1
      target_partition: global
      visibility: public
      port: "443"
      criticality: critical
    probe_opts:                                                         # Probe-type-specific options
      # (see Probe Types section)
```

#### Dynamic Tags

Tag keys are not hardcoded in the agent binary. They are collected dynamically from the configuration at startup and used as Prometheus label names. See [Dynamic Labels](#dynamic-labels) in the Metrics Reference for details.

When `allowed_tag_keys` is configured, only those keys are permitted — any target using a key outside the list is rejected at config load time. When `allowed_tag_keys` is absent or empty, the agent collects keys dynamically from all targets, subject to a safety limit of 30 unique keys (`MaxGlobalTagKeys`).

All tag keys (whether from the allowlist or collected dynamically) must be valid Prometheus label names (`[a-zA-Z_][a-zA-Z0-9_]*`) and must not collide with fixed labels (`target`, `target_name`, `probe_type`, `proxied`).

### Validation Rules

- `name` must be unique across all targets
- `address` must be non-empty
- `probe_type` must be one of: `tcp`, `http`, `icmp`, `mtu`, `dns`, `tls_cert`, `http_body`, `proxy`
- After defaults are applied, `interval` must be > 0 (set `target.interval` or `agent.default_interval`)
- After defaults are applied, `timeout` must be > 0 (set `target.timeout` or `agent.default_timeout`)
- `timeout` must be ≤ `interval`
- `tags` must have at most 20 entries per target
- Tag keys must be valid Prometheus label names (`[a-zA-Z_][a-zA-Z0-9_]*`)
- Tag keys must not collide with fixed labels (`target`, `target_name`, `probe_type`, `proxied`)
- `allowed_tag_keys` must not contain duplicates
- In dynamic mode (no allowlist), at most 30 unique tag keys across all targets
- `icmp` and `mtu` reject literal IPv6 addresses because these probes currently use IPv4-only ICMP sockets
- `icmp_payload_sizes` must be sorted in descending order
- `dns_query_type` must be one of: `A`, `AAAA`, `CNAME`
- For `http` and `http_body`, `method` must be one of: `GET`, `HEAD`, `POST`; an empty value defaults to `GET`
- For `http` and `http_body`, every `expected_status_codes` value must be a valid HTTP status code in the range `100`-`599`; an empty list accepts any fully received response
- For `http_body`, `body_match_regex` must be a valid Go regular expression
- `proxy_url` is required when `probe_type` is `proxy`; optional for `http` and `http_body`
- When set, `proxy_url` must be `http://[user:pass@]host[:port]` or `https://[user:pass@]host[:port]`; paths other than `/`, query strings, fragments, invalid ports, relative URLs, and non-HTTP schemes are rejected
- If `proxy_url` includes `user:pass@`, the credentials are used for proxy Basic authentication; `proxy` probes send them as `Proxy-Authorization` on the CONNECT request

## Probe Types

### TCP

Measures TCP connection establishment time.

```yaml
- name: "rds-postgres"
  address: "rds-endpoint.eu-central-1.rds.amazonaws.com:5432"
  probe_type: tcp
  timeout: 3s
```

No `probe_opts` required.

### HTTP/HTTPS

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

#### expected_status_codes

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

### ICMP

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

### MTU/PMTUD

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

#### Default Payload Sizes

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

For a detailed explanation of PMTUD, ICMP Destination Unreachable codes, PMTUD black holes, and how the MTU probe interprets results, see [docs/mtu-pmtud.md](docs/mtu-pmtud.md).

### DNS

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

### TLS Certificate Expiry

Performs a TLS handshake and extracts the leaf certificate's `NotAfter` timestamp.

```yaml
- name: "api-gw-pub-eu-tls"
  address: "api-gw-pub.example.internal:443"
  probe_type: tls_cert
  interval: 300s
  timeout: 5s
```

### HTTP Body Validation

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

### Proxy Connectivity

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

#### Proxy Probe vs HTTP Probe with `proxy_url`

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

#### Interpreting Metrics for Proxied HTTP Probes

When an HTTP probe uses `proxy_url`, the phase timing metrics reflect the full proxied path:

| Phase | What It Measures (proxied) |
|---|---|
| `tcp_connect` | TCP dial to proxy + CONNECT tunnel establishment to target |
| `tls_handshake` | TLS handshake with the target (through the tunnel) |
| `ttfb` | Time to first byte from the target (through the tunnel) |
| `transfer` | Response body transfer (through the tunnel) |

The `tcp_connect` phase is notably higher for proxied probes compared to direct ones because it includes both the connection to the proxy and the CONNECT handshake. This is expected and can be used to estimate proxy overhead by comparing `tcp_connect` of a proxied probe against a direct probe to the same target.

#### Identifying Proxied Probes on the Dashboard

The agent automatically adds a `proxied` label to every metric: `"true"` when the target has a `proxy_url` configured, `"false"` otherwise. This requires no manual configuration — the agent detects it from `probe_opts.proxy_url`.

On the Grafana dashboard:
- The "All Probes — Status Table" includes a "Proxied" column that shows "YES" (orange) for proxied probes and is blank for direct ones.
- The "Proxied HTTP Probes" section filters by `probe_type="http", proxied="true"`.
- The "HTTP Phase Timing (Proxied)" panel shows HTTP phase timings for proxied HTTP probes.
- The "Proxy CONNECT Probes" section filters by `probe_type="proxy"`.
- The "Proxy CONNECT Phase Timing" panel shows raw CONNECT probe phases: `proxy_dial`, `proxy_tls`, and `proxy_connect`.
- PromQL filter: `probe_success{proxied="true"}` selects all proxied probes regardless of probe type or service name.

For a detailed explanation of how HTTP proxies work, when to use each probe type, and how to interpret proxied metrics, see [docs/proxy-probing.md](docs/proxy-probing.md).

## Metrics Reference

Every probe metric carries two kinds of labels: fixed labels set by the agent automatically, and dynamic labels derived from the target's `tags` map in the configuration.

### Fixed Labels

These labels are hardcoded in the agent binary and applied to every metric automatically. They cannot be removed or renamed via configuration.

| Label         | Source                  | Description                                      |
|---------------|-------------------------|--------------------------------------------------|
| `target`      | `address` field         | Target address (e.g. `https://ssm.eu-central-1.amazonaws.com`) |
| `target_name` | `name` field            | Unique target name from config (e.g. `egress-proxy-ok`) |
| `probe_type`  | `probe_type` field      | Probe type (e.g. `tcp`, `http`, `proxy`)         |
| `proxied`     | auto from `proxy_url`   | `"true"` if target uses a proxy, `"false"` otherwise |

### Dynamic Labels

When `allowed_tag_keys` is configured, the agent uses that list directly as the dynamic label schema. Targets may only use keys from the allowlist, and targets that do not define a particular allowed key get an empty string as the label value.

When `allowed_tag_keys` is absent or empty, the agent falls back to dynamic mode: it collects all unique tag keys from every target in the configuration and registers them as Prometheus label names. This means adding a new label (e.g. `target_account`, `team`, `environment`) requires only a configuration change, subject to the global safety limit below.

**Limits:** Each target may have at most **20 tags** (`MaxTagsPerTarget`). In dynamic mode (no allowlist), at most **30 unique tag keys** across all targets (`MaxGlobalTagKeys`). Keep the number of unique tag keys low to avoid high label cardinality in the TSDB.

**Reload:** Changing `agent.allowed_tag_keys` requires restarting the agent. SIGHUP reload supports target changes and tag values within the existing key set.

**Example:** Given these two targets:

```yaml
targets:
  - name: api-gw
    tags: { service: api-gw, scope: same-region, criticality: critical }
  - name: bastion-cn
    tags: { service: bastion, scope: cross-region }
```

The agent registers three dynamic labels: `service`, `scope`, `criticality`. The `bastion-cn` target gets `criticality=""` because it does not define that key.

### Probe Metrics

| Metric                              | Type  | Labels          | Description                                    |
|-------------------------------------|-------|-----------------|------------------------------------------------|
| `probe_success`                     | Gauge | common          | 1 if probe succeeded, 0 if failed              |
| `probe_duration_seconds`            | Gauge | common          | Total probe duration                           |
| `probe_phase_duration_seconds`      | Gauge | common + `phase`| Per-phase timing for probes with sub-phases    |
| `probe_http_status_code`            | Gauge | common          | HTTP response status code                      |
| `probe_tls_cert_expiry_timestamp`   | Gauge | common          | Unix timestamp of TLS certificate expiry       |
| `probe_icmp_packet_loss_ratio`      | Gauge | common          | Packet loss ratio 0.0-1.0                      |
| `probe_icmp_avg_rtt_seconds`        | Gauge | common          | Average ICMP echo round-trip time              |
| `probe_icmp_hop_count`              | Gauge | common          | TTL / hop count from ICMP reply                |
| `probe_mtu_path_bytes`              | Gauge | common          | Legacy detected path MTU in bytes (-1 if all failed) |
| `probe_mtu_bytes`                   | Gauge | common          | Largest confirmed MTU in bytes                 |
| `probe_mtu_state`                   | Gauge | common + `state`, `detail` | MTU state info metric, value is always 1 |
| `probe_mtu_frag_needed_total`       | Counter | common        | Matched ICMP fragmentation-needed responses    |
| `probe_mtu_timeouts_total`          | Counter | common        | MTU probe attempts that timed out              |
| `probe_mtu_retries_total`           | Counter | common        | Additional MTU attempts after the first attempt |
| `probe_mtu_local_errors_total`      | Counter | common        | Local host/kernel send errors, such as EMSGSIZE |
| `probe_skipped_overlap_total`       | Counter | common        | Probe executions skipped due to stale tick after a long-running probe |
| `probe_dns_resolve_seconds`         | Gauge | common          | DNS resolution time                            |
| `probe_dns_result_match`            | Gauge | common          | 1 if DNS result matches expected, 0 otherwise  |
| `probe_http_body_match`             | Gauge | common          | 1 if body matches pattern, 0 otherwise         |

### Agent Metadata Metrics

| Metric                              | Type  | Labels      | Description                                              |
|-------------------------------------|-------|-------------|----------------------------------------------------------|
| `agent_info`                        | Gauge | `version`   | Agent build info (always 1)                              |
| `agent_config_info`                 | Gauge | `hash`      | Short SHA256 hash of the effective configuration (always 1) |
| `agent_targets_total`               | Gauge | -           | Total number of configured targets                       |
| `agent_config_reload_timestamp`     | Gauge | -           | Unix timestamp of last config reload                     |

#### `agent_config_info`

The hash is computed over the effective configuration **after** defaults
have been applied and validation has passed, not over the raw YAML bytes.
`Targets` are sorted by `name` before hashing, so reordering targets in the
YAML file does not change the hash. Whitespace, comments, and key order in
the YAML file are irrelevant.

The hash is emitted as the first 12 hex characters of SHA256 and is also
written to the agent log at startup and after every successful reload. Use
it to verify that:

- multiple agent instances are running the same effective configuration,
- a `SIGHUP` reload actually picked up the new configuration,
- an agent was not left behind on a stale configuration after a rollout.

On reload, the previous series is `Reset()` so `/metrics` only ever exposes
the hash of the currently active configuration.

### Phase Labels

The `probe_phase_duration_seconds` metric uses a `phase` label with these values:

| Phase           | Probe Type | Description                          |
|-----------------|------------|--------------------------------------|
| `dns_resolve`   | HTTP       | DNS resolution time                  |
| `tcp_connect`   | HTTP       | TCP connection establishment         |
| `tls_handshake` | HTTP       | TLS handshake (HTTPS only)           |
| `ttfb`          | HTTP       | Time to first byte                   |
| `transfer`      | HTTP       | Response body transfer time          |
| `proxy_dial`    | Proxy      | TCP dial to the proxy                |
| `proxy_tls`     | Proxy      | TLS handshake with the proxy         |
| `proxy_connect` | Proxy      | CONNECT request and response         |

## Deployment

### Prerequisites

- Go 1.26+ (build only)
- Linux with `CAP_NET_RAW` capability (only for MTU probes; ICMP uses unprivileged sockets)
- Dedicated `netsonar` system user

### Install

```bash
# Build
cd netsonar
make build

# Copy binary
sudo cp bin/netsonar /usr/local/bin/

# Create config directory and user
sudo useradd --system --no-create-home --shell /usr/sbin/nologin netsonar
sudo mkdir -p /etc/netsonar
sudo cp config.example.yaml /etc/netsonar/config.yaml
sudo chown -R netsonar:netsonar /etc/netsonar

# Install systemd unit
sudo cp netsonar.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now netsonar
```

### systemd Unit

The included `netsonar.service` provides:

- Runs as `netsonar` user (non-root)
- `CAP_NET_RAW` ambient capability for MTU probes (ICMP uses unprivileged sockets)
- Security hardening: `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome=true`, `PrivateTmp=true`, `MemoryDenyWriteExecute=true`
- Automatic restart on failure with 5-second delay
- Config reload via `systemctl reload netsonar` (sends SIGHUP)

### Verify

```bash
# Check service status
sudo systemctl status netsonar

# Liveness / readiness
curl -s http://localhost:9275/healthz
curl -s http://localhost:9275/readyz

# Scrape metrics
curl -s http://localhost:9275/metrics | head -20

# Check agent metadata
curl -s http://localhost:9275/metrics | grep agent_
```

`/healthz` returns `200 ok` when the HTTP server is running. `/readyz` returns `200 ok` after the scheduler has started; before readiness it returns `503 not ready`.

### Container Deployment

NetSonar runs well in containers. The only consideration is `CAP_NET_RAW` — required only for MTU probes. All other probe types (TCP, HTTP, ICMP, DNS, TLS, proxy) work without any special capabilities.

Docker without MTU probes (least-privilege):

```bash
docker run --rm \
  --cap-drop=ALL \
  --read-only \
  -v /path/to/config.yaml:/etc/netsonar/config.yaml:ro \
  -p 9275:9275 \
  netsonar:latest
```

Docker with MTU probes:

```bash
docker run --rm \
  --cap-drop=ALL \
  --cap-add=NET_RAW \
  --read-only \
  -v /path/to/config.yaml:/etc/netsonar/config.yaml:ro \
  -p 9275:9275 \
  netsonar:latest
```

Kubernetes security context (without MTU probes):

```yaml
securityContext:
  runAsNonRoot: true
  readOnlyRootFilesystem: true
  allowPrivilegeEscalation: false
  capabilities:
    drop: [ALL]
```

For MTU probes, add `capabilities: { add: [NET_RAW] }`. ICMP probes use unprivileged sockets and require `net.ipv4.ping_group_range` to include the process GID (default on most distributions).

For the full container deployment guide covering Kubernetes manifests, rootless Podman, `ping_group_range` per-pod configuration, and troubleshooting, see [docs/container-deployment.md](docs/container-deployment.md).

## Telegraf Scrape Configuration

The agent replaces the per-target `inputs.prometheus` blocks (Blackbox Exporter) and per-target `inputs.net_response` / `inputs.http_response` blocks (native Telegraf) with a single scrape endpoint.

Add this to your Telegraf configuration (e.g. `/etc/telegraf/telegraf.d/netsonar.conf`):

```toml
# NetSonar — single scrape endpoint replaces all per-target blocks
[[inputs.prometheus]]
  urls = ["http://localhost:9275/metrics"]
  metric_version = 2
  [inputs.prometheus.tags]
    monitor = "network-monitor"
```

All target labels (`target_name`, `service`, `scope`, `provider`, `target_region`, etc.) are already embedded in the metrics by the agent. No additional tag configuration is needed in Telegraf.

### Output Configuration

The existing output pipeline remains unchanged:

```toml
# Primary: Kafka
[[outputs.kafka]]
  brokers = ["kafka-broker:9092"]
  topic = "telegraf-metrics"
  data_format = "prometheusremotewrite"

# ICE fallback: local VictoriaMetrics
[[outputs.http]]
  url = "http://localhost:8428/api/v1/write"
  data_format = "prometheusremotewrite"
```

## Operations

### Configuration Reload

Reload the configuration without restarting the agent:

```bash
# Via systemd
sudo systemctl reload netsonar

# Via signal
sudo kill -HUP $(pidof netsonar)
```

The agent diffs the new configuration against the running state: removed targets are stopped, new targets are started, changed targets are restarted, and unchanged targets continue without interruption. When a target is removed or its configuration changes, the agent also deletes the corresponding time series from `/metrics`, so dashboards and alerting never see stale values from targets that no longer exist. If the new configuration is invalid, the agent continues with the previous configuration and logs the error.

If the new configuration changes the effective set of tag keys (either via `allowed_tag_keys` or dynamically collected from targets), the reload is rejected with a log message: `config reload rejected: tag key set changed; restart required`. This is because Prometheus label names are fixed at startup and cannot be changed without recreating the metrics exporter.

### Probe Failure Logs

Probe failure reasons are written to the agent log with structured fields. Metrics intentionally expose only stable values such as `probe_success`, duration, status code, and probe-specific gauges; the raw error string is not exported as a Prometheus label because it can have high cardinality.

The scheduler logs state changes per target:

| Event | Level | Message |
|---|---|---|
| First failed probe for a target | `warn` | `probe failed` |
| Target changes from success to failure | `warn` | `probe failed` |
| Failed target reports a different error string | `warn` | `probe failed` |
| Failed target repeats the same error | `debug` | `probe still failing` |
| Target recovers from failure | `info` | `probe recovered` |

Failure and recovery logs include `target_name`, `target`, `probe_type`, and `duration`. Failure logs also include `error`, for example:

```text
level=WARN msg="probe failed" target_name=egress-proxy target=https://example.com probe_type=proxy duration=23ms error="proxy CONNECT returned status 407"
```

Use `log_level: debug` only when repeated identical failures are needed for investigation. At the default `info` level, repeated identical failures are suppressed after the first warning until the error changes or the target recovers.

### Graceful Shutdown

```bash
sudo systemctl stop netsonar
```

On `SIGTERM` or `SIGINT`, the agent cancels all probe goroutines and allows the HTTP server a 5-second grace period for in-flight scrapes before exiting cleanly.

### Troubleshooting

| Symptom                                  | Cause                                    | Fix                                                    |
|------------------------------------------|------------------------------------------|--------------------------------------------------------|
| MTU probes show `probe_success=0`        | Missing `CAP_NET_RAW`                    | Verify systemd unit has `AmbientCapabilities=CAP_NET_RAW` |
| ICMP probes show `probe_success=0`       | `ping_group_range` excludes process GID  | `sysctl -w net.ipv4.ping_group_range="0 2147483647"` |
| No metrics on `/metrics`                 | Agent not running or wrong listen address| Check `systemctl status` and `--listen-addr` flag      |
| Probe shows `probe_success=0`             | DNS, TCP, TLS, HTTP, proxy, or permission failure | Check `journalctl -u netsonar` for `probe failed` |
| Config reload ignored                    | Invalid YAML in new config               | Check agent logs (`journalctl -u netsonar`) |
| Config reload rejected                   | Tag key set changed since startup        | Restart the agent; SIGHUP cannot change label schema   |
| High memory usage                        | Too many targets or label cardinality    | Keep targets under 100; keep unique tag keys consistent across targets |
| Config rejected at startup               | A target exceeds 20 tags                 | Reduce tag count to ≤ 20 per target                   |
