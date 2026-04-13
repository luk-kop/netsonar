# Configuration Reference

See `config.example.yaml` for a complete working example.

## Example Config and Dashboard Sync

The `config.example.yaml` and `grafana/dashboards/netsonar.json` are designed to work together out of the box. The example config uses a specific set of tag keys, and the dashboard expects those same keys as Prometheus labels to populate its columns:

| Tag Key in Config    | Dashboard Column | Description                                      |
|----------------------|------------------|--------------------------------------------------|
| `service`            | Service          | Logical service name (e.g. `api-pub`, `rds`)     |
| `scope`              | Scope            | Network scope (`same-region`, `cross-region`, `aws-regional`) |
| `impact`             | Impact           | Business impact level (`critical`, `high`, `medium`, `low`) |
| `target_region`      | Region           | Target's cloud region (e.g. `eu-central-1`)      |
| `target_partition`   | Partition        | Network partition (e.g. `global`, `china`)        |
| `target_account`     | Account          | Account or environment identifier                |

If you add, remove, or rename tag keys in your config, update the dashboard transformations accordingly — otherwise columns will appear empty or extra labels will be hidden.

The dashboard also references the `impact` label directly in the "Critical Failures" stat panel (`impact="critical"`). If you use different values for impact levels, update that PromQL query to match.

## Agent Settings

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
    - impact
    - target_region
    - target_partition
    - target_account
```

When `allowed_tag_keys` contains entries, targets may only use tag keys from this list. When absent or empty, the agent collects tag keys dynamically from all targets (limited to 30 unique keys).

## Target Definition

```yaml
targets:
  - name: "api-gw-pub-eu"                                              # Unique identifier (required)
    address: "api-gw-pub.example.internal:443"                               # Target address (required)
    probe_type: tcp                                                     # Probe type (required)
    interval: 30s                                                       # Override agent default_interval
    timeout: 3s                                                         # Override agent default_timeout (must be ≤ interval)
    tags:                                                               # Prometheus labels (dynamic, max 20)
      service: api-gw-pub
      scope: same-region
      impact: critical
      target_region: eu-central-1
      target_partition: global
      target_account: ep-devops-eu1
    probe_opts:                                                         # Probe-type-specific options
      # (see Probe Types section)
```

### Dynamic Tags

Tag keys are not hardcoded in the agent binary. They are collected dynamically from the configuration at startup and used as Prometheus label names. See [Dynamic Labels](metrics.md#dynamic-labels) in the Metrics Reference for details.

When `allowed_tag_keys` is configured, only those keys are permitted — any target using a key outside the list is rejected at config load time. When `allowed_tag_keys` is absent or empty, the agent collects keys dynamically from all targets, subject to a safety limit of 30 unique keys (`MaxGlobalTagKeys`).

All tag keys (whether from the allowlist or collected dynamically) must be valid Prometheus label names (`[a-zA-Z_][a-zA-Z0-9_]*`) and must not collide with fixed labels (`target`, `target_name`, `probe_type`, `proxied`).

## Validation Rules

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
