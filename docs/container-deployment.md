# Container Deployment Guide

## Table of Contents

- [Overview](#overview)
- [Linux Capabilities](#linux-capabilities)
  - [When is CAP_NET_RAW required?](#when-is-cap_net_raw-required)
  - [Unprivileged ICMP](#unprivileged-icmp)
- [Docker](#docker)
  - [Without MTU probes](#without-mtu-probes)
  - [With MTU probes](#with-mtu-probes)
  - [Non-root with file capabilities](#non-root-with-file-capabilities)
- [Kubernetes](#kubernetes)
  - [Without MTU probes](#without-mtu-probes-1)
  - [With MTU probes](#with-mtu-probes-1)
  - [Setting ping_group_range per pod](#setting-ping_group_range-per-pod)
- [Rootless Podman](#rootless-podman)
- [Troubleshooting](#troubleshooting)
  - [ICMP probes fail with "permission denied"](#icmp-probes-fail-with-permission-denied)
  - [MTU probes fail with "CAP_NET_RAW required"](#mtu-probes-fail-with-cap_net_raw-required)
  - [Checking ping_group_range](#checking-ping_group_range)
  - [Hardened hosts](#hardened-hosts)

## Overview

NetSonar is distributed as a single static binary and runs well in containers. The main consideration is Linux capabilities: which probe types need `CAP_NET_RAW` and which do not.

> **TL;DR — if your configuration does not include `probe_type: mtu`, you do not
> need `CAP_NET_RAW` at all.** Drop all capabilities, run as non-root, and use a
> read-only root filesystem. ICMP probes use unprivileged kernel sockets and work
> without any capabilities (only `net.ipv4.ping_group_range` must include the
> process GID, which is the default on most distributions). The `CAP_NET_RAW`
> sections below apply only to MTU/PMTUD probing.

| Probe type | Requires CAP_NET_RAW |
|---|---|
| TCP | No |
| HTTP / HTTPS | No |
| HTTP body | No |
| DNS | No |
| TLS certificate | No |
| Proxy / CONNECT | No |
| ICMP | No (unprivileged ICMP via kernel SOCK_DGRAM) |
| MTU | **Yes** (raw socket, `IP_PMTUDISC_PROBE`, ICMP DU parsing) |

## Linux Capabilities

### When is CAP_NET_RAW required?

Only `probe_type: mtu` requires `CAP_NET_RAW`. The capability must be
effective for the NetSonar process, not just present in the container spec.
The MTU prober needs:

- A raw ICMP socket (`SOCK_RAW`) to set the `IP_PMTUDISC_PROBE` socket option via `setsockopt`
- Access to raw ICMP Destination Unreachable packets (code 4: fragmentation needed) to detect path MTU limits
- The ability to parse the embedded original packet inside DU messages to verify the response matches the probe

These operations are not available on unprivileged (SOCK_DGRAM) ICMP sockets.

### Unprivileged ICMP

The ICMP prober uses the kernel's unprivileged ICMP socket (`SOCK_DGRAM` + `IPPROTO_ICMP`). On the wire, this produces identical ICMP echo request/reply packets as a raw socket. The kernel manages ICMP IDs, filters responses per socket (no crosstalk), and delivers TTL via control messages.

This requires the `net.ipv4.ping_group_range` sysctl to include the GID of the process running NetSonar. On most modern Linux distributions and container runtimes this is set to `0 2147483647` (all groups allowed) by default.

## Docker

### Without MTU probes

No special flags needed. Drop all capabilities for least-privilege:

```bash
docker run --rm \
  --cap-drop=ALL \
  --read-only \
  -v /path/to/config.yaml:/etc/netsonar/config.yaml:ro \
  -p 9275:9275 \
  netsonar:latest
```

### With MTU probes

Add `CAP_NET_RAW`. The simplest runtime model is to run the container process
as root while dropping every capability except `NET_RAW`:

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

### Non-root with file capabilities

For a hardened non-root image, grant `CAP_NET_RAW` to the NetSonar binary at
image build time:

```dockerfile
RUN apk add --no-cache libcap \
    && setcap cap_net_raw+ep /usr/local/bin/netsonar
USER netsonar
```

Then run the container as the non-root user. The binary file capability gives
the process the raw-socket permission it needs for MTU probes.

If you also drop all runtime capabilities, keep `NET_RAW` in the container
bounding set so the file capability can be applied:

```bash
docker run --rm \
  --cap-drop=ALL \
  --cap-add=NET_RAW \
  --read-only \
  -v /path/to/config.yaml:/etc/netsonar/config.yaml:ro \
  -p 9275:9275 \
  netsonar:latest
```

Important: file capabilities are affected by `no_new_privs`. If your runtime
sets `no-new-privileges`, the process may not acquire `cap_net_raw` from the
binary at exec time. In that case MTU probes fail with `operation not
permitted` even though the container spec mentions `NET_RAW`.

## Kubernetes

### Without MTU probes

Use a restrictive security context. No capabilities needed:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: netsonar
spec:
  template:
    spec:
      containers:
        - name: netsonar
          image: netsonar:latest
          ports:
            - containerPort: 9275
              name: metrics
          securityContext:
            runAsNonRoot: true
            readOnlyRootFilesystem: true
            allowPrivilegeEscalation: false
            capabilities:
              drop: [ALL]
```

### With MTU probes

Add `NET_RAW` capability. For the most direct setup, run as root while dropping
all other capabilities:

```yaml
          securityContext:
            runAsUser: 0
            readOnlyRootFilesystem: true
            allowPrivilegeEscalation: false
            capabilities:
              drop: [ALL]
              add: [NET_RAW]
```

For a non-root Kubernetes deployment, prefer an image where the binary has
`cap_net_raw+ep` set as described in
[Non-root with file capabilities](#non-root-with-file-capabilities). Verify the
runtime actually preserves that file capability for the process. Do not assume
that `runAsNonRoot: true` plus `capabilities.add: [NET_RAW]` is sufficient on
every runtime.

If you rely on file capabilities, `allowPrivilegeEscalation: false` can block
the capability gain because Kubernetes sets `no_new_privs`. Test MTU probing in
the target cluster before enabling that hardening setting for MTU-enabled
deployments.

Concrete non-root example for an image with `cap_net_raw+ep` on the binary:

```yaml
          securityContext:
            runAsNonRoot: true
            runAsUser: 10001
            readOnlyRootFilesystem: true
            allowPrivilegeEscalation: true
            capabilities:
              drop: [ALL]
              add: [NET_RAW]
```

Here `add: [NET_RAW]` keeps `NET_RAW` in the bounding set, and
`allowPrivilegeEscalation: true` avoids `no_new_privs` blocking the binary's
file capability at exec time.

### Setting ping_group_range per pod

The `net.ipv4.ping_group_range` sysctl is network-namespaced since Linux 4.18 (2018). On kernels 4.18+ you can set it per pod instead of relying on the host default:

```yaml
      securityContext:
        sysctls:
          - name: net.ipv4.ping_group_range
            value: "0 2147483647"
```

This requires the kubelet to allow the sysctl:

```
--allowed-unsafe-sysctls=net.ipv4.ping_group_range
```

On kernels before 4.18 this sysctl is global (host-level only) and cannot be set per pod.

In practice, most hosts already have this set to `0 2147483647`, so you typically do not need to configure it explicitly. Check with `sysctl net.ipv4.ping_group_range` on the node.

## Rootless Podman

Unprivileged ICMP works in rootless Podman as long as the host has `ping_group_range` set correctly (which it does on most distributions):

```bash
podman run --rm \
  --read-only \
  -v /path/to/config.yaml:/etc/netsonar/config.yaml:ro \
  -p 9275:9275 \
  netsonar:latest
```

MTU probing does not work in rootless Podman — there is no way to grant `CAP_NET_RAW` without root. Use rootful Podman or Docker with `--cap-add=NET_RAW` for MTU probes.

## Troubleshooting

Before starting a container or pod with a production config, you can run
environment diagnostics:

```bash
netsonar --doctor --config /etc/netsonar/config.yaml
```

The doctor command is config-aware: it only fails checks required by the probe
types present in the config. For example, missing effective `CAP_NET_RAW` fails
when MTU targets are configured, but is skipped for configs without MTU targets.

### ICMP probes fail with "permission denied"

The ICMP prober uses unprivileged ICMP sockets. If you see a permission error, the kernel's `net.ipv4.ping_group_range` does not include the GID of the NetSonar process.

**Fix (host-level):**

```bash
# Check current value
sysctl net.ipv4.ping_group_range

# If it shows "1  0" (disabled), enable for all groups:
sudo sysctl -w net.ipv4.ping_group_range="0 2147483647"

# Make persistent:
echo 'net.ipv4.ping_group_range = 0 2147483647' | sudo tee /etc/sysctl.d/99-ping-group.conf
sudo sysctl --system
```

**Fix (Kubernetes, kernel 4.18+):**

Add the sysctl to the pod spec as shown in [Setting ping_group_range per pod](#setting-ping_group_range-per-pod).

### MTU probes fail with "CAP_NET_RAW required"

The MTU prober requires a raw ICMP socket. Ensure `CAP_NET_RAW` is effective for
the NetSonar process:

- Docker lab/simple deployment: `--cap-add=NET_RAW --user 0:0`
- Docker hardened non-root deployment: set `cap_net_raw+ep` on the binary
- Kubernetes simple deployment: run as root, add `NET_RAW`, and verify the
  process has it
- Kubernetes hardened non-root deployment: use a file capability and avoid
  blocking it with `no_new_privs`
- Rootless Podman: not supported — use rootful Podman or Docker

To inspect process capabilities from inside the container:

```bash
grep Cap /proc/self/status
```

For `CAP_NET_RAW`, bit 13 must be present in the effective capability set.

### Checking ping_group_range

From the host or inside the container:

```bash
cat /proc/sys/net/ipv4/ping_group_range
```

Expected output for unprivileged ICMP to work:

```
0	2147483647
```

If it shows `1	0`, unprivileged ICMP is disabled.

### Hardened hosts

Some hardened configurations (e.g. CIS benchmarks) disable `ping_group_range` by setting it to `1 0`. On these hosts:

- ICMP probes without `CAP_NET_RAW`: will fail unless you restore `ping_group_range` or add `CAP_NET_RAW` to the container
- MTU probes: require `CAP_NET_RAW` regardless of `ping_group_range`

The least-invasive fix is to set `ping_group_range` to include the GID of the container process rather than granting `CAP_NET_RAW` for ICMP. This keeps the capability surface minimal — `NET_RAW` is only needed if the configuration includes `probe_type: mtu`.
