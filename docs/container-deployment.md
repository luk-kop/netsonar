# Container Deployment Guide

## Table of Contents

- [Overview](#overview)
- [Linux Capabilities](#linux-capabilities)
  - [Unprivileged ICMP and MTU](#unprivileged-icmp-and-mtu)
- [Docker](#docker)
  - [Docker without MTU probes](#docker-without-mtu-probes)
  - [Docker with MTU probes](#docker-with-mtu-probes)
  - [Non-root containers](#non-root-containers)
- [Kubernetes](#kubernetes)
  - [Kubernetes without MTU probes](#kubernetes-without-mtu-probes)
  - [Kubernetes with MTU probes](#kubernetes-with-mtu-probes)
  - [Setting ping_group_range per pod](#setting-ping_group_range-per-pod)
- [Rootless Podman](#rootless-podman)
- [Troubleshooting](#troubleshooting)
  - [ICMP or MTU probes fail with "permission denied"](#icmp-or-mtu-probes-fail-with-permission-denied)
  - [Checking ping_group_range](#checking-ping_group_range)
  - [Hardened hosts](#hardened-hosts)

## Overview

NetSonar is distributed as a single static binary and runs well in containers. Probe traffic does not require `CAP_NET_RAW`; ICMP and MTU use Linux unprivileged ping sockets.

> **TL;DR — you do not need `CAP_NET_RAW` for NetSonar probes.** Drop all
> capabilities, run as non-root, and use a read-only root filesystem. ICMP and
> MTU probes require only that `net.ipv4.ping_group_range` include the process
> effective or supplementary GID.

| Probe type | Requires Linux capability |
|---|---|
| TCP | No |
| HTTP / HTTPS | No |
| HTTP body | No |
| DNS | No |
| TLS certificate | No |
| Proxy / CONNECT | No |
| ICMP | No (unprivileged ICMP via kernel SOCK_DGRAM) |
| MTU | No (unprivileged ICMP ping socket + error queue) |

## Linux Capabilities

### Unprivileged ICMP and MTU

The ICMP and MTU probers use the kernel's unprivileged ICMP socket (`SOCK_DGRAM` + `IPPROTO_ICMP`). On the wire, this produces ICMP echo request/reply packets. The kernel manages ICMP IDs and filters responses per socket. MTU probing additionally enables `IP_RECVERR` and `IP_MTU_DISCOVER=IP_PMTUDISC_PROBE` to read PMTUD errors from the socket error queue.

This requires the `net.ipv4.ping_group_range` sysctl to include the effective or supplementary GID of the process running NetSonar. On most modern Linux distributions and container runtimes this is set to `0 2147483647` by default.

> **Note:** `net.ipv4.ping_group_range` is a Linux kernel sysctl that defines
> the range of group IDs (GIDs) allowed to create unprivileged ICMP datagram
> sockets (`socket(AF_INET, SOCK_DGRAM, IPPROTO_ICMP)`). It takes two
> space-separated integers — a lower and upper bound. A process whose
> effective or supplementary GID falls inside this range can send ICMP echo
> requests (ping) without `CAP_NET_RAW` and without being root. The default
> `0 2147483647` allows every GID; setting it to `1 0` (lower > upper)
> disables unprivileged ICMP sockets entirely. NetSonar relies on this
> mechanism for both ICMP and MTU probes, so the agent's runtime GID must be
> covered by the configured range. Since Linux 4.18 the sysctl is
> network-namespaced, so it can be set per pod / per container instead of
> globally on the host.

## Docker

### Docker without MTU probes

No special flags needed. Drop all capabilities for least-privilege:

```bash
docker run --rm \
  --cap-drop=ALL \
  --read-only \
  -v /path/to/config.yaml:/etc/netsonar/config.yaml:ro \
  -p 9275:9275 \
  netsonar:latest
```

### Docker with MTU probes

No extra Linux capability is required. Ensure `ping_group_range` includes the
container process effective or supplementary GID:

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

### Non-root containers

No file capability is needed for MTU probing. Run as a non-root user, drop all
capabilities, and make sure `net.ipv4.ping_group_range` includes the process
effective or supplementary GID.

## Kubernetes

### Kubernetes without MTU probes

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

### Kubernetes with MTU probes

Keep the same restrictive container securityContext, run as a fixed non-root
UID/GID, and add `ping_group_range` as a pod sysctl if your runtime default
does not already include the process effective or supplementary GID. The two
`securityContext` blocks live at different scopes — container-level under
`spec.containers[].securityContext`, and pod-level under `spec.securityContext`
(siblings of `containers:`):

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
          securityContext:                 # container-level
            runAsNonRoot: true
            runAsUser: 10001
            readOnlyRootFilesystem: true
            allowPrivilegeEscalation: false
            capabilities:
              drop: [ALL]
      securityContext:                     # pod-level (sibling of containers)
        sysctls:
          - name: net.ipv4.ping_group_range
            value: "10001 10001"           # match runAsUser/runAsGroup
```

### Setting ping_group_range per pod

The `net.ipv4.ping_group_range` sysctl is network-namespaced since Linux 4.18 (2018). On kernels 4.18+ you can set it per pod instead of relying on the host default:

```yaml
# spec:
#   template:
#     spec:
      securityContext:           # pod-level (sibling of containers)
        sysctls:
          - name: net.ipv4.ping_group_range
            value: "0 2147483647"
```

Kubernetes treats `net.ipv4.ping_group_range` as a safe sysctl since 1.18, so
typical clusters do not need kubelet unsafe-sysctl opt-in for this setting.

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

MTU probing works in rootless Podman when the network namespace's
`ping_group_range` includes the process effective or supplementary GID.

## Troubleshooting

Before starting a container or pod with a production config, you can run
environment diagnostics:

```bash
netsonar --doctor --config /etc/netsonar/config.yaml
```

The doctor command is config-aware: it only fails checks required by the probe
types present in the config. For example, a `ping_group_range` that excludes the
process effective and supplementary GIDs fails when ICMP or MTU targets are
configured.

### ICMP or MTU probes fail with "permission denied"

The ICMP and MTU probers use unprivileged ICMP sockets. If you see a permission error, the kernel's `net.ipv4.ping_group_range` does not include the effective or supplementary GID of the NetSonar process.

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

### Checking ping_group_range

From the host or inside the container:

```bash
cat /proc/sys/net/ipv4/ping_group_range
```

Expected output for unprivileged ICMP to work:

```text
0 2147483647
```

If it shows `1 0`, unprivileged ICMP is disabled.

### Hardened hosts

Some hardened configurations (e.g. CIS benchmarks) disable `ping_group_range` by setting it to `1 0`. On these hosts:

- ICMP probes fail unless you restore `ping_group_range`
- MTU probes fail unless you restore `ping_group_range`

The least-invasive fix is to set `ping_group_range` to include the effective or
supplementary GID of the container process.
