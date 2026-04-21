# E2E Metrics Harness

This harness starts the real `netsonar` binary in Docker Compose against
controlled local targets, then checks the emitted Prometheus metrics.

## Purpose

This is an integration lab, not a full production network simulation. It checks
the complete NetSonar data path:

```text
config -> scheduler -> real prober -> metrics exporter -> /metrics -> assertions
```

The lab is meant to catch application-level regressions: wrong `probe_success`
values, missing or renamed metrics, incorrect labels, status-code/body-match
logic errors, and broken container ping-socket setup for ICMP/MTU.

It also checks conditional metric semantics for selected edge cases, so metrics
that should be absent when a signal was not observed do not regress back to
zero-value placeholders.

Run:

```sh
docker compose -f lab/e2e/docker-compose.yml up --build --abort-on-container-exit test-runner
docker compose -f lab/e2e/docker-compose.yml down -v
```

Covered in the basic harness:

- `tcp`: open and closed local ports
- `http`: expected status match, expected status mismatch, and accept-any status
- `http`: connection-refused case where HTTP response-derived metrics must stay absent
- `http_body`: body match, body mismatch, and body match with unexpected status
- `http` with `proxy_url`: regular HTTP forwarded through the fake proxy
- `proxy`: CONNECT accepted and denied
- `dns`: resolution, expected-result match, expected-result mismatch, and no-expected absence semantics
- `icmp`: echo success against the fake target container, including a single-reply case where stddev must stay absent
- `mtu`: PMTUD success against the fake target container plus a resolve-failure case where `probe_mtu_bytes` and ICMP RTT stay absent
- `tls_cert`: proxy tunnel success plus connect-failure absence semantics for expiry metrics

The agent image runs as the non-root `netsonar` user. The compose service sets
`net.ipv4.ping_group_range` so ICMP and MTU probes can use Linux unprivileged
ICMP ping sockets inside the Docker lab. MTU assertions use ranges rather than a
fixed value because Docker network MTU can vary by host.

No lab service needs `CAP_NET_RAW` or `cap_add: [NET_RAW]`. See the
[Container Deployment Guide](../../docs/container-deployment.md) for
production-ready least-privilege examples.

The `http-body-bad-status` case is intentionally strict:

- endpoint returns HTTP 500
- response body contains `healthy`
- target config sets `expected_status_codes: [200]`
- expected metrics are `probe_success=0`, `probe_http_body_match=1`,
  `probe_http_status_code=500`

If that case reports `probe_success=1`, the agent is overstating health for
`http_body` probes.

The `http-via-proxy` case is separate from the raw CONNECT proxy cases:

- target config uses `probe_type: http` with `proxy_url`
- fake proxy forwards `GET http://fake-targets:8080/ok`
- expected metrics are `probe_success=1` and `probe_http_status_code=200`

This catches regressions in normal HTTP forward-proxy routing, while
`proxy-connect-ok` and `proxy-connect-denied` continue to cover raw CONNECT
tunnel behaviour.

The `tls-cert-via-proxy` case covers certificate inspection through CONNECT:

- target config uses `probe_type: tls_cert` with `proxy_url`
- fake proxy opens a CONNECT tunnel to `fake-targets:9443`
- expected metrics are `probe_success=1` and a populated
  `probe_tls_cert_expiry_timestamp_seconds`

This catches regressions where `tls_cert` targets are labelled with
`network_path="proxy"` but would accidentally dial the target directly.

The DNS result-match cases are intentionally paired:

- `dns-localhost-match` expects `localhost` to resolve to `127.0.0.1`
- `dns-localhost-mismatch` expects the documentation-only address
  `203.0.113.10`
- expected metrics are `probe_dns_result_match=1` for the match case and `0`
  for the mismatch case

This keeps the `probe_dns_result_match` metric covered by the e2e harness.

The harness also checks absence semantics for selected edge cases:

- `http-no-response` must not emit `probe_http_status_code` or
  `probe_http_response_truncated`
- `tls-cert-connect-fail` must not emit
  `probe_tls_cert_expiry_timestamp_seconds`
- `dns-fake-targets` must not emit `probe_dns_result_match` because no
  `dns_expected` values are configured
- `icmp-single-reply` must emit `probe_icmp_avg_rtt_seconds` but not
  `probe_icmp_stddev_rtt_seconds`
- `mtu-no-resolve` must not emit `probe_mtu_bytes` or
  `probe_icmp_avg_rtt_seconds`

## Relation to Production

The lab uses real Docker networking, real sockets, real HTTP, real ICMP, and the
same agent binary that production uses. That makes it a good check that NetSonar
does not misreport metrics under controlled conditions.

It does not prove that a production network path behaves the same way. Production
can differ because of routing, firewalls, security groups, split-horizon DNS,
enterprise proxies, Kubernetes security policies, AppArmor/seccomp, VPN or
interconnect MTU, ICMP filtering, packet loss, and latency.

Use this lab as a CI/regression gate. For production confidence, also run a small
environment smoke test against real targets:

For an interactive setup with Prometheus and Grafana dashboards, see
[lab/dev-stack/](../dev-stack/).

- a known HTTP health endpoint
- a known DNS record/resolver path
- the production proxy path, if used
- one ICMP target in the expected network
- one MTU target across the critical VPN/interconnect path

If the lab passes but production smoke checks fail, investigate the environment
first: `ping_group_range`, network policies, routing, firewalls, DNS resolver
behavior, and proxy allowlists.
