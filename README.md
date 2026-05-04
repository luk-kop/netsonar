# NetSonar

<!-- markdownlint-disable MD033 -->
<p align="center">
  <img src="docs/img/netsonar_horizontal_lockup_readme.svg" alt="NetSonar" width="520">
</p>
<!-- markdownlint-enable MD033 -->

[![CI](https://github.com/luk-kop/netsonar/actions/workflows/ci.yml/badge.svg)](https://github.com/luk-kop/netsonar/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/luk-kop/netsonar)](https://go.dev/)
[![Go Report Card](https://goreportcard.com/badge/github.com/luk-kop/netsonar)](https://goreportcard.com/report/github.com/luk-kop/netsonar)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

A purpose-built **Go** binary that probes a YAML-configured list of **network
targets** and exposes results as **Prometheus** metrics on a `/metrics`
endpoint — a single, lightweight agent with zero external dependencies.

## Table of Contents

- [Overview](#overview)
- [Build](#build)
- [Usage](#usage)
- [Configuration Reference](#configuration-reference) · [full docs](docs/configuration.md)
- [Probe Types](#probe-types) · [full docs](docs/probe-types.md)
- [Metrics Reference](#metrics-reference) · [full docs](docs/metrics.md)
- [Deployment](#deployment) · [ICMP and MTU Permissions](#icmp-and-mtu-permissions)
- [Scrape Configuration Examples](#scrape-configuration-examples)
- [Operations](#operations) · [full docs](docs/operations.md) · [Doctor mode](docs/doctor.md)

## Overview

The agent runs as a single static binary (~20-30 MB resident memory for up to
100 targets), requires no external dependencies, and uses unprivileged ICMP
ping sockets for ICMP and MTU probes. It owns its target list internally and
exposes pre-labelled metrics, unlike the Blackbox Exporter's multi-target
`/probe?target=X&module=Y` pattern.

Supported probe types: `tcp`, `http`, `icmp`, `mtu`, `dns`, `tls_cert`,
`http_body`, `proxy_connect`.

## Build

Requires Go 1.26+. For linting: [golangci-lint](https://golangci-lint.run/welcome/install/).

```bash
cd netsonar

# Build static binary (version injected from git tags)
make build

# Run all checks: fmt, vet, lint, test, build
make all
```

The binary is written to `bin/netsonar`.

### Make Targets

| Target                          | Description                                                |
|---------------------------------|------------------------------------------------------------|
| `build`                         | Static binary with `CGO_ENABLED=0` and version injection   |
| `build-release`                 | Cross-compile for `linux/amd64` and `linux/arm64`          |
| `test`                          | Run all tests (`go test ./...`)                            |
| `test-short`                    | Run tests in short mode (`go test -short ./...`)           |
| `test-race`                     | Run tests with race detector                               |
| `test-pbt`                      | Run property-based tests only (`-run Property`)            |
| `lint`                          | Run `golangci-lint`                                        |
| `fmt`                           | Format code with `gofmt -s -w`                             |
| `vet`                           | Run `go vet`                                               |
| `clean`                         | Remove `bin/` and `.cache/` directories                    |
| `all`                           | `fmt` + `vet` + `lint` + `test` + `build`                  |
| `lab-e2e`                       | Run end-to-end tests in Docker                             |
| `lab-dev`                       | Start local observability stack (Prometheus + Grafana)     |
| `lab-dev-internet`              | Start dev-stack with public Internet smoke targets         |
| `lab-dev-reload`                | Reload dev-stack agent config with SIGHUP                  |
| `lab-dev-down`                  | Stop dev-stack and remove volumes                          |
| `lab-mv`                        | Start NetSonar + Blackbox side-by-side validation lab      |
| `lab-mv-down`                   | Stop metrics validation lab and remove volumes             |
| `lab-metrics-validation`        | Alias for `lab-mv`                                         |
| `lab-metrics-validation-down`   | Alias for `lab-mv-down`                                    |

## Usage

```bash
# Start with default config path
./bin/netsonar --config /etc/netsonar/config.yaml

# Override listen address
./bin/netsonar --config config.yaml --listen-addr :9275

# Check whether the current host/container can run the configured probes
./bin/netsonar --doctor --config config.yaml
```

### CLI Flags

| Flag            | Default                     | Description                          |
|-----------------|-----------------------------|--------------------------------------|
| `--config`      | `/etc/netsonar/config.yaml` | Path to YAML configuration file      |
| `--listen-addr` | (from config, or `:9275`)   | Override `agent.listen_addr`         |
| `--doctor`      | `false`                     | Run environment diagnostics and exit (see [docs/doctor.md](docs/doctor.md)) |
| `--version`     | `false`                     | Print version and exit               |

## Configuration Reference

Agent settings, target definition, dynamic tags, and validation rules — see [docs/configuration.md](docs/configuration.md).

Quick reference: `config.example.yaml` contains a complete working example.

## Probe Types

Supported: `tcp`, `http`, `icmp`, `mtu`, `dns`, `tls_cert`, `http_body`, `proxy_connect`.

Each probe type with full YAML examples, options, and behaviour details — see [docs/probe-types.md](docs/probe-types.md).

Additional deep-dives:

- [MTU/PMTUD internals](docs/mtu-pmtud.md)
- [MTU measurement options](docs/mtu-measurement-options.md)
- [HTTP request payload probes](docs/http-request-payload-probe.md)
- [Proxy probing guide](docs/proxy-probing.md)

## Metrics Reference

All probe and agent metadata metrics with labels and types, including RTT vs
primary-latency semantics — see [docs/metrics.md](docs/metrics.md).
Metric validation against independent tools is documented in
[docs/metrics-validation.md](docs/metrics-validation.md).

## Deployment

### Prerequisites

- **OS:** Linux (amd64/arm64). The agent uses Linux-specific ICMP socket
  features and is not supported on macOS or Windows.
- **Kernel:** 3.0+ (for unprivileged ICMP sockets via
  `net.ipv4.ping_group_range`)
- **Go 1.26+** (build only — the binary is statically linked and has no runtime
  dependencies)
- Dedicated `netsonar` system user (recommended)

### ICMP and MTU Permissions

ICMP and MTU probes use unprivileged sockets (`SOCK_DGRAM`) and do **not**
require `CAP_NET_RAW`. However, the kernel must allow the process effective or
supplementary GID to open these sockets via `net.ipv4.ping_group_range`. Most
distributions set this to `0 2147483647` by default, but some (notably hardened
or minimal images) restrict it.

> **Note:** `net.ipv4.ping_group_range` is a Linux kernel sysctl that defines
> the range of group IDs (GIDs) allowed to create unprivileged ICMP datagram
> sockets (`socket(AF_INET, SOCK_DGRAM, IPPROTO_ICMP)`). It takes two
> space-separated integers — a lower and upper bound. A process whose
> effective or supplementary GID falls inside this range can send ICMP echo
> requests (ping) without `CAP_NET_RAW` and without being root. The default
> `0 2147483647` allows every GID; setting it to `1 0` (lower > upper)
> disables unprivileged ICMP sockets entirely. NetSonar relies on this
> mechanism for both ICMP and MTU probes, so the agent's runtime GID must be
> covered by the configured range.

If ICMP or MTU probes fail with
`permission denied (check net.ipv4.ping_group_range)`, widen the range:

```bash
# Temporary (until reboot)
sudo sysctl -w net.ipv4.ping_group_range="0 2147483647"

# Persistent
echo 'net.ipv4.ping_group_range = 0 2147483647' | sudo tee /etc/sysctl.d/99-ping-group.conf
sudo sysctl --system
```

The `--doctor` flag checks this automatically and reports the result:

```bash
./bin/netsonar --doctor --config config.yaml
```

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
- `EnvironmentFile=-/etc/default/netsonar` for operator overrides
  (e.g. `NETSONAR_OPTS="--listen-addr :9999"`)
- `SyslogIdentifier=netsonar` for consistent journal filtering
- Security hardening: `NoNewPrivileges`, `ProtectSystem=strict`,
  `ProtectHome=true`, `PrivateTmp=true`, `PrivateDevices=true`,
  `ProtectClock=true`, `MemoryDenyWriteExecute=true`, `ProtectKernelTunables`,
  `ProtectKernelModules`, `ProtectControlGroups`, `RestrictSUIDSGID`,
  `RestrictNamespaces`, `RestrictRealtime`, `LockPersonality`,
  `SystemCallArchitectures=native`, `ReadOnlyPaths=/etc/netsonar`
- Resource limits: `MemoryMax=256M`, `CPUQuota=25%`
- Automatic restart on failure with 5-second delay
- Config reload via `systemctl reload netsonar` (sends SIGHUP)

### Verify

```bash
# Check service status
sudo systemctl status netsonar

# Check environment capabilities for the configured probes
sudo -u netsonar ./bin/netsonar --doctor --config /etc/netsonar/config.yaml

# Liveness / readiness
curl -s http://localhost:9275/healthz
curl -s http://localhost:9275/readyz

# Scrape metrics
curl -s http://localhost:9275/metrics | head -20

# Check agent metadata
curl -s http://localhost:9275/metrics | grep netsonar_
```

`/healthz` returns `200 ok` when the HTTP server is running. `/readyz` returns
`200 ok` when the HTTP server is running (the server starts after the
scheduler, so readiness is guaranteed by the time requests arrive).

`--doctor` loads the config, checks only the environment features required by
the configured probe types, and exits non-zero only on failed required checks.
For example, a `ping_group_range` that excludes the process effective and
supplementary GIDs fails when the config contains ICMP or MTU targets.

For the full check list (Config, Process, ListenAddr, ICMP, MTU, DNS),
severity model (`PASS` / `WARN` / `FAIL` / `SKIP`), exit code semantics, the
`--listen-addr` override, and sample output, see
[docs/doctor.md](docs/doctor.md).

### Container Deployment

NetSonar runs well in containers. No probe type requires `CAP_NET_RAW`; ICMP
and MTU need `net.ipv4.ping_group_range` to include the process effective or
supplementary GID.

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
  --sysctl net.ipv4.ping_group_range="0 2147483647" \
  --user 65532:65532 \
  --read-only \
  -v /path/to/config.yaml:/etc/netsonar/config.yaml:ro \
  -p 9275:9275 \
  netsonar:latest
```

> **Note:** `--cap-drop=ALL` removes every Linux capability from the container
> (including `CAP_NET_RAW`, `CAP_NET_BIND_SERVICE`, `CAP_CHOWN`, etc.), so the
> process runs with the absolute minimum kernel privileges. NetSonar does not
> need any capability — ICMP and MTU probes use unprivileged ping sockets
> gated by `net.ipv4.ping_group_range`, not raw sockets — so dropping all
> capabilities is safe and is the recommended least-privilege baseline.

Kubernetes security context:

```yaml
securityContext:
  runAsNonRoot: true
  readOnlyRootFilesystem: true
  allowPrivilegeEscalation: false
  capabilities:
    drop: [ALL]
```

For ICMP or MTU probes, add `net.ipv4.ping_group_range` as a pod sysctl when the
runtime default does not already include the process effective or supplementary
GID.

For the full container deployment guide covering Kubernetes manifests,
rootless Podman, `ping_group_range` per-pod configuration, and troubleshooting,
see [docs/container-deployment.md](docs/container-deployment.md).

## Scrape Configuration Examples

The agent exposes pre-labelled Prometheus metrics on `/metrics`. All target
labels (`target_name`, `service`, `scope`, etc.) are already embedded — no
relabelling needed.

### Local Dev Stack

For a one-command local setup with Prometheus and Grafana (pre-provisioned
dashboard), see [lab/dev-stack/](lab/dev-stack/):

```bash
make lab-dev
# Grafana: http://localhost:3000  Prometheus: http://localhost:9090
make lab-dev-reload

# Optional: add public Internet smoke targets on top of the local fake targets
# This starts the stack directly; no prior `make lab-dev` is needed.
# Adds HTTP/TCP/DNS/TLS checks, BadSSL failures, and MTU smoke probes.
make lab-dev-internet
make lab-dev-down
```

For side-by-side HTTP phase validation against Prometheus Blackbox Exporter, use
the dedicated [lab/metrics-validation/](lab/metrics-validation/) stack:

```bash
make lab-mv
# Grafana: http://localhost:3000  Prometheus: http://localhost:9090
make lab-mv-down
```

### Prometheus

```yaml
scrape_configs:
  - job_name: netsonar
    static_configs:
      - targets: ["localhost:9275"]
    scrape_interval: 30s
```

### Telegraf

```toml
[[inputs.prometheus]]
  urls = ["http://localhost:9275/metrics"]
  metric_version = 2
  [inputs.prometheus.tags]
    monitor = "network-monitor"
```

### VictoriaMetrics

VictoriaMetrics supports the same `scrape_configs` format as Prometheus — use
the Prometheus example above with `vmagent` or single-node VictoriaMetrics. For
`vmagent`:

```yaml
scrape_configs:
  - job_name: netsonar
    static_configs:
      - targets: ["localhost:9275"]
    scrape_interval: 30s
```

## Operations

For running multiple agents on one host and other operational notes, see
[docs/operations.md](docs/operations.md).

### Configuration Reload

Reload the configuration without restarting the agent:

```bash
# Via systemd
sudo systemctl reload netsonar

# Via signal
sudo kill -HUP $(pidof netsonar)
```

The agent diffs the new configuration against the running state: removed
targets are stopped, new targets are started, changed targets are restarted,
and unchanged targets continue without interruption. When a target is removed
or its configuration changes, the agent also deletes the corresponding time
series from `/metrics`, so dashboards and alerting never see stale values from
targets that no longer exist. If the new configuration is invalid, the agent
continues with the previous configuration and logs the error.

Some fields are startup-only and cannot be changed via SIGHUP. If the new
configuration changes any of these, the reload is rejected and the agent
continues with the previous configuration:

- **Tag key set** (via `allowed_tag_keys` or dynamically collected from
  targets) — Prometheus label names are fixed at startup and cannot be changed
  without recreating the metrics exporter.
- **`listen_addr`** — the HTTP server binds at startup and cannot rebind
  without a restart.
- **`metrics_path`** — the HTTP mux is configured at startup.
- **`log_format`** — the slog handler is installed once at startup.
- **`enable_runtime_metrics`** — Go/process collectors are registered when the
  metrics exporter is created.

`log_level` can be changed with a SIGHUP reload and takes effect immediately.

### Probe Failure Logs

Probe failure reasons are written to the agent log with structured fields.
Metrics intentionally expose only stable values such as `probe_success`,
duration, status code, and probe-specific gauges; the raw error string is not
exported as a Prometheus label because it can have high cardinality.

The scheduler logs state changes per target:

| Event                                           | Level   | Message               |
|-------------------------------------------------|---------|-----------------------|
| First failed probe for a target                 | `warn`  | `probe failed`        |
| Target changes from success to failure          | `warn`  | `probe failed`        |
| Failed target reports a different error string  | `warn`  | `probe failed`        |
| Failed target repeats the same error            | `debug` | `probe still failing` |
| Target recovers from failure                    | `info`  | `probe recovered`     |

Failure and recovery logs include `target_name`, `target`, `probe_type`, and
`duration`. Failure logs also include `error`, for example:

```text
level=WARN msg="probe failed" target_name=egress-proxy target=example.com:443 probe_type=proxy_connect duration=23ms error="proxy CONNECT returned status 407"
```

Use `log_level: debug` only when repeated identical failures are needed for
investigation. At the default `info` level, repeated identical failures are
suppressed after the first warning until the error changes or the target
recovers.

Set `agent.log_format: json` for structured JSON logs; the default is `text`.
`log_level` can be changed with a SIGHUP config reload, while `log_format` is
applied at startup and requires a restart.

Text format:

```text
level=WARN msg="probe failed" target_name=egress-proxy target=example.com:443 probe_type=proxy_connect duration=23ms error="proxy CONNECT returned status 407"
```

JSON format:

```json
{"time":"2026-04-15T20:31:22.123456789Z","level":"WARN","msg":"probe failed","target_name":"egress-proxy","target":"example.com:443","probe_type":"proxy_connect","duration":"23ms","error":"proxy CONNECT returned status 407"}
```

### Graceful Shutdown

```bash
sudo systemctl stop netsonar
```

On `SIGTERM` or `SIGINT`, the agent cancels all probe goroutines and allows
the HTTP server a 5-second grace period for in-flight scrapes before exiting
cleanly.

### Troubleshooting

| Symptom                                   | Cause                                                                | Fix                                                                    |
|-------------------------------------------|----------------------------------------------------------------------|------------------------------------------------------------------------|
| ICMP or MTU probes show `probe_success=0` | `ping_group_range` excludes process effective and supplementary GIDs | Run `./bin/netsonar --doctor --config <path>` to confirm, then `sysctl -w net.ipv4.ping_group_range="0 2147483647"` |
| No metrics on `/metrics`                  | Agent not running or wrong listen address                            | Check `systemctl status` and `--listen-addr` flag                      |
| Probe shows `probe_success=0`             | DNS, TCP, TLS, HTTP, proxy, or permission failure                    | Check `journalctl -u netsonar` for `probe failed`                      |
| Config reload ignored                     | Invalid YAML in new config                                           | Check agent logs (`journalctl -u netsonar`)                            |
| Config reload rejected                    | Startup-only field changed since startup                             | Restart the agent; SIGHUP cannot change tag keys, listen address, metrics path, log format, or runtime metrics setting |
| High memory usage                         | Too many targets or label cardinality                                | Keep targets under 100; keep unique tag keys consistent across targets |
| Config rejected at startup                | A target exceeds 20 tags                                             | Reduce tag count to ≤ 20 per target                                    |
