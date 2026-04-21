# Metrics Reference

Every probe metric carries two kinds of labels: fixed labels set by the agent automatically, and dynamic labels derived from the target's `tags` map in the configuration.

## Design References

NetSonar's metric surface is intentionally Prometheus-native and borrows from existing probe exporters where they overlap with NetSonar's scope:

- **Prometheus naming and instrumentation guidance**: metric names use base units and plural unit suffixes such as `_seconds` and `_bytes`; missing current observations are represented by absent series rather than in-band sentinel values where that keeps aggregations safe.
- **Prometheus Blackbox Exporter**: used as the closest pull-model reference for HTTP, DNS, ICMP, and TLS probing semantics. Examples include `probe_success` as the primary health signal, HTTP phase breakdown, and reporting the earliest TLS certificate expiry across the peer chain.
- **Telegraf input plugins**: used as a secondary reference because Telegraf is closer to NetSonar's "one agent, many check types" model. Its ping, DNS, HTTP response, and x509 certificate plugins informed which signals are operator-friendly and which labels would create unnecessary cardinality.
- **W3C Navigation Timing / browser tooling conventions**: used for HTTP timing semantics. In particular, NetSonar's `ttfb` phase is measured from request start to response start, matching browser timing and making phase data comparable with Chrome DevTools, k6, WebPageTest, and Blackbox Exporter.
- **`ping(8)` / iputils conventions**: used for ICMP packet-loss and RTT summary semantics, including calculating packet loss from packets actually sent and exposing RTT variation via standard deviation.

The MTU probe is custom: Blackbox Exporter and Telegraf do not provide a directly comparable MTU/PMTUD exporter. MTU metrics therefore follow NetSonar's probe contract plus general Prometheus conventions: keep aggregations safe, make degraded state visible through `probe_success`, and avoid exporting low-value internal retry counters.

## Metric Validation

Metric definitions and metric validation are documented separately. This
reference describes the metric surface and semantics; [metrics-validation.md](metrics-validation.md)
describes how those semantics are checked against independent tools such as
Prometheus Blackbox Exporter, `curl`, `dig`, `openssl`, and `ping`.

## Fixed Labels

These labels are hardcoded in the agent binary and applied to every metric automatically. They cannot be removed or renamed via configuration.

| Label          | Source                | Description                                      |
|----------------|-----------------------|--------------------------------------------------|
| `target`       | `address` field       | Target address (e.g. `https://ssm.eu-central-1.amazonaws.com`) |
| `target_name`  | `name` field          | Unique target name from config (e.g. `egress-proxy-ok`) |
| `probe_type`   | `probe_type` field    | Probe type (e.g. `tcp`, `http`, `proxy`)         |
| `network_path` | auto from `proxy_url` | `"proxy"` if target uses `proxy_url`, `"direct"` otherwise |

## Dynamic Labels

When `allowed_tag_keys` is configured, the agent uses that list directly as the dynamic label schema. Targets may only use keys from the allowlist, and targets that do not define a particular allowed key get an empty string as the label value.

When `allowed_tag_keys` is absent or empty, the agent falls back to dynamic mode: it collects all unique tag keys from every target in the configuration and registers them as Prometheus label names. This means adding a new label (e.g. `target_account`, `team`, `environment`) requires only a configuration change, subject to the global safety limit below.

**Limits:** Each target may have at most **20 tags** (`MaxTagsPerTarget`). The total number of unique tag keys is capped at **30** (`MaxGlobalTagKeys`) — this limit applies in both modes: in dynamic mode (no allowlist) it counts the keys collected from all targets, and in allowlist mode it limits the length of `allowed_tag_keys`. The reason for this cap is that every unique tag key becomes a Prometheus label on every metric series; too many labels degrade TSDB (Prometheus/Mimir) write and query performance. These limits apply only to user-defined tags — the **4 fixed labels** (`target`, `target_name`, `probe_type`, `network_path`) are always present and do not count towards these limits.

In dynamic mode, the effective maximum number of labels per probe metric series is **4 fixed labels + up to 20 discovered tag labels = 24**, because no single target may define more than 20 tags. In allowlist mode, the schema comes from `allowed_tag_keys`, so every probe metric series has **4 fixed labels + up to 30 allowlisted tag labels = 34**; targets that do not define a particular allowlisted key get an empty string value for that label.

**Reload:** Changing `agent.allowed_tag_keys` requires restarting the agent. SIGHUP reload supports target changes and tag values within the existing key set.

**Example:** Given these two targets:

```yaml
targets:
  - name: api-gw
    tags: { service: api-gw, scope: same-region, impact: critical }
  - name: bastion-cn
    tags: { service: bastion, scope: cross-region }
```

The agent registers three dynamic labels: `service`, `scope`, `impact`. The `bastion-cn` target gets `impact=""` because it does not define that key.

## Metric Naming Convention

Probe metric names follow the pattern `probe_<domain>_<measurement>_<unit>`, where `<domain>` identifies **what is being measured**, not which probe type emits the metric.

- **`probe_http_*`** — metrics about the HTTP protocol layer (status code, body match, response truncation).
- **`probe_tls_*`** — metrics about TLS certificates. Emitted by both `http` and `tls_cert` probe types, because both observe TLS certificates during their operation.
- **`probe_icmp_*`** — metrics about ICMP echo behaviour (RTT, packet loss). Emitted by both `icmp` and `mtu` probe types, because MTU probes use ICMP echo requests internally.
- **`probe_mtu_*`** — metrics specific to path MTU discovery (discovered MTU bytes, state).
- **`probe_dns_*`** — metrics about DNS resolution (resolve time, result match).

The `probe_type` label distinguishes which probe produced the observation. For example, `probe_icmp_avg_rtt_seconds{probe_type="icmp"}` is RTT from a dedicated ICMP probe, while `probe_icmp_avg_rtt_seconds{probe_type="mtu"}` is RTT from the ICMP echo requests that the MTU probe sends as part of its PMTUD algorithm.

Generic metrics without a domain prefix (`probe_success`, `probe_duration_seconds`, `probe_phase_duration_seconds`) are emitted by all or multiple probe types.

The following table shows which probe types emit each metric:

| Metric | Domain | Emitted by |
|---|---|---|
| `probe_success` | — | all |
| `probe_duration_seconds` | — | all |
| `probe_phase_duration_seconds` | — | http, proxy |
| `probe_http_status_code` | `http_` | http |
| `probe_http_response_truncated` | `http_` | http |
| `probe_http_body_match` | `http_` | http_body |
| `probe_tls_cert_expiry_timestamp_seconds` | `tls_` | http, tls_cert |
| `probe_tls_cert_chain_expiry_timestamp_seconds` | `tls_` | http, tls_cert |
| `probe_icmp_packet_loss_ratio` | `icmp_` | icmp |
| `probe_icmp_avg_rtt_seconds` | `icmp_` | icmp, mtu |
| `probe_icmp_stddev_rtt_seconds` | `icmp_` | icmp |
| `probe_mtu_bytes` | `mtu_` | mtu |
| `probe_mtu_state` | `mtu_` | mtu |
| `probe_dns_resolve_seconds` | `dns_` | dns |
| `probe_dns_result_match` | `dns_` | dns |
| `probe_skipped_overlap_total` | — | all |

## Probe Metrics

| Metric                              | Type  | Labels          | Description                                    |
|-------------------------------------|-------|-----------------|------------------------------------------------|
| `probe_success`                     | Gauge | common          | 1 if probe succeeded, 0 if failed              |
| `probe_duration_seconds`            | Gauge | common          | Total probe duration                           |
| `probe_phase_duration_seconds`      | Gauge | common + `phase`| Per-phase timing for probes with sub-phases    |
| `probe_http_status_code`            | Gauge | common          | HTTP response status code                      |
| `probe_tls_cert_expiry_timestamp_seconds` | Gauge | common    | Unix timestamp of earliest TLS certificate expiry in the peer chain |
| `probe_tls_cert_chain_expiry_timestamp_seconds` | Gauge | common + `cert_index`, `cert_role` | Unix timestamp of each peer certificate expiry |
| `probe_http_response_truncated`     | Gauge | common          | 1 if HTTP response body exceeded transfer limit, 0 otherwise |
| `probe_icmp_packet_loss_ratio`      | Gauge | common          | Packet loss ratio 0.0-1.0                      |
| `probe_icmp_avg_rtt_seconds`        | Gauge | common          | Average ICMP echo round-trip time (ICMP and MTU probes) |
| `probe_icmp_stddev_rtt_seconds`     | Gauge | common          | Population standard deviation of ICMP echo RTT |
| `probe_mtu_bytes`                   | Gauge | common          | Largest confirmed MTU in bytes                 |
| `probe_mtu_state`                   | Gauge | common + `state`, `detail` | MTU state info metric, value is always 1 |
| `probe_skipped_overlap_total`       | Counter | common        | Probe executions skipped due to stale tick after a long-running probe |
| `probe_dns_resolve_seconds`         | Gauge | common          | DNS resolution time                            |
| `probe_dns_result_match`            | Gauge | common          | 1 if DNS result matches expected, 0 otherwise  |
| `probe_http_body_match`             | Gauge | common          | 1 if body matches pattern, 0 otherwise         |

## Agent Metadata Metrics

| Metric                              | Type  | Labels      | Description                                              |
|-------------------------------------|-------|-------------|----------------------------------------------------------|
| `agent_info`                        | Gauge | `version`   | Agent build info (always 1)                              |
| `agent_config_info`                 | Gauge | `hash`      | Short SHA256 hash of the effective configuration (always 1) |
| `agent_targets_total`               | Gauge | -           | Total number of configured targets                       |
| `agent_config_reload_timestamp_seconds` | Gauge | -        | Unix timestamp of last config reload                     |

### `agent_config_reload_timestamp_seconds`

The timestamp is set both at **startup** (initial config load) and after
every successful **SIGHUP reload**. Because the initial load also sets the
gauge, `time() - agent_config_reload_timestamp_seconds` effectively tracks
agent uptime when no reloads have occurred.

### `agent_config_info`

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

## Phase Labels

The `probe_phase_duration_seconds` metric uses a `phase` label with these values:

| Phase           | Probe Type | Description                          |
|-----------------|------------|--------------------------------------|
| `dns_resolve`   | HTTP       | DNS resolution time                  |
| `tcp_connect`   | HTTP       | TCP connection establishment         |
| `tls_handshake` | HTTPS, TLS cert via proxy | TLS handshake with the target |
| `ttfb`          | HTTP       | Time to first byte — see note below  |
| `transfer`      | HTTP       | Body read time up to the effective transfer limit |

### TTFB semantics

`ttfb` is measured from the moment the connection is ready to send the HTTP request (after TCP for plain HTTP, after TLS handshake for HTTPS) until the first byte of the response is received. It captures server processing time plus response-header wire time, and does **not** overlap with `tls_handshake`.

This matches the W3C Navigation Timing API (`responseStart − requestStart`) and the Prometheus Blackbox Exporter `processing` phase, so NetSonar's phase breakdown is directly comparable to Chrome DevTools, k6, WebPageTest, and Blackbox. Phases are non-overlapping: `dns_resolve + tcp_connect + tls_handshake + ttfb + transfer ≈ probe_duration_seconds`.

| Phase           | Probe Type | Description                          |
|-----------------|------------|--------------------------------------|
| `proxy_dial`    | Proxy, TLS cert via proxy | TCP dial to the proxy                |
| `proxy_tls`     | Proxy, TLS cert via proxy | TLS handshake with the proxy         |
| `proxy_connect` | Proxy, TLS cert via proxy | CONNECT request and response         |

Direct `tls_cert` probes expose certificate metrics but do not currently emit phase timing. `tls_cert` probes through a proxy emit the proxy tunnel phases plus `tls_handshake` for the target TLS handshake.

## Conditional Metric Semantics

Some probe metrics are meaningful for every probe result, while others are meaningful only when a specific observation was made during the latest probe run.

NetSonar therefore distinguishes between:

- **always-emitted metrics** such as `probe_success` and `probe_duration_seconds`
- **conditionally emitted metrics** such as RTT, HTTP status, certificate expiry, DNS match result, and per-phase timings

Conditionally emitted metrics follow **current-observation semantics**: they reflect only what was observed in the latest probe result. If the latest probe did not produce the underlying observation, the corresponding Prometheus series is deleted rather than retaining a stale value or exporting a placeholder zero.

This applies when zero would be misleading as an in-band sentinel. For example:

- ICMP average RTT is meaningful only when at least one echo reply was received
- ICMP RTT standard deviation is meaningful only when at least two echo replies were received
- HTTP status code is meaningful only when an HTTP response was received
- TLS certificate expiry is meaningful only when a certificate was observed
- DNS match result is meaningful only when the comparison was actually evaluated

The RTT rules above apply to both ICMP and MTU probe paths, since MTU probing uses ICMP echo internally.

As a rule, NetSonar emits `0` only when it is a valid value for the metric itself, not as a stand-in for "not observed", "unknown", or "not applicable".

Emission of conditional metrics is based on the semantics of the probe result, not on incidental Go zero-values. Probe implementations and metric recording should use explicit observation state such as "reply observed", "response received", or "match evaluated" when deciding whether a metric should be emitted.

A missing conditional series means **"not observed in the latest probe result"**, not "zero" and not "exporter broken". Dashboards and alerts should interpret such cases together with `probe_success` and probe-specific diagnostic metrics.

`probe_icmp_packet_loss_ratio` is a deliberate exception to the conditional rule: on total ICMP failure, NetSonar emits `1.0` as a clear "nothing got through" signal.

### Alerting Guidance

For conditionally emitted value metrics, alerts should usually key off `probe_success` or probe-specific state metrics rather than `absent()` on the value metric itself.

Use `absent()` or `absent_over_time()` primarily to detect cases where metrics are missing unexpectedly, such as:

- the scrape target disappearing
- the exporter failing to expose metrics
- ingestion or scrape pipeline failures

Do not treat absence of a conditional value metric as a generic probe failure signal when that metric is documented as conditionally emitted by design.

### Emission Summary

| Metric | Semantics | Emitted when | Absence means |
|---|---|---|---|
| `probe_success` | always | every probe result | unexpected for an active target |
| `probe_duration_seconds` | always | every probe result | unexpected for an active target |
| `probe_phase_duration_seconds` | conditional | the phase was observed in the latest probe result | the phase was not reached or not observed in the latest probe result |
| `probe_http_status_code` | conditional | an HTTP response was received in the latest probe result | no HTTP response was received in the latest probe result |
| `probe_http_response_truncated` | conditional | truncation evaluation was performed in the latest probe result | truncation was not evaluable in the latest probe result |
| `probe_http_body_match` | conditional | body evaluation was performed in the latest probe result | body evaluation was not performed in the latest probe result |
| `probe_tls_cert_expiry_timestamp_seconds` | conditional | a certificate was observed in the latest probe result | no certificate was observed in the latest probe result |
| `probe_tls_cert_chain_expiry_timestamp_seconds` | conditional | peer certificates were observed in the latest probe result | no peer certificates were observed in the latest probe result |
| `probe_icmp_packet_loss_ratio` | always (ICMP probes) | every ICMP probe result | unexpected for an active ICMP target |
| `probe_icmp_avg_rtt_seconds` | conditional | at least one ICMP echo reply was observed in the latest probe result | no RTT was observed in the latest probe result |
| `probe_icmp_stddev_rtt_seconds` | conditional | at least two ICMP echo replies were observed in the latest probe result | RTT variation was not observable in the latest probe result |
| `probe_mtu_bytes` | conditional | at least one MTU size was confirmed in the latest probe result | no MTU size was confirmed in the latest probe result |
| `probe_mtu_state` | always | every MTU probe result | unexpected for an active MTU target |
| `probe_dns_resolve_seconds` | always | every DNS probe result | unexpected for an active DNS target |
| `probe_dns_result_match` | conditional | DNS result comparison was evaluated in the latest probe result | DNS result comparison was not evaluated in the latest probe result |
| `probe_skipped_overlap_total` | always | exported for active targets; increments when a stale tick is dropped | unexpected for an active target |

## Dashboard Interpretation

### HTTP Transfer Limit

For `probe_type="http"`, the response body is discarded and read only up to the effective transfer limit: `probe_opts.max_transfer_bytes` when set, otherwise 1 MiB. If the response exceeds that limit, `probe_http_response_truncated` is set to `1`; truncation does not fail the probe by itself. `probe_duration_seconds` and `probe_phase_duration_seconds{phase="transfer"}` measure the capped read, not full response download time.

For `probe_type="http_body"`, oversized bodies remain probe failures (`probe_success=0`) under the HTTP body prober's validation semantics. `probe_http_response_truncated` is not emitted for `http_body`.

### Probes Exceeding Interval (skipped cycles)

This panel tracks how often a probe was still running when the next scheduled tick fired. The scheduler enforces at-most-one-in-flight per target, so it drops the stale tick and increments `probe_skipped_overlap_total`.

An empty panel means all probes complete within their configured interval — this is the expected state. If values appear, consider:

- Increasing the target's `interval` so the probe has more time between cycles.
- Reducing the target's `timeout` to cap how long a slow probe can block.
- Checking network conditions — high latency or packet loss can push probe durations beyond the interval.

### ICMP Panels

The ICMP section shows packet loss ratio, average RTT, and RTT standard deviation. Average RTT is meaningful only when the probe observed at least one echo reply. RTT standard deviation is meaningful only when the probe observed at least two echo replies.

MTU probes also emit `probe_icmp_avg_rtt_seconds`, computed as the average RTT across all successful ICMP echo replies during the probe (sanity echo and step-down payloads). This allows dashboards to show network latency for MTU targets without relying on `probe_duration_seconds`, which reflects the full PMTUD algorithm duration.

If no ICMP echo replies were observed, RTT metrics are absent rather than exported as zero.

Packet loss is calculated as `(sent - received) / sent`, where `sent` counts echo requests after a successful socket write. If the probe context expires before all pings are sent, unsent pings are not counted as lost.

`probe_icmp_stddev_rtt_seconds` near zero is normal — it means all echo replies in the sequence had nearly identical round-trip times, indicating a stable network path. Non-zero values indicate jitter, which can be caused by network congestion, route changes, or asymmetric paths. The metric uses population standard deviation (not sample), consistent with `ping(8)` mdev semantics.

Common causes of 100% packet loss with a working `ping` command:

- **Unprivileged ICMP not enabled** — ICMP and MTU probes require `net.ipv4.ping_group_range` to include the process effective or supplementary GID. Without it, the socket open fails immediately with "permission denied".
- **Firewall or security group rules** — ICMP may be allowed for the user's shell but blocked for the agent's network namespace or source port range.
- **Cross-region/cross-partition paths** — WireGuard, MPLS, or cloud interconnects sometimes drop ICMP while passing TCP/UDP.

## Example `/metrics` Output

This is a shortened capture from the local `lab/dev-stack` NetSonar container, scraped directly from `http://127.0.0.1:9275/metrics` inside the container network. Timings, timestamps, and config hashes will differ between runs. Prometheus adds scrape labels such as `job` and `instance` after collection; they are not emitted by NetSonar itself.

```prometheus
# HELP agent_config_info Hash of the effective configuration currently in use (always 1).
# TYPE agent_config_info gauge
agent_config_info{hash="53590d3a77e8"} 1
# HELP agent_config_reload_timestamp_seconds Unix timestamp of last configuration reload.
# TYPE agent_config_reload_timestamp_seconds gauge
agent_config_reload_timestamp_seconds 1.776588892e+09
# HELP agent_info Agent build information (always 1).
# TYPE agent_info gauge
agent_info{version="dev"} 1
# HELP agent_targets_total Total number of configured targets.
# TYPE agent_targets_total gauge
agent_targets_total 22

# HELP probe_success 1 if the probe succeeded, 0 if it failed.
# TYPE probe_success gauge
probe_success{impact="critical",network_path="direct",probe_type="http",scope="same-region",service="fake-http",target="http://fake-targets:8080/ok",target_account="dev-stack",target_name="http-ok",target_partition="dev",target_region="local"} 1
probe_success{impact="high",network_path="direct",probe_type="http",scope="same-region",service="fake-http",target="http://fake-targets:8080/error",target_account="dev-stack",target_name="http-500-expected-200",target_partition="dev",target_region="local"} 0
probe_success{impact="high",network_path="proxy",probe_type="http",scope="same-region",service="fake-proxy",target="http://fake-targets:8080/ok",target_account="dev-stack",target_name="http-via-proxy",target_partition="dev",target_region="local"} 1
probe_success{impact="high",network_path="proxy",probe_type="tls_cert",scope="same-region",service="fake-proxy",target="fake-targets:9443",target_account="dev-stack",target_name="tls-cert-via-proxy",target_partition="dev",target_region="local"} 1
probe_success{impact="low",network_path="direct",probe_type="dns",scope="same-region",service="fake-dns",target="localhost",target_account="dev-stack",target_name="dns-localhost-mismatch",target_partition="dev",target_region="local"} 0

# HELP probe_duration_seconds Total probe duration in seconds.
# TYPE probe_duration_seconds gauge
probe_duration_seconds{impact="critical",network_path="direct",probe_type="http",scope="same-region",service="fake-http",target="http://fake-targets:8080/ok",target_account="dev-stack",target_name="http-ok",target_partition="dev",target_region="local"} 0.008137887

# HELP probe_phase_duration_seconds Per-phase timing for probes that expose sub-phase breakdowns (HTTP: dns_resolve, tcp_connect, tls_handshake, ttfb, transfer; proxy: proxy_dial, proxy_tls, proxy_connect).
# TYPE probe_phase_duration_seconds gauge
probe_phase_duration_seconds{impact="critical",network_path="direct",phase="dns_resolve",probe_type="http",scope="same-region",service="fake-http",target="http://fake-targets:8080/ok",target_account="dev-stack",target_name="http-ok",target_partition="dev",target_region="local"} 0.000777178
probe_phase_duration_seconds{impact="critical",network_path="direct",phase="tcp_connect",probe_type="http",scope="same-region",service="fake-http",target="http://fake-targets:8080/ok",target_account="dev-stack",target_name="http-ok",target_partition="dev",target_region="local"} 0.000253737
probe_phase_duration_seconds{impact="critical",network_path="direct",phase="ttfb",probe_type="http",scope="same-region",service="fake-http",target="http://fake-targets:8080/ok",target_account="dev-stack",target_name="http-ok",target_partition="dev",target_region="local"} 0.006660328
probe_phase_duration_seconds{impact="critical",network_path="direct",phase="transfer",probe_type="http",scope="same-region",service="fake-http",target="http://fake-targets:8080/ok",target_account="dev-stack",target_name="http-ok",target_partition="dev",target_region="local"} 0.000389704

# HELP probe_http_status_code HTTP response status code.
# TYPE probe_http_status_code gauge
probe_http_status_code{impact="critical",network_path="direct",probe_type="http",scope="same-region",service="fake-http",target="http://fake-targets:8080/ok",target_account="dev-stack",target_name="http-ok",target_partition="dev",target_region="local"} 200
probe_http_status_code{impact="high",network_path="direct",probe_type="http",scope="same-region",service="fake-http",target="http://fake-targets:8080/error",target_account="dev-stack",target_name="http-500-expected-200",target_partition="dev",target_region="local"} 500

# HELP probe_http_response_truncated 1 if the HTTP response body exceeded the effective transfer limit, 0 otherwise.
# TYPE probe_http_response_truncated gauge
probe_http_response_truncated{impact="critical",network_path="direct",probe_type="http",scope="same-region",service="fake-http",target="http://fake-targets:8080/ok",target_account="dev-stack",target_name="http-ok",target_partition="dev",target_region="local"} 0

# HELP probe_tls_cert_chain_expiry_timestamp_seconds Unix timestamp in seconds of each TLS peer certificate expiry.
# TYPE probe_tls_cert_chain_expiry_timestamp_seconds gauge
probe_tls_cert_chain_expiry_timestamp_seconds{cert_index="0",cert_role="leaf",impact="high",network_path="proxy",probe_type="tls_cert",scope="same-region",service="fake-proxy",target="fake-targets:9443",target_account="dev-stack",target_name="tls-cert-via-proxy",target_partition="dev",target_region="local"} 2.091883319e+09
# HELP probe_tls_cert_expiry_timestamp_seconds Unix timestamp in seconds of the earliest TLS peer certificate expiry.
# TYPE probe_tls_cert_expiry_timestamp_seconds gauge
probe_tls_cert_expiry_timestamp_seconds{impact="high",network_path="proxy",probe_type="tls_cert",scope="same-region",service="fake-proxy",target="fake-targets:9443",target_account="dev-stack",target_name="tls-cert-via-proxy",target_partition="dev",target_region="local"} 2.091883319e+09

# HELP probe_icmp_packet_loss_ratio ICMP packet loss ratio (0.0–1.0).
# TYPE probe_icmp_packet_loss_ratio gauge
probe_icmp_packet_loss_ratio{impact="high",network_path="direct",probe_type="icmp",scope="same-region",service="fake-icmp",target="fake-targets",target_account="dev-stack",target_name="icmp-fake-targets",target_partition="dev",target_region="local"} 0
# HELP probe_icmp_avg_rtt_seconds Average ICMP echo round-trip time in seconds.
# TYPE probe_icmp_avg_rtt_seconds gauge
probe_icmp_avg_rtt_seconds{impact="high",network_path="direct",probe_type="icmp",scope="same-region",service="fake-icmp",target="fake-targets",target_account="dev-stack",target_name="icmp-fake-targets",target_partition="dev",target_region="local"} 0.00010804
# HELP probe_icmp_stddev_rtt_seconds Population standard deviation of ICMP echo round-trip time in seconds.
# TYPE probe_icmp_stddev_rtt_seconds gauge
probe_icmp_stddev_rtt_seconds{impact="high",network_path="direct",probe_type="icmp",scope="same-region",service="fake-icmp",target="fake-targets",target_account="dev-stack",target_name="icmp-fake-targets",target_partition="dev",target_region="local"} 2.3828e-05

# HELP probe_mtu_bytes Largest confirmed MTU size from the MTU probe in bytes.
# TYPE probe_mtu_bytes gauge
probe_mtu_bytes{impact="critical",network_path="direct",probe_type="mtu",scope="cross-region",service="fake-mtu",target="fake-targets",target_account="dev-stack-remote",target_name="mtu-fake-targets",target_partition="dev",target_region="eu-west-1"} 1500
# HELP probe_mtu_state MTU probe state as an info metric with state and detail labels (value is always 1).
# TYPE probe_mtu_state gauge
probe_mtu_state{detail="largest_size_confirmed",impact="critical",network_path="direct",probe_type="mtu",scope="cross-region",service="fake-mtu",state="ok",target="fake-targets",target_account="dev-stack-remote",target_name="mtu-fake-targets",target_partition="dev",target_region="eu-west-1"} 1
probe_mtu_state{detail="local_message_too_large",impact="high",network_path="direct",probe_type="mtu",scope="same-region",service="fake-mtu",state="degraded",target="fake-targets",target_account="dev-stack",target_name="mtu-fake-targets-too-large",target_partition="dev",target_region="local"} 1

# HELP probe_dns_resolve_seconds DNS resolution time in seconds.
# TYPE probe_dns_resolve_seconds gauge
probe_dns_resolve_seconds{impact="high",network_path="direct",probe_type="dns",scope="same-region",service="fake-dns",target="localhost",target_account="dev-stack",target_name="dns-localhost-match",target_partition="dev",target_region="local"} 0.000264605
# HELP probe_dns_result_match 1 if DNS result matches expected values, 0 otherwise.
# TYPE probe_dns_result_match gauge
probe_dns_result_match{impact="high",network_path="direct",probe_type="dns",scope="same-region",service="fake-dns",target="localhost",target_account="dev-stack",target_name="dns-localhost-match",target_partition="dev",target_region="local"} 1
probe_dns_result_match{impact="low",network_path="direct",probe_type="dns",scope="same-region",service="fake-dns",target="localhost",target_account="dev-stack",target_name="dns-localhost-mismatch",target_partition="dev",target_region="local"} 0
```
