# NetSonar

[![CI](https://github.com/luk-kop/netsonar/actions/workflows/ci.yml/badge.svg)](https://github.com/luk-kop/netsonar/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/luk-kop/netsonar)](https://go.dev/)
[![Go Report Card](https://goreportcard.com/badge/github.com/luk-kop/netsonar)](https://goreportcard.com/report/github.com/luk-kop/netsonar)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

A purpose-built **Go** binary that probes a YAML-configured list of **network targets** and exposes results as **Prometheus** metrics on a `/metrics` endpoint — a single, lightweight agent with zero external dependencies.

## Table of Contents

- [Overview](#overview)
- [Build](#build)
- [Usage](#usage)
- [Configuration Reference](#configuration-reference) · [full docs](docs/configuration.md)
- [Probe Types](#probe-types) · [full docs](docs/probe-types.md)
- [Metrics Reference](#metrics-reference) · [full docs](docs/metrics.md)
- [Deployment](#deployment) · [ICMP Permissions](#icmp-permissions)
- [Scrape Configuration Examples](#scrape-configuration-examples)
- [Operations](#operations)

## Overview

The agent runs as a single static binary (~20-30 MB resident memory for up to 100 targets), requires no external dependencies, and uses `CAP_NET_RAW` only for MTU probes (ICMP uses unprivileged sockets). It owns its target list internally and exposes pre-labelled metrics, unlike the Blackbox Exporter's multi-target `/probe?target=X&module=Y` pattern.

Supported probe types: TCP, HTTP/HTTPS, ICMP, MTU/PMTUD, DNS, TLS certificate expiry, HTTP body validation, proxy connectivity.

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

| Target      | Description                                              |
|-------------|----------------------------------------------------------|
| `build`     | Static binary with `CGO_ENABLED=0` and version injection |
| `test`      | Run all tests (`go test ./...`)                          |
| `test-short`| Run tests in short mode (`go test -short ./...`)         |
| `test-race` | Run tests with race detector                             |
| `test-pbt`  | Run property-based tests only (`-run Property`)          |
| `lint`      | Run `golangci-lint`                                      |
| `fmt`       | Format code with `gofmt -s -w`                           |
| `vet`       | Run `go vet`                                             |
| `clean`     | Remove `bin/` directory                                  |
| `all`       | `fmt` + `vet` + `lint` + `test` + `build`                |

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

| Flag            | Default                       | Description                        |
|-----------------|-------------------------------|------------------------------------|
| `--config`      | `/etc/netsonar/config.yaml`   | Path to YAML configuration file    |
| `--listen-addr` | (from config, or `:9275`)     | Override `agent.listen_addr`       |
| `--doctor`      | `false`                       | Run environment diagnostics and exit |

## Configuration Reference

Agent settings, target definition, dynamic tags, and validation rules — see [docs/configuration.md](docs/configuration.md).

Quick reference: `config.example.yaml` contains a complete working example.

## Probe Types

Supported: TCP, HTTP/HTTPS, ICMP, MTU/PMTUD, DNS, TLS certificate expiry, HTTP body validation, proxy connectivity.

Each probe type with full YAML examples, options, and behaviour details — see [docs/probe-types.md](docs/probe-types.md).

Additional deep-dives:
- [MTU/PMTUD internals](docs/mtu-pmtud.md)
- [Proxy probing guide](docs/proxy-probing.md)

## Metrics Reference

All probe and agent metadata metrics with labels and types — see [docs/metrics.md](docs/metrics.md).

## Deployment

### Prerequisites

- **OS:** Linux (amd64/arm64). The agent uses Linux-specific ICMP socket features and is not supported on macOS or Windows.
- **Kernel:** 3.0+ (for unprivileged ICMP sockets via `net.ipv4.ping_group_range`)
- **Go 1.26+** (build only — the binary is statically linked and has no runtime dependencies)
- **`CAP_NET_RAW`** capability (only for MTU probes; all other probe types work without it)
- Dedicated `netsonar` system user (recommended)

### ICMP Permissions

ICMP probes use unprivileged sockets (`SOCK_DGRAM`) and do **not** require `CAP_NET_RAW`. However, the kernel must allow the process GID to open these sockets via `net.ipv4.ping_group_range`. Most distributions set this to `0 2147483647` by default, but some (notably hardened or minimal images) restrict it.

If ICMP probes fail with `permission denied (check net.ipv4.ping_group_range)`, widen the range:

```bash
# Temporary (until reboot)
sudo sysctl -w net.ipv4.ping_group_range="0 2147483647"

# Persistent
echo 'net.ipv4.ping_group_range = 0 2147483647' | sudo tee /etc/sysctl.d/99-ping-group.conf
sudo sysctl --system
```

When running via the included `netsonar.service`, the systemd unit already grants `CAP_NET_RAW` (needed for MTU probes), which also covers ICMP raw sockets — so `ping_group_range` is not required in that case. The sysctl fix is only needed when running the binary directly (e.g. `./netsonar -config config.yaml`) as a non-root user without `CAP_NET_RAW`.

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
- `CAP_NET_RAW` ambient capability for MTU probes (ICMP uses unprivileged sockets)
- `EnvironmentFile=-/etc/default/netsonar` for operator overrides (e.g. `NETSONAR_OPTS="--listen-addr :9999"`)
- `SyslogIdentifier=netsonar` for consistent journal filtering
- Security hardening: `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome=true`, `PrivateTmp=true`, `PrivateDevices=true`, `ProtectClock=true`, `MemoryDenyWriteExecute=true`, `ProtectKernelTunables`, `ProtectKernelModules`, `ProtectControlGroups`, `RestrictSUIDSGID`, `RestrictNamespaces`, `RestrictRealtime`, `LockPersonality`, `SystemCallArchitectures=native`, `ReadOnlyPaths=/etc/netsonar`
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
curl -s http://localhost:9275/metrics | grep agent_
```

`/healthz` returns `200 ok` when the HTTP server is running. `/readyz` returns `200 ok` when the HTTP server is running (the server starts after the scheduler, so readiness is guaranteed by the time requests arrive).

`--doctor` loads the config, checks only the environment features required by
the configured probe types, and exits non-zero only on failed required checks.
For example, missing effective `CAP_NET_RAW` fails when the config contains MTU
targets, but is skipped when it does not.

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

Docker with MTU probes (simple runtime model):

```bash
docker run --rm \
  --cap-drop=ALL \
  --cap-add=NET_RAW \
  --user 0:0 \
  --read-only \
  -v /path/to/config.yaml:/etc/netsonar/config.yaml:ro \
  -p 9275:9275 \
  netsonar:latest
```

For hardened non-root containers with MTU probes, grant
`cap_net_raw+ep` to the `netsonar` binary in the image and still keep
`NET_RAW` in the container bounding set with `--cap-add=NET_RAW`.

Kubernetes security context (without MTU probes):

```yaml
securityContext:
  runAsNonRoot: true
  readOnlyRootFilesystem: true
  allowPrivilegeEscalation: false
  capabilities:
    drop: [ALL]
```

For MTU probes, `CAP_NET_RAW` must be effective for the process. In Kubernetes,
either run as root with `NET_RAW`, or use a non-root image with
`cap_net_raw+ep` on the binary, `capabilities.add: [NET_RAW]` for the bounding
set, and no `no_new_privs` blocking file capabilities. ICMP probes use
unprivileged sockets and require `net.ipv4.ping_group_range` to include the
process GID (default on most distributions).

For the full container deployment guide covering Kubernetes manifests, rootless Podman, `ping_group_range` per-pod configuration, and troubleshooting, see [docs/container-deployment.md](docs/container-deployment.md).

## Scrape Configuration Examples

The agent exposes pre-labelled Prometheus metrics on `/metrics`. All target labels (`target_name`, `service`, `scope`, etc.) are already embedded — no relabelling needed.

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

VictoriaMetrics supports the same `scrape_configs` format as Prometheus — use the Prometheus example above with `vmagent` or single-node VictoriaMetrics. For `vmagent`:

```yaml
scrape_configs:
  - job_name: netsonar
    static_configs:
      - targets: ["localhost:9275"]
    scrape_interval: 30s
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
| MTU probes show `probe_success=0`        | Missing effective `CAP_NET_RAW`          | Verify systemd `AmbientCapabilities=CAP_NET_RAW` or container file capability/bounding set |
| ICMP probes show `probe_success=0`       | `ping_group_range` excludes process GID  | `sysctl -w net.ipv4.ping_group_range="0 2147483647"` |
| No metrics on `/metrics`                 | Agent not running or wrong listen address| Check `systemctl status` and `--listen-addr` flag      |
| Probe shows `probe_success=0`             | DNS, TCP, TLS, HTTP, proxy, or permission failure | Check `journalctl -u netsonar` for `probe failed` |
| Config reload ignored                    | Invalid YAML in new config               | Check agent logs (`journalctl -u netsonar`) |
| Config reload rejected                   | Tag key set changed since startup        | Restart the agent; SIGHUP cannot change label schema   |
| High memory usage                        | Too many targets or label cardinality    | Keep targets under 100; keep unique tag keys consistent across targets |
| Config rejected at startup               | A target exceeds 20 tags                 | Reduce tag count to ≤ 20 per target                   |
