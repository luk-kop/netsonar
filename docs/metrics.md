# Metrics Reference

Every probe metric carries two kinds of labels: fixed labels set by the agent automatically, and dynamic labels derived from the target's `tags` map in the configuration.

## Fixed Labels

These labels are hardcoded in the agent binary and applied to every metric automatically. They cannot be removed or renamed via configuration.

| Label         | Source                  | Description                                      |
|---------------|-------------------------|--------------------------------------------------|
| `target`      | `address` field         | Target address (e.g. `https://ssm.eu-central-1.amazonaws.com`) |
| `target_name` | `name` field            | Unique target name from config (e.g. `egress-proxy-ok`) |
| `probe_type`  | `probe_type` field      | Probe type (e.g. `tcp`, `http`, `proxy`)         |
| `proxied`     | auto from `proxy_url`   | `"true"` if target uses a proxy, `"false"` otherwise |

## Dynamic Labels

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

## Probe Metrics

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

## Agent Metadata Metrics

| Metric                              | Type  | Labels      | Description                                              |
|-------------------------------------|-------|-------------|----------------------------------------------------------|
| `agent_info`                        | Gauge | `version`   | Agent build info (always 1)                              |
| `agent_config_info`                 | Gauge | `hash`      | Short SHA256 hash of the effective configuration (always 1) |
| `agent_targets_total`               | Gauge | -           | Total number of configured targets                       |
| `agent_config_reload_timestamp`     | Gauge | -           | Unix timestamp of last config reload                     |

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
| `tls_handshake` | HTTP       | TLS handshake (HTTPS only)           |
| `ttfb`          | HTTP       | Time to first byte                   |
| `transfer`      | HTTP       | Response body transfer time          |
| `proxy_dial`    | Proxy      | TCP dial to the proxy                |
| `proxy_tls`     | Proxy      | TLS handshake with the proxy         |
| `proxy_connect` | Proxy      | CONNECT request and response         |

## Dashboard Interpretation

### MTU Diagnostic Events (rate)

The MTU diagnostic counters (`probe_mtu_frag_needed_total`, `probe_mtu_timeouts_total`, `probe_mtu_retries_total`, `probe_mtu_local_errors_total`) are incremented in bursts during each MTU probe cycle. A single cycle tests multiple packet sizes from largest to smallest, generating several events in quick succession, then goes silent until the next interval.

On the dashboard this produces a characteristic spike pattern: the `rate()` jumps (e.g. to ~0.3 ops/s) for roughly one scrape interval, then drops back to zero. The spikes appear simultaneously across all MTU targets because the scheduler starts them together and they share the same interval.

This is normal, healthy behavior. What to watch for:

- **Sustained non-zero rate** — probes are overlapping or continuously retrying, check if the interval is too short relative to the probe duration.
- **Increasing `Local Errors` rate** — kernel-level send failures (EMSGSIZE, permission errors), usually indicates a host configuration issue.
- **`Frag Needed` without a successful MTU result** — the path is signaling fragmentation but no size passes, possibly a black-hole or misconfigured middlebox.

### Probes Exceeding Interval (skipped cycles)

This panel tracks how often a probe was still running when the next scheduled tick fired. The scheduler enforces at-most-one-in-flight per target, so it drops the stale tick and increments `probe_skipped_overlap_total`.

An empty panel means all probes complete within their configured interval — this is the expected state. If values appear, consider:

- Increasing the target's `interval` so the probe has more time between cycles.
- Reducing the target's `timeout` to cap how long a slow probe can block.
- Checking network conditions — high latency or packet loss can push probe durations beyond the interval.

### ICMP Panels

The ICMP section shows packet loss ratio, average RTT, and hop count (TTL). All three metrics are only meaningful when the ICMP probe successfully receives at least one echo reply. When all pings time out, packet loss is 1.0 and RTT/hop count remain at zero.

Common causes of 100% packet loss with a working `ping` command:

- **Unprivileged ICMP not enabled** — the kernel requires `net.ipv4.ping_group_range` to include the process GID. Without it, the socket open fails immediately with "permission denied".
- **Firewall or security group rules** — ICMP may be allowed for the user's shell but blocked for the agent's network namespace or source port range.
- **Cross-region/cross-partition paths** — WireGuard, MPLS, or cloud interconnects sometimes drop ICMP while passing TCP/UDP.
