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
| `probe_type`   | `probe_type` field    | Probe type (e.g. `tcp`, `http`, `proxy_connect`) |
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
    tags: { service: api-gw, scope: local, impact: critical }
  - name: bastion-public
    tags: { service: bastion, scope: external }
```

The agent registers three dynamic labels: `service`, `scope`, `impact`. The `bastion-public` target gets `impact=""` because it does not define that key.

## Metric Naming Convention

Probe metric names follow the pattern `probe_<domain>_<measurement>_<unit>`, where `<domain>` identifies **what is being measured**, not which probe type emits the metric.

- **`probe_http_*`** — metrics about the HTTP protocol layer (status code, body match, response truncation).
- **`probe_tls_*`** — metrics about TLS certificates. Emitted by `tls_cert` probes and by `http` probes when `probe_opts.tls_emit_cert_metrics: true`, because both observe TLS certificates during their operation.
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
| `probe_timeout_seconds` | — | all |
| `probe_interval_seconds` | — | all |
| `probe_timed_out` | — | all |
| `probe_phase_duration_seconds` | — | tcp, http, http_body, tls_cert, icmp, mtu, proxy_connect |
| `probe_http_status_code` | `http_` | http, http_body |
| `probe_http_response_truncated` | `http_` | http |
| `probe_http_body_match` | `http_` | http_body |
| `probe_proxy_connect_status_code` | `proxy_` | proxy_connect, tls_cert (with `proxy_url`), http (with `proxy_url`, `https://` target), http_body (with `proxy_url`, `https://` target) |
| `probe_tls_cert_expiry_timestamp_seconds` | `tls_` | http (when `tls_emit_cert_metrics=true`), tls_cert |
| `probe_tls_cert_chain_expiry_timestamp_seconds` | `tls_` | http (when `tls_emit_cert_metrics=true`), tls_cert |
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
| `probe_timeout_seconds`             | Gauge | common          | Effective configured target timeout            |
| `probe_interval_seconds`            | Gauge | common          | Effective configured target interval           |
| `probe_timed_out`                   | Gauge | common          | 1 if the probe reached or exceeded its configured timeout, 0 otherwise |
| `probe_phase_duration_seconds`      | Gauge | common + `phase`| Per-phase timing for probes with sub-phases    |
| `probe_http_status_code`            | Gauge | common          | HTTP response status code                      |
| `probe_proxy_connect_status_code`   | Gauge | common          | HTTP status code returned by the proxy to a CONNECT request |
| `probe_tls_cert_expiry_timestamp_seconds` | Gauge | common    | Unix timestamp of earliest TLS certificate expiry in the peer chain |
| `probe_tls_cert_chain_expiry_timestamp_seconds` | Gauge | common + `cert_index`, `cert_role` | Unix timestamp of each peer certificate expiry |
| `probe_http_response_truncated`     | Gauge | common          | 1 if HTTP response body exceeded response body limit, 0 otherwise |
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

| Metric                                      | Type  | Labels                                | Description                                              |
|---------------------------------------------|-------|---------------------------------------|----------------------------------------------------------|
| `netsonar_build_info`                       | Gauge | `version`, `revision`, `build_date`   | NetSonar build info (always 1)                           |
| `netsonar_config_info`                      | Gauge | `hash`                                | Short SHA256 hash of the effective configuration (always 1) |
| `netsonar_targets_total`                    | Gauge | -                                     | Total number of configured targets                       |
| `netsonar_config_reload_timestamp_seconds`  | Gauge | -                                     | Unix timestamp of last config reload                     |

### `netsonar_config_reload_timestamp_seconds`

The timestamp is set both at **startup** (initial config load) and after
every successful **SIGHUP reload**. Because the initial load also sets the
gauge, `time() - netsonar_config_reload_timestamp_seconds` effectively tracks
agent uptime when no reloads have occurred.

### `netsonar_config_info`

The hash is computed over the effective configuration **after** defaults
have been applied and validation has passed, not over the raw YAML bytes.
`Targets` are sorted by `name` before hashing, so reordering targets in the
YAML file does not change the hash. Whitespace, comments, and key order in
the YAML file are irrelevant.

## Optional Runtime Metrics

By default, NetSonar does not expose Go runtime or process metrics from the
Prometheus client library. Set `agent.enable_runtime_metrics: true` to add
standard metric families such as `go_goroutines`, `go_memstats_*`, and
`process_cpu_seconds_total` to the existing `/metrics` endpoint.

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
| `dns_resolve`   | TCP, HTTP (direct), http_body (direct), TLS cert (direct), ICMP, MTU | DNS resolution time for hostname targets |
| `tcp_connect`   | TCP, HTTP (direct), http_body (direct), TLS cert (direct) | TCP connection establishment |
| `tls_handshake` | HTTPS (http, http_body) direct and proxy path, TLS cert (direct and via proxy) | TLS handshake with the target |
| `request_write` | HTTP, http_body (direct and proxy path) | Request write time after connection/TLS readiness |
| `ttfb`          | HTTP, http_body (direct and proxy path) | Time to first byte after request write — see note below |
| `transfer`      | HTTP, http_body (direct and proxy path) | Body read time up to the effective response body limit |

### Phase Matrix by Probe Type

This table is the canonical reference for which `phase` label values can be
emitted by each probe type and path. Conditional phases are emitted only when
that part of the probe was reached and applies to the target.

| Probe type / path | Condition | Possible emitted phases |
|---|---|---|
| `tcp` | hostname target | `dns_resolve`, `tcp_connect` |
| `tcp` | literal IP target | `tcp_connect` |
| `http` direct | `http://` target | `dns_resolve` for hostname targets, `tcp_connect`, `request_write`, `ttfb`, `transfer` |
| `http` direct | `https://` target | `dns_resolve` for hostname targets, `tcp_connect`, `tls_handshake`, `request_write`, `ttfb`, `transfer` |
| `http` proxy | `http://` target, `http://` proxy | `proxy_dial`, `request_write`, `ttfb`, `transfer` |
| `http` proxy | `http://` target, `https://` proxy | `proxy_dial`, `proxy_tls`, `request_write`, `ttfb`, `transfer` |
| `http` proxy | `https://` target, `http://` proxy | `proxy_dial`, `proxy_connect`, `tls_handshake`, `request_write`, `ttfb`, `transfer` |
| `http` proxy | `https://` target, `https://` proxy | `proxy_dial`, `proxy_tls`, `proxy_connect`, `tls_handshake`, `request_write`, `ttfb`, `transfer` |
| `http_body` direct | `http://` target | `dns_resolve` for hostname targets, `tcp_connect`, `request_write`, `ttfb`, `transfer` |
| `http_body` direct | `https://` target | `dns_resolve` for hostname targets, `tcp_connect`, `tls_handshake`, `request_write`, `ttfb`, `transfer` |
| `http_body` proxy | `http://` target, `http://` proxy | `proxy_dial`, `request_write`, `ttfb`, `transfer` |
| `http_body` proxy | `http://` target, `https://` proxy | `proxy_dial`, `proxy_tls`, `request_write`, `ttfb`, `transfer` |
| `http_body` proxy | `https://` target, `http://` proxy | `proxy_dial`, `proxy_connect`, `tls_handshake`, `request_write`, `ttfb`, `transfer` |
| `http_body` proxy | `https://` target, `https://` proxy | `proxy_dial`, `proxy_tls`, `proxy_connect`, `tls_handshake`, `request_write`, `ttfb`, `transfer` |
| `tls_cert` direct | hostname target | `dns_resolve`, `tcp_connect`, `tls_handshake` |
| `tls_cert` direct | literal IP target | `tcp_connect`, `tls_handshake` |
| `tls_cert` proxy | `http://` proxy | `proxy_dial`, `proxy_connect`, `tls_handshake` |
| `tls_cert` proxy | `https://` proxy | `proxy_dial`, `proxy_tls`, `proxy_connect`, `tls_handshake` |
| `proxy_connect` | `http://` proxy | `proxy_dial`, `proxy_connect` |
| `proxy_connect` | `https://` proxy | `proxy_dial`, `proxy_tls`, `proxy_connect` |
| `dns` | any target | none; DNS duration is emitted as `probe_dns_resolve_seconds`, not `probe_phase_duration_seconds` |
| `icmp` | hostname target | `dns_resolve` |
| `icmp` | literal IP target | none |
| `mtu` | hostname target | `dns_resolve` |
| `mtu` | literal IP target | none |

`request_write`, `ttfb`, and `transfer` appear only for probe types that perform
an HTTP request/response exchange (`http` and `http_body`). TCP,
`proxy_connect`, and `tls_cert` probes stop at lower protocol layers, so those
HTTP exchange phases are not emitted for them.

### TTFB semantics

`request_write` is measured from the moment the connection is ready to send the HTTP request (after TCP for plain HTTP, after TLS handshake for HTTPS, or after the last proxy-setup phase on the proxy path) until the request has been written. For large generated request bodies, upload time appears in this phase.

`ttfb` is measured from request write completion until the first byte of the response is received. It captures server processing time plus response-header wire time, and does **not** overlap with `tls_handshake` or `request_write`.

Phases are non-overlapping: `dns_resolve + tcp_connect + tls_handshake + request_write + ttfb + transfer ≈ probe_duration_seconds` for direct probes; on the proxy path the equivalent sum substitutes `proxy_dial` (optionally `proxy_tls`, `proxy_connect`) for `tcp_connect` and `dns_resolve`.

The `transfer` phase semantics differ slightly between `http` and `http_body`:
`http` reads the response body up to the configurable `response_body_limit_bytes` (default 1 MiB) and reports truncation via `probe_http_response_truncated` without failing the probe. `http_body` reads up to a fixed 1 MiB limit and fails the probe when the response body exceeds that limit. In both cases the `transfer` phase measures the time spent reading the body up to the effective limit.

| Phase           | Probe Type | Description                          |
|-----------------|------------|--------------------------------------|
| `proxy_dial`    | Proxy, HTTP/http_body via proxy, TLS cert via proxy | TCP dial to the proxy (includes proxy hostname DNS) |
| `proxy_tls`     | Proxy, HTTP/http_body via proxy, TLS cert via proxy | TLS handshake with the proxy (only for `https://` proxies) |
| `proxy_connect` | Proxy, HTTP/http_body via proxy (HTTPS targets only), TLS cert via proxy | CONNECT request write and response read |

`tcp_connect` and `proxy_dial` are **mutually exclusive** for a single probe execution: direct probes emit `tcp_connect`, proxy-path probes emit `proxy_dial` instead. `dns_resolve` is not emitted for proxy-path `http`/`http_body` probes — proxy hostname resolution is included in `proxy_dial` and the target hostname is resolved by the proxy itself.

Direct `tls_cert` probes emit `dns_resolve` for hostname targets, plus `tcp_connect` and `tls_handshake`. Direct TCP probes emit `dns_resolve` for hostname targets plus `tcp_connect`. `tls_cert` probes through a proxy emit the proxy tunnel phases plus `tls_handshake` for the target TLS handshake.

## Conditional Metric Semantics

Some probe metrics are meaningful for every probe result, while others are meaningful only when a specific observation was made during the latest probe run.

NetSonar therefore distinguishes between:

- **always-emitted metrics** such as `probe_success` and `probe_duration_seconds`
- **configuration metrics** such as `probe_timeout_seconds` and `probe_interval_seconds`, emitted for every active target before the first probe result
- **conditionally emitted metrics** such as RTT, HTTP status, certificate expiry, DNS match result, and per-phase timings

`probe_timed_out` is an always-emitted result metric. It is set to 1 when any
of three independent sources indicate the per-probe deadline was reached:

1. The prober self-reported a timeout in `result.TimedOut` (e.g. an ICMP probe
   concluding all echoes timed out within their per-attempt budget).
2. `probeCtx.Err() == context.DeadlineExceeded` after `Probe()` returned
   (strict context-state check).
3. The wall-clock probe duration measured by the scheduler reached or exceeded
   the configured timeout. This is a fallback for the race where Go's
   `net.Dialer` internal deadline timer fires before `context.WithTimeout`'s
   own cancel goroutine has updated `ctx.Err()` — observed in practice for TCP
   probes against silently-dropping targets, where the dial returns an
   `i/o timeout` error before the context state catches up.

Use it to distinguish timeout failures from fast failures such as DNS, TCP RST,
TLS handshake, proxy CONNECT, HTTP validation, or permission errors.

Conditionally emitted metrics follow **current-observation semantics**: they reflect only what was observed in the latest probe result. If the latest probe did not produce the underlying observation, the corresponding Prometheus series is deleted rather than retaining a stale value or exporting a placeholder zero.

This applies when zero would be misleading as an in-band sentinel. For example:

- ICMP average RTT is meaningful only when at least one echo reply was received
- ICMP RTT standard deviation is meaningful only when at least two echo replies were received
- HTTP status code is meaningful only when an HTTP response was received
- TLS certificate expiry is meaningful only when a certificate was observed and, for `http` probes, `tls_emit_cert_metrics` is enabled
- DNS match result is meaningful only when the comparison was actually evaluated

The RTT rules above apply to both ICMP and MTU probe paths, since MTU probing uses ICMP echo internally.

As a rule, NetSonar emits `0` only when it is a valid value for the metric itself, not as a stand-in for "not observed", "unknown", or "not applicable".

Emission of conditional metrics is based on the semantics of the probe result, not on incidental Go zero-values. Probe implementations and metric recording should use explicit observation state such as "reply observed", "response received", or "match evaluated" when deciding whether a metric should be emitted.

A missing conditional series means **"not observed in the latest probe result"**, not "zero" and not "exporter broken". Dashboards and alerts should interpret such cases together with `probe_success` and probe-specific diagnostic metrics.

`probe_icmp_packet_loss_ratio` is a deliberate exception to the conditional rule: on total ICMP failure, NetSonar emits `1.0` as a clear "nothing got through" signal.

For phase metrics specifically, a phase is emitted when it was started and measured, regardless of sub-operation success. A phase is absent when it was not reached.

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
| `probe_timeout_seconds` | config | active target is scheduled | unexpected for an active target |
| `probe_interval_seconds` | config | active target is scheduled | unexpected for an active target |
| `probe_timed_out` | always | every probe result | unexpected for an active target after first result |
| `probe_phase_duration_seconds` | conditional | the phase was observed in the latest probe result | the phase was not reached or not observed in the latest probe result |
| `probe_http_status_code` | conditional | an HTTP response was received in the latest probe result | no HTTP response was received in the latest probe result |
| `probe_proxy_connect_status_code` | conditional | a proxy CONNECT response was received in the latest probe result | no proxy CONNECT response was received in the latest probe result (no proxy hop, or proxy connection failed before a response) |
| `probe_http_response_truncated` | conditional | truncation evaluation was performed in the latest probe result | truncation was not evaluable in the latest probe result |
| `probe_http_body_match` | conditional | body evaluation was performed in the latest probe result | body evaluation was not performed in the latest probe result |
| `probe_tls_cert_expiry_timestamp_seconds` | conditional | a certificate was observed in the latest probe result and, for `http` probes, `tls_emit_cert_metrics=true` | no certificate was observed in the latest probe result, or HTTP TLS cert metrics are disabled |
| `probe_tls_cert_chain_expiry_timestamp_seconds` | conditional | peer certificates were observed in the latest probe result and, for `http` probes, `tls_emit_cert_metrics=true` | no peer certificates were observed in the latest probe result, or HTTP TLS cert metrics are disabled |
| `probe_icmp_packet_loss_ratio` | always (ICMP probes) | every ICMP probe result | unexpected for an active ICMP target |
| `probe_icmp_avg_rtt_seconds` | conditional | at least one ICMP echo reply was observed in the latest probe result | no RTT was observed in the latest probe result |
| `probe_icmp_stddev_rtt_seconds` | conditional | at least two ICMP echo replies were observed in the latest probe result | RTT variation was not observable in the latest probe result |
| `probe_mtu_bytes` | conditional | at least one MTU size was confirmed in the latest probe result | no MTU size was confirmed in the latest probe result |
| `probe_mtu_state` | always | every MTU probe result | unexpected for an active MTU target |
| `probe_dns_resolve_seconds` | always | every DNS probe result | unexpected for an active DNS target |
| `probe_dns_result_match` | conditional | DNS result comparison was evaluated in the latest probe result | DNS result comparison was not evaluated in the latest probe result |
| `probe_skipped_overlap_total` | always | exported for active targets; increments when a stale tick is dropped | unexpected for an active target |

## RTT and Latency Interpretation

### Native RTT

**RTT** (round-trip time) is the time required for a probe packet or request to travel to the remote endpoint and for the corresponding reply to return. RTT is not specific to ICMP as a networking concept, but in NetSonar only ICMP-derived metrics expose strict RTT directly.

Native RTT metrics in NetSonar are:

| Probe Type | Metric | Meaning |
|---|---|---|
| `icmp` | `probe_icmp_avg_rtt_seconds` | Average ICMP echo round-trip time across successful replies in the latest probe |
| `icmp` | `probe_icmp_stddev_rtt_seconds` | Variation of ICMP echo RTT across successful replies in the latest probe |
| `mtu` | `probe_icmp_avg_rtt_seconds` | Average ICMP echo round-trip time observed during PMTUD ICMP echo steps |

`probe_duration_seconds` is **not** RTT. It measures the total wall-clock time of the whole probe execution.

### Probe-Specific Latency Signals

Several probe types expose timings that are useful to operators, but they are not a single cross-probe status-table column. The main Grafana status table shows status, timeout, total probe time, timeout limit, limit-used ratio, and target identity labels. Detailed latency attribution lives in the probe-specific phase, duration, and state panels.

| Probe Type | Metric / Phase | Operator interpretation | Strict RTT? | Where to inspect |
|---|---|---|---|---|
| `icmp` | `probe_icmp_avg_rtt_seconds` | Network round-trip latency | Yes | ICMP RTT panels |
| `mtu` | `probe_icmp_avg_rtt_seconds` | Network round-trip latency observed during MTU probing | Yes | MTU state and RTT-derived panels |
| `tcp` | `probe_phase_duration_seconds{phase="tcp_connect"}` | TCP connect latency | No | TCP Phase Breakdown / TCP Phase Timing |
| `dns` | `probe_dns_resolve_seconds` | DNS lookup latency | No | DNS Resolve Time |
| `http` | `probe_phase_duration_seconds{phase="ttfb"}` | Request-to-first-byte latency | No | HTTP Phase Breakdown / HTTP Phase Timing |
| `http_body` | `probe_phase_duration_seconds` and `probe_duration_seconds` | HTTP exchange, capped body read, and body validation duration | No | HTTP Response Body phase and duration panels |
| `tls_cert` | `probe_phase_duration_seconds{phase="tls_handshake"}` | TLS handshake latency | No | TLS Cert Phase Breakdown / TLS Phase Timing |
| `proxy_connect` | `probe_phase_duration_seconds{phase="proxy_connect"}` | Proxy CONNECT request/response latency | No | Proxy CONNECT Phase Breakdown / Phase Timing |

Per-metric interpretation notes:

- `tcp_connect` measures TCP connection establishment time. It is often a good network-path indicator, but it is not pure RTT.
- `probe_dns_resolve_seconds` measures DNS resolution time. It includes resolver behavior and lookup overhead, so it should not be treated as pure network RTT.
- `tls_handshake` measures the TLS handshake phase. It is useful for diagnosing handshake slowness, but it is not RTT.
- `ttfb` measures time from request-send readiness to first response byte. It includes server processing time and response-header travel time, so it must not be interpreted as RTT.
- `http_body` probes emit the same per-phase timings as `http`; use those phase panels plus `probe_http_body_match` to diagnose validation failures.
- `proxy_connect` measures CONNECT request/response latency through the proxy tunnel. It is useful for proxy-path diagnosis, not RTT estimation.
- `probe_proxy_connect_status_code` records the HTTP status returned by the proxy to a CONNECT request. It is distinct from `probe_http_status_code`, which records the target HTTP response for `http` and `http_body`.

`probe_icmp_stddev_rtt_seconds` tracks RTT variation, not latency itself.

### Operator Guidance and Terminology

When reading dashboards and alerts:

- use `probe_icmp_avg_rtt_seconds` when you want true network RTT
- use phase metrics when you want to identify which protocol stage is slow
- use `probe_duration_seconds` when you want the total cost of the probe as an operation
- do not assume that two timing metrics with the same unit have the same meaning

The main Grafana duration panels use a 5-minute rolling median per target. This
keeps normal millisecond-range latency readable without letting a short outlier
dominate the panel for several minutes. Threshold lines are hidden on median
duration panels to keep the chart visually clean; use phase breakdown and phase
timing panels for latency diagnosis. The "Slowest HTTP Probes" panel
intentionally remains raw `topk` data because its purpose is to surface outliers
and timeouts.

A useful mental model is:

- **RTT** answers: "how slow is the round trip on the path?"
- **Phase timing** answers: "which part of the protocol interaction is slow?"
- **Total probe duration** answers: "how long did the whole probe take?"

For consistent naming in dashboards and runbooks:

- reserve **RTT** for ICMP-derived round-trip metrics
- use **phase timing** for protocol-stage timings such as `tcp_connect`, `dns_resolve`, `tls_handshake`, `ttfb`, or `proxy_connect`
- use **Total Probe Time** or **Probe Duration** for `probe_duration_seconds`

## Dashboard Interpretation

### HTTP Response Body Limit

For `probe_type="http"`, the response body is discarded and read only up to the effective response body limit: `probe_opts.response_body_limit_bytes` when set, otherwise 1 MiB. If the response exceeds that limit, `probe_http_response_truncated` is set to `1`; truncation does not fail the probe by itself. `probe_duration_seconds` and `probe_phase_duration_seconds{phase="transfer"}` measure the capped read, not full response download time.

For `probe_type="http_body"`, oversized bodies remain probe failures (`probe_success=0`) under the HTTP body prober's validation semantics. `probe_http_response_truncated` is not emitted for `http_body`.

### Total Probe Time in the Status Table

The `Total Probe Time` column in the "All Probes — Status Table" shows `probe_duration_seconds`, which is the **wall-clock** duration of the full probe execution. Its typical range depends strongly on the probe type:

| Probe type | Typical TPT (healthy target) | Drivers |
|---|---|---|
| `tcp` | tens of ms | Single connect attempt |
| `http`, `http_body` | tens–hundreds of ms | DNS + connect + TLS + request write + TTFB + transfer (capped) |
| `tls_cert` | tens–hundreds of ms | DNS + connect + TLS handshake |
| `dns` | tens of ms | Single resolver lookup |
| `proxy_connect` | tens–hundreds of ms | Proxy dial + TLS (optional) + CONNECT |
| `icmp` | **seconds** | `ping_count × ping_interval` + per-echo RTTs (default `ping_interval = 1s`) |
| `mtu` | **seconds** | Sanity echo + step-down across `icmp_payload_sizes`, each size with up to `mtu_retries` attempts and `mtu_per_attempt_timeout` each (defaults: 3 retries, 2s per attempt) |

Because TPT for `icmp` and `mtu` is dominated by configured intervals and retry budgets — not by network speed — it will be in the seconds range even for perfectly healthy targets. This is why the column is **not color-coded**: a multi-second TPT on ICMP/MTU is normal, and flagging it as "bad" would be misleading.

**Failure behavior.** On probe failure, TPT still reflects wall-clock time until the probe returned. Two distinct shapes:

- **Full timeout** (target unreachable, no replies): TPT approaches the probe's configured `timeout` (the scheduler wraps each probe in `context.WithTimeout(ctx, target.Timeout)`). For ICMP, the context deadline is shared across all pings in the sequence, so TPT on a fully unreachable target converges to `target.Timeout`, not `ping_count × something`. For MTU, retries stop early once the shared deadline expires, so TPT is also bounded by `target.Timeout` — potentially less than the theoretical `len(icmp_payload_sizes) × mtu_retries × mtu_per_attempt_timeout` budget.
- **Fast fail** (DNS resolve error, socket open error, config validation): TPT is near zero because the probe returns immediately — despite `Status = FAIL`.

**Operator guidance.**

- Treat `Status` as the primary health signal for a target. TPT is diagnostic context, not a health indicator.
- When alerting on slow probes, alert on `probe_duration_seconds` **per `probe_type`** with thresholds that match each probe's expected shape. Do not apply a single global threshold across probe types.
- If `Status = FAIL`, look at `Timed Out`, `Limit Used`, and TPT to tell a full timeout (TPT near the timeout boundary, `Limit Used` near 100%) from a fast fail (TPT ≈ 0) without opening the details dashboard.

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
# HELP netsonar_config_info Hash of the effective configuration currently in use (always 1).
# TYPE netsonar_config_info gauge
netsonar_config_info{hash="53590d3a77e8"} 1
# HELP netsonar_config_reload_timestamp_seconds Unix timestamp of last configuration reload.
# TYPE netsonar_config_reload_timestamp_seconds gauge
netsonar_config_reload_timestamp_seconds 1.776588892e+09
# HELP netsonar_build_info NetSonar build information (always 1).
# TYPE netsonar_build_info gauge
netsonar_build_info{build_date="unknown",revision="unknown",version="dev"} 1
# HELP netsonar_targets_total Total number of configured targets.
# TYPE netsonar_targets_total gauge
netsonar_targets_total 22

# HELP probe_success 1 if the probe succeeded, 0 if it failed.
# TYPE probe_success gauge
probe_success{impact="critical",network_path="direct",probe_type="http",scope="local",service="fake-http",target="http://fake-targets:8080/ok",target_account="dev-stack",target_name="http-ok",target_partition="dev",target_region="local"} 1
probe_success{impact="high",network_path="direct",probe_type="http",scope="local",service="fake-http",target="http://fake-targets:8080/error",target_account="dev-stack",target_name="http-500-expected-200",target_partition="dev",target_region="local"} 0
probe_success{impact="high",network_path="proxy",probe_type="http",scope="local",service="fake-proxy",target="http://fake-targets:8080/ok",target_account="dev-stack",target_name="http-via-proxy",target_partition="dev",target_region="local"} 1
probe_success{impact="high",network_path="proxy",probe_type="tls_cert",scope="local",service="fake-proxy",target="fake-targets:9443",target_account="dev-stack",target_name="tls-cert-via-proxy",target_partition="dev",target_region="local"} 1
probe_success{impact="low",network_path="proxy",probe_type="proxy_connect",scope="local",service="fake-proxy",target="fake-targets:9001",target_account="dev-stack",target_name="proxy-connect-denied",target_partition="dev",target_region="local"} 1
probe_success{impact="low",network_path="direct",probe_type="dns",scope="local",service="fake-dns",target="localhost",target_account="dev-stack",target_name="dns-localhost-mismatch",target_partition="dev",target_region="local"} 0

# HELP probe_duration_seconds Total probe duration in seconds.
# TYPE probe_duration_seconds gauge
probe_duration_seconds{impact="critical",network_path="direct",probe_type="http",scope="local",service="fake-http",target="http://fake-targets:8080/ok",target_account="dev-stack",target_name="http-ok",target_partition="dev",target_region="local"} 0.008137887

# HELP probe_phase_duration_seconds Per-phase timing for probes that expose sub-phase breakdowns (TCP: dns_resolve for hostname targets, tcp_connect; HTTP and http_body direct: dns_resolve for hostname targets, tcp_connect, tls_handshake for https, request_write, ttfb, transfer; HTTP and http_body via proxy: proxy_dial, optional proxy_tls for https proxies, proxy_connect for https targets, tls_handshake for https targets, request_write, ttfb, transfer — tcp_connect and dns_resolve are not emitted on the proxy path; TLS cert direct: dns_resolve for hostname targets, tcp_connect, tls_handshake; ICMP and MTU: dns_resolve for hostname targets; proxy_connect and TLS cert via proxy: proxy_dial, proxy_tls, proxy_connect; TLS cert via proxy also adds tls_handshake).
# TYPE probe_phase_duration_seconds gauge
probe_phase_duration_seconds{impact="critical",network_path="direct",phase="dns_resolve",probe_type="http",scope="local",service="fake-http",target="http://fake-targets:8080/ok",target_account="dev-stack",target_name="http-ok",target_partition="dev",target_region="local"} 0.000777178
probe_phase_duration_seconds{impact="critical",network_path="direct",phase="tcp_connect",probe_type="http",scope="local",service="fake-http",target="http://fake-targets:8080/ok",target_account="dev-stack",target_name="http-ok",target_partition="dev",target_region="local"} 0.000253737
probe_phase_duration_seconds{impact="critical",network_path="direct",phase="ttfb",probe_type="http",scope="local",service="fake-http",target="http://fake-targets:8080/ok",target_account="dev-stack",target_name="http-ok",target_partition="dev",target_region="local"} 0.006660328
probe_phase_duration_seconds{impact="critical",network_path="direct",phase="transfer",probe_type="http",scope="local",service="fake-http",target="http://fake-targets:8080/ok",target_account="dev-stack",target_name="http-ok",target_partition="dev",target_region="local"} 0.000389704
probe_phase_duration_seconds{impact="low",network_path="proxy",phase="proxy_dial",probe_type="proxy_connect",scope="local",service="fake-proxy",target="fake-targets:9001",target_account="dev-stack",target_name="proxy-connect-denied",target_partition="dev",target_region="local"} 0.000412303
probe_phase_duration_seconds{impact="low",network_path="proxy",phase="proxy_connect",probe_type="proxy_connect",scope="local",service="fake-proxy",target="fake-targets:9001",target_account="dev-stack",target_name="proxy-connect-denied",target_partition="dev",target_region="local"} 0.001874519

# HELP probe_http_status_code HTTP response status code.
# TYPE probe_http_status_code gauge
probe_http_status_code{impact="critical",network_path="direct",probe_type="http",scope="local",service="fake-http",target="http://fake-targets:8080/ok",target_account="dev-stack",target_name="http-ok",target_partition="dev",target_region="local"} 200
probe_http_status_code{impact="high",network_path="direct",probe_type="http",scope="local",service="fake-http",target="http://fake-targets:8080/error",target_account="dev-stack",target_name="http-500-expected-200",target_partition="dev",target_region="local"} 500

# HELP probe_proxy_connect_status_code HTTP status code returned by the proxy to a CONNECT request. Emitted by probe_type=proxy_connect, and by probe_type=http, http_body, or tls_cert when proxy_url targets an HTTPS destination that requires CONNECT.
# TYPE probe_proxy_connect_status_code gauge
probe_proxy_connect_status_code{impact="low",network_path="proxy",probe_type="proxy_connect",scope="local",service="fake-proxy",target="fake-targets:9001",target_account="dev-stack",target_name="proxy-connect-denied",target_partition="dev",target_region="local"} 403

# HELP probe_http_response_truncated 1 if the HTTP response body exceeded the effective response body limit, 0 otherwise.
# TYPE probe_http_response_truncated gauge
probe_http_response_truncated{impact="critical",network_path="direct",probe_type="http",scope="local",service="fake-http",target="http://fake-targets:8080/ok",target_account="dev-stack",target_name="http-ok",target_partition="dev",target_region="local"} 0

# HELP probe_tls_cert_chain_expiry_timestamp_seconds Unix timestamp in seconds of each TLS peer certificate expiry.
# TYPE probe_tls_cert_chain_expiry_timestamp_seconds gauge
probe_tls_cert_chain_expiry_timestamp_seconds{cert_index="0",cert_role="leaf",impact="high",network_path="proxy",probe_type="tls_cert",scope="local",service="fake-proxy",target="fake-targets:9443",target_account="dev-stack",target_name="tls-cert-via-proxy",target_partition="dev",target_region="local"} 2.091883319e+09
# HELP probe_tls_cert_expiry_timestamp_seconds Unix timestamp in seconds of the earliest TLS peer certificate expiry.
# TYPE probe_tls_cert_expiry_timestamp_seconds gauge
probe_tls_cert_expiry_timestamp_seconds{impact="high",network_path="proxy",probe_type="tls_cert",scope="local",service="fake-proxy",target="fake-targets:9443",target_account="dev-stack",target_name="tls-cert-via-proxy",target_partition="dev",target_region="local"} 2.091883319e+09

# HELP probe_icmp_packet_loss_ratio ICMP packet loss ratio (0.0–1.0).
# TYPE probe_icmp_packet_loss_ratio gauge
probe_icmp_packet_loss_ratio{impact="high",network_path="direct",probe_type="icmp",scope="local",service="fake-icmp",target="fake-targets",target_account="dev-stack",target_name="icmp-fake-targets",target_partition="dev",target_region="local"} 0
# HELP probe_icmp_avg_rtt_seconds Average ICMP echo round-trip time in seconds.
# TYPE probe_icmp_avg_rtt_seconds gauge
probe_icmp_avg_rtt_seconds{impact="high",network_path="direct",probe_type="icmp",scope="local",service="fake-icmp",target="fake-targets",target_account="dev-stack",target_name="icmp-fake-targets",target_partition="dev",target_region="local"} 0.00010804
# HELP probe_icmp_stddev_rtt_seconds Population standard deviation of ICMP echo round-trip time in seconds.
# TYPE probe_icmp_stddev_rtt_seconds gauge
probe_icmp_stddev_rtt_seconds{impact="high",network_path="direct",probe_type="icmp",scope="local",service="fake-icmp",target="fake-targets",target_account="dev-stack",target_name="icmp-fake-targets",target_partition="dev",target_region="local"} 2.3828e-05

# HELP probe_mtu_bytes Largest confirmed MTU size from the MTU probe in bytes.
# TYPE probe_mtu_bytes gauge
probe_mtu_bytes{impact="critical",network_path="direct",probe_type="mtu",scope="external",service="fake-mtu",target="fake-targets",target_account="dev-stack-remote",target_name="mtu-fake-targets",target_partition="dev",target_region="eu-west-1"} 1500
# HELP probe_mtu_state MTU probe state as an info metric with state and detail labels (value is always 1).
# TYPE probe_mtu_state gauge
probe_mtu_state{detail="largest_size_confirmed",impact="critical",network_path="direct",probe_type="mtu",scope="external",service="fake-mtu",state="ok",target="fake-targets",target_account="dev-stack-remote",target_name="mtu-fake-targets",target_partition="dev",target_region="eu-west-1"} 1
probe_mtu_state{detail="local_message_too_large",impact="high",network_path="direct",probe_type="mtu",scope="local",service="fake-mtu",state="degraded",target="fake-targets",target_account="dev-stack",target_name="mtu-fake-targets-too-large",target_partition="dev",target_region="local"} 1

# HELP probe_dns_resolve_seconds DNS resolution time in seconds.
# TYPE probe_dns_resolve_seconds gauge
probe_dns_resolve_seconds{impact="high",network_path="direct",probe_type="dns",scope="local",service="fake-dns",target="localhost",target_account="dev-stack",target_name="dns-localhost-match",target_partition="dev",target_region="local"} 0.000264605
# HELP probe_dns_result_match 1 if DNS result matches expected values, 0 otherwise.
# TYPE probe_dns_result_match gauge
probe_dns_result_match{impact="high",network_path="direct",probe_type="dns",scope="local",service="fake-dns",target="localhost",target_account="dev-stack",target_name="dns-localhost-match",target_partition="dev",target_region="local"} 1
probe_dns_result_match{impact="low",network_path="direct",probe_type="dns",scope="local",service="fake-dns",target="localhost",target_account="dev-stack",target_name="dns-localhost-mismatch",target_partition="dev",target_region="local"} 0
```
