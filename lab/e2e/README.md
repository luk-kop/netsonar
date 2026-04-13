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
logic errors, and broken container capability setup for ICMP/MTU.

Run:

```sh
docker compose -f lab/e2e/docker-compose.yml up --build --abort-on-container-exit test-runner
docker compose -f lab/e2e/docker-compose.yml down -v
```

Covered in the basic harness:

- `tcp`: open and closed local ports
- `http`: expected status match, expected status mismatch, and accept-any status
- `http_body`: body match, body mismatch, and body match with unexpected status
- `proxy`: CONNECT accepted and denied
- `icmp`: echo success against the fake target container
- `mtu`: PMTUD success against the fake target container

The agent image grants `cap_net_raw+ep` to the `netsonar` binary and runs as the
non-root `netsonar` user. The compose service keeps `NET_RAW` in the bounding
set so ICMP/MTU probes can exercise the real kernel/network path inside the
Docker lab. MTU assertions use ranges rather than a fixed value because Docker
network MTU can vary by host.

> **Note — CAP_NET_RAW is only needed for MTU probes.** The `cap_net_raw+ep`
> file capability in the Dockerfile and `cap_add: [NET_RAW]` in the compose
> service exist solely because this harness includes `probe_type: mtu` targets.
> All other probe types — including ICMP, which uses unprivileged kernel sockets
> — work without any capabilities. If you adapt this lab for a setup that does
> not include MTU probes, you can safely remove `cap_add: [NET_RAW]` from
> `docker-compose.yml` and the `libcap`/`setcap` lines from
> `Dockerfile.netsonar`. See the
> [Container Deployment Guide](../../docs/container-deployment.md) for
> production-ready least-privilege examples.

The `http-body-bad-status` case is intentionally strict:

- endpoint returns HTTP 500
- response body contains `healthy`
- target config sets `expected_status_codes: [200]`
- expected metrics are `probe_success=0`, `probe_http_body_match=1`,
  `probe_http_status_code=500`

If that case reports `probe_success=1`, the agent is overstating health for
`http_body` probes.

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

- a known HTTP health endpoint
- a known DNS record/resolver path
- the production proxy path, if used
- one ICMP target in the expected network
- one MTU target across the critical VPN/interconnect path

If the lab passes but production smoke checks fail, investigate the environment
first: effective `CAP_NET_RAW`, `ping_group_range`, network policies, routing,
firewalls, DNS resolver behavior, and proxy allowlists.
