# Doctor Mode

`--doctor` runs config-aware environment diagnostics and exits. It loads the
configuration, then checks only the host capabilities required by the probe
types declared in that config. No probes are executed and no metrics are
exposed; the agent prints a human-readable report and returns an exit code.

## Invocation

```bash
./bin/netsonar --doctor --config /etc/netsonar/config.yaml
```

Optional flags:

| Flag            | Effect                                                           |
|-----------------|------------------------------------------------------------------|
| `--config`      | Path to the YAML configuration file (same as normal startup).    |
| `--listen-addr` | Override `agent.listen_addr` for the bind check only.            |

`--listen-addr` is useful when the running agent already binds the configured
port and you want the doctor to test a different address without editing the
config:

```bash
./bin/netsonar --doctor --config /etc/netsonar/config.yaml --listen-addr :9999
```

## Exit Code

| Exit | Meaning                                                                     |
|------|-----------------------------------------------------------------------------|
| `0`  | No check returned `FAIL`. `WARN` and `SKIP` results do not affect the exit. |
| `1`  | At least one check returned `FAIL`.                                         |

## Severity Levels

| Severity | Meaning                                                                            |
|----------|------------------------------------------------------------------------------------|
| `PASS`   | The check ran and the environment satisfies the requirement.                       |
| `WARN`   | The check could not determine the result, or detected something advisory.          |
| `FAIL`   | The check ran and the environment does not satisfy a required capability.          |
| `SKIP`   | The check is not relevant for this configuration (e.g. no ICMP targets defined).   |

## Checks

The doctor groups checks into sections. A section is omitted when it has no
checks to report.

### Config

- **load config** — parses and validates the YAML file. A `FAIL` here aborts
  the doctor run; later sections are skipped.

### Process

- **uid/gid/groups** — reports the effective UID, GID, and supplementary
  groups. Always present. Reported as `WARN` only if supplementary groups
  cannot be read.

### ListenAddr

- **bind `<addr>`** — attempts a TCP listen on `agent.listen_addr` (or the
  `--listen-addr` override) and immediately closes it. `FAIL` if the port is
  occupied or the address is unusable.

### ICMP

Runs only when the config contains at least one `icmp` target.

- **ping_group_range** — reads `/proc/sys/net/ipv4/ping_group_range` and
  checks whether the process effective GID or any supplementary GID is inside
  the range. `FAIL` if no GID is in range; `WARN` if the file cannot be read
  or parsed.
- **unprivileged socket** — opens a `SOCK_DGRAM` ICMP socket. `FAIL` if the
  kernel rejects the socket open.

On non-Linux builds the section reports `WARN` with `not supported on this
platform`.

### MTU

Runs only when the config contains at least one `mtu` target.

- **ping_group_range** — same check as ICMP, repeated under the MTU section
  for clarity.
- **ping socket + PMTUDISC** — opens an `IPPROTO_ICMP` ping socket and sets
  `IP_RECVERR` and `IP_MTU_DISCOVER=IP_PMTUDISC_PROBE`. `FAIL` if any of
  those operations is rejected. This validates the kernel features required
  by MTU probes, not just plain ICMP.

On non-Linux builds the section reports `WARN` with `not supported on this
platform`.

### DNS

Runs only when the config contains at least one `dns` target.

- **resolv.conf** / **resolv.conf nameservers** — reads `/etc/resolv.conf`
  and lists the configured nameservers. Severity depends on whether DNS
  targets rely on the system resolver:
  - `PASS` when at least one nameserver is configured.
  - `FAIL` when `/etc/resolv.conf` is missing or has no nameservers **and**
    one or more DNS targets do not set `probe_opts.dns_server`.
  - `WARN` when the same problem occurs but every DNS target sets
    `probe_opts.dns_server`, so the system resolver is unused.

## Sample Output

```text
NetSonar doctor

Config path: /etc/netsonar/config.yaml
Targets:
  http: 5
  icmp: 2
  mtu: 1

Config:
  [PASS] load config: targets=8

Process:
  [PASS] uid/gid/groups: uid=1000 gid=1000 groups=[1000]

ListenAddr:
  [PASS] bind :9275: ok

ICMP:
  [PASS] ping_group_range: 0 2147483647 includes gids [1000]
  [PASS] unprivileged socket: ok

MTU:
  [PASS] ping_group_range: 0 2147483647 includes gids [1000]
  [PASS] ping socket + PMTUDISC: ok

DNS:
  [SKIP] resolver: no dns targets in config

Result: OK
```

A failing check adds a `hint:` line under the result, for example:

```text
ICMP:
  [FAIL] ping_group_range: 0 0 does not include gids [1000]
    hint: Set net.ipv4.ping_group_range to include the process effective or supplementary GID.
  [FAIL] unprivileged socket: socket: operation not permitted
    hint: Ensure net.ipv4.ping_group_range includes the process effective or supplementary GID.

Result: FAIL
```

## When to Run

- **Before first start** on a new host or container image, to confirm the
  environment supports the configured probe types.
- **After kernel sysctl changes** (especially `net.ipv4.ping_group_range`).
- **After changing the runtime user, GIDs, or container security context**.
- **When troubleshooting** `probe_success=0` for ICMP, MTU, or DNS targets,
  before digging into the agent logs.

## Platform Support

ICMP and MTU checks require Linux. On other platforms those sections report
`WARN` with `not supported on this platform`. NetSonar itself is only
supported on Linux (see [README.md](../README.md#prerequisites)).
