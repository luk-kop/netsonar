# Dev Stack — NetSonar + Prometheus + Grafana

Local observability stack for development, demos, and dashboard iteration.
One `docker compose up` gives you the full NetSonar pipeline with pre-configured
Grafana dashboards.

## What's Inside

| Service        | Port  | Description                              |
|----------------|-------|------------------------------------------|
| netsonar       | 9275  | Agent probing fake targets               |
| fake-targets   | —     | Controlled TCP/HTTP/proxy/DNS endpoints  |
| prometheus     | 9090  | Scrapes netsonar every 5 s               |
| grafana        | 3000  | Pre-provisioned with NetSonar dashboard  |

## Quick Start

```sh
make lab-dev
```

Open Grafana at [http://localhost:3000](http://localhost:3000) (login:
`admin` / `admin`, or browse anonymously as Viewer).

The NetSonar dashboard is auto-provisioned under the **NetSonar** folder.
Select the **Prometheus** datasource in the dashboard variable dropdown.

Prometheus UI is available at [http://localhost:9090](http://localhost:9090).

## Internet Smoke Targets

The default stack is fully local and deterministic. To check outbound Internet
egress from the agent container, start the stack with the optional Internet
config:

```sh
make lab-dev-internet
```

You do not need to run `make lab-dev` first. This command starts the same
Prometheus/Grafana/NetSonar stack, but starts NetSonar with
`config/netsonar.with-internet.yaml`, generated from the default local config
plus `config/netsonar-internet.yaml`. If the local stack is already running,
`make lab-dev-internet` recreates the NetSonar container with the merged config.

Use `make lab-dev` to switch back to the local fake-target config. Use
`make lab-dev-reload` only after editing the currently running config file; a
reload does not switch between `netsonar.yaml` and
`netsonar.with-internet.yaml` because the config path is a startup argument.

Both `make lab-dev` and `make lab-dev-internet` force container recreation so
the selected config path is applied. After switching configs, wait for the first
probe cycle and Prometheus scrape before reading dashboard panels.

The Internet overlay keeps the default fake-local targets and adds a public
smoke set:

- HTTPS GET to `https://example.com/`
- TCP connects to public HTTPS endpoints, labelled as `cross-region` and
  `aws-regional` so those existing TCP dashboard panels have data
- TLS certificate expiry check for `example.com:443`
- DNS A lookup for `cloudflare.com` through `1.1.1.1:53`
- DNS expected-result match for `one.one.one.one` through `1.1.1.1:53`
- MTU/PMTUD checks to public targets: `1.1.1.1`, `8.8.8.8`,
  `9.9.9.9`, and `ping.ripe.net`
- TLS validation failures from BadSSL:
  `expired.badssl.com`, `wrong.host.badssl.com`, `self-signed.badssl.com`, and
  `untrusted-root.badssl.com`
- TLS certificate inspection for `expired.badssl.com:443` with
  `tls_skip_verify: true`, useful for showing certificate expiry even when
  trust validation would fail

Treat these as best-effort smoke checks only. Public endpoints, DNS recursion,
corporate proxies, firewalls, and captive portals can change results without any
NetSonar code change. Keep `make lab-dev` as the deterministic local workflow
and use `make lab-dev-internet` when you specifically want to validate egress.

In the Internet config, `scope` values are dashboard demo labels. They do not
mean the public target is truly in a same-region, cross-region, or AWS regional
network path from your machine.

The merged Internet config includes TCP targets labelled `same-region`,
`cross-region`, and `aws-regional` so the three TCP duration panels have data.
The `same-region` TCP series comes from the base fake-local config; the
cross-region and AWS regional series come from public Internet targets.

The BadSSL HTTP targets are expected to report `probe_success=0` with TLS
errors. They are included to make broken-certificate states visible in Grafana,
not to represent healthy Internet dependencies.

The public MTU targets require outbound ICMP echo and ICMP fragmentation-needed
feedback to work. They are intentionally low-frequency best-effort smoke
checks, not a public MTU benchmark. If they fail while TCP/HTTP/DNS probes
succeed, first suspect local firewall, Docker host networking, VPN policy, or
upstream ICMP filtering.

## Probe Coverage

The stack uses controlled fake targets, so all "regions" are simulated with
labels rather than real remote networks. This is intentional: the goal is to
exercise dashboard filters, PromQL grouping, and reload behaviour locally.

Included scenarios:

- TCP open/closed targets, including a healthy `scope=cross-region` target.
- HTTP success, expected status failure, and accept-any status handling.
- HTTP body match, mismatch, and body match with unexpected status.
- HTTP through a forward proxy (`http-via-proxy`) and raw CONNECT proxy probes.
- TLS certificate inspection through a forward proxy (`tls-cert-via-proxy`).
- DNS resolution plus DNS expected-result match and mismatch cases.
- ICMP and MTU probes against the fake target container, including MTU failure
  cases for oversized packets and an unreachable IPv4 target.
- Cross-region labels with `target_region=eu-west-1` and
  `target_account=dev-stack-remote` for dashboard validation.

The MTU target uses a denser payload list than the built-in default:

```yaml
[1472, 1452, 1432, 1412, 1392, 1372, 1272, 1172, 1072]
```

That maps to path MTU checkpoints from 1500 down to 1100 bytes, with extra
coverage around common tunnel sizes.

The `mtu-fake-targets-too-large` and `mtu-unreachable` probes are expected to
fail. They keep the MTU dashboard and status table populated with degraded or
unreachable states during local development.

## Stopping

```sh
make lab-dev-down
```

The `-v` flag removes named volumes (prometheus-data, grafana-data). Omit it
to keep data across restarts.

## Differences from E2E Harness

The `lab/e2e/` harness is a lightweight CI gate — it runs assertions against
`/metrics` and exits. This dev-stack is meant for interactive use:

- Persistent Prometheus storage for time-series exploration.
- Grafana with the production dashboard for visual validation.
- No test-runner container — the stack stays up until you stop it.
- Slightly longer default interval (5 s vs 2 s) to keep the UI readable.

## Customization

Edit `config/netsonar.yaml` to add or change probe targets. The agent picks
up the config at startup. To reload without restarting the stack, send SIGHUP
to the agent:

```sh
make lab-dev-reload
```

Reload supports target changes and tag values as long as the configured tag key
set stays the same. Changes to `agent.allowed_tag_keys` or `agent.log_format`
require restarting the `netsonar` container.

The Grafana dashboard JSON is mounted from `grafana/dashboards/` in the repo
root, so any dashboard changes you make in the repo are reflected on next
Grafana restart.
