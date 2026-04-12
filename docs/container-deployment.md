# Container Deployment Guide

## Table of Contents

- [Overview](#overview)
- [Linux Capabilities](#linux-capabilities)
  - [When is CAP_NET_RAW required?](#when-is-cap_net_raw-required)
  - [Unprivileged ICMP](#unprivileged-icmp)
- [Docker](#docker)
  - [Without MTU probes](#without-mtu-probes)
  - [With MTU probes](#with-mtu-probes)
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

Only `probe_type: mtu` requires `CAP_NET_RAW`. The MTU prober needs:

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
  -p 9116:9116 \
  netsonar:latest
```

### With MTU probes

Add `CAP_NET_RAW`:

```bash
docker run --rm \
  --cap-drop=ALL \
  --cap-add=NET_RAW \
  --read-only \
  -v /path/to/config.yaml:/etc/netsonar/config.yaml:ro \
  -p 9116:9116 \
  netsonar:latest
```

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
            - containerPort: 9116
              name: metrics
          securityContext:
            runAsNonRoot: true
            readOnlyRootFilesystem: true
            allowPrivilegeEscalation: false
            capabilities:
              drop: [ALL]
```

### With MTU probes

Add `NET_RAW` capability:

```yaml
          securityContext:
            runAsNonRoot: true
            readOnlyRootFilesystem: true
            allowPrivilegeEscalation: false
            capabilities:
              drop: [ALL]
              add: [NET_RAW]
```

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
  -p 9116:9116 \
  netsonar:latest
```

MTU probing does not work in rootless Podman — there is no way to grant `CAP_NET_RAW` without root. Use rootful Podman or Docker with `--cap-add=NET_RAW` for MTU probes.

## Troubleshooting

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

The MTU prober requires a raw ICMP socket. Grant `CAP_NET_RAW` to the container:

- Docker: `--cap-add=NET_RAW`
- Kubernetes: `capabilities: { add: [NET_RAW] }` in the container security context
- Rootless Podman: not supported — use rootful Podman or Docker

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
