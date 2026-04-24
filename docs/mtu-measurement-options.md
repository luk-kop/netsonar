# MTU Measurement Options

## Purpose

This document compares practical ways NetSonar could measure or estimate path
MTU. It is intentionally separate from the current implementation guide:

- `docs/mtu-pmtud.md` describes how the current NetSonar MTU probe works.
- This document describes available design options and their trade-offs.
- It also records the current decision: ship the descending checkpoint algorithm
  unchanged.

The core question is not "can we send larger and smaller packets?" The core
question is what signal proves that a given packet size crossed the path, how
expensive failed probes are, and how ambiguous the result becomes when ICMP,
UDP, or application replies are filtered.

## Terms

| Term | Meaning |
|---|---|
| Link MTU | Maximum IP packet size supported by one link. |
| Path MTU / PMTU | Minimum link MTU across all links between sender and target. |
| Probe size | Full IP packet size being tested, including IP header. MTU probes require DF / no-fragmentation behavior; if the network fragments oversized packets, the probe loses the size signal. |
| ICMP payload size | ICMP Echo payload bytes. For IPv4 with a standard 20-byte header and no IP options, `PMTU = payload + 28`. |
| Confirmation signal | Evidence that a probe of a specific size reached the target or was otherwise accepted by the path. |
| Too-large signal | Evidence that a probe was too large for the path, usually ICMP Fragmentation Needed / Packet Too Big or local `EMSGSIZE`. |
| Timeout | No useful signal before the deadline. A timeout is ambiguous. |

## External References

- RFC 1191: classical IPv4 Path MTU Discovery using DF and ICMP Fragmentation Needed. Relevant to Option 1 and Option 4: <https://www.rfc-editor.org/rfc/rfc1191>
- RFC 4821: Packetization Layer Path MTU Discovery search framework. Relevant to Option 2 and Option 5: <https://www.rfc-editor.org/rfc/rfc4821>
- RFC 8899: Datagram Packetization Layer Path MTU Discovery. Relevant to Option 5 and future UDP-style designs: <https://www.rfc-editor.org/rfc/rfc8899>
- Linux `ip(7)` socket options including `IP_MTU_DISCOVER`, `IP_PMTUDISC_PROBE`, and `IP_RECVERR`. Relevant to Option 1 and Option 4: <https://www.man7.org/linux/man-pages/man7/ip.7.html>

## Option Summary

| Option | Main signal | Best fit | Main weakness | NetSonar fit |
|---|---|---|---|---|
| ICMP descending checkpoints | ICMP Echo Reply and ICMP Fragmentation Needed | Operational threshold monitoring | Coarse result, expensive black-hole timeouts | Current default |
| ICMP adaptive upward search | ICMP Echo Reply and ICMP Fragmentation Needed | Better PMTU estimate with bounded cost | More state and config | Deferred |
| ICMP pure binary search | ICMP Echo Reply and ICMP Fragmentation Needed | Controlled lab networks | Fragile under ambiguous timeouts | Weak default |
| UDP error-queue probing | ICMP errors, UDP replies, Port Unreachable | Hosts where UDP is allowed but ping sockets are not | Requires UDP reachability or useful ICMP errors | Secondary backend |
| Application-layer PLPMTUD | Application/protocol acknowledgments | Protocols with real request/ack semantics | Needs protocol integration | Not generic NetSonar MTU |
| TCP MSS/connect inference | TCP handshake/options or application success | TCP service health and MSS sanity | Does not directly measure arbitrary PMTU | Diagnostic only |
| OS/interface MTU inspection | Local interface/route data | Local host diagnostics | Not end-to-end | Not sufficient |
| Passive observation | Existing traffic behavior | Low overhead environments with traffic | No active guarantee, hard attribution | Out of scope for current agent |

## Option 1: ICMP Descending Checkpoints

This is the current NetSonar implementation.

The agent sends ICMP Echo requests with IPv4 DF enabled. It starts from the
largest configured payload and steps down through a list until one probe gets an
Echo Reply. For IPv4 with a standard 20-byte header and no IP options:

```text
path MTU = successful ICMP payload + 8 byte ICMP header + 20 byte IPv4 header
```

Example default checkpoints:

| ICMP payload | Reported PMTU |
|---:|---:|
| 1472 | 1500 |
| 1392 | 1420 |
| 1372 | 1400 |
| 1272 | 1300 |
| 1172 | 1200 |
| 1072 | 1100 |

### How It Works

1. Resolve the target to IPv4.
2. Send a small ICMP sanity Echo.
3. Try configured payload sizes in descending order.
4. Treat Echo Reply as success for that size.
5. Treat ICMP Destination Unreachable code 4 as too large.
6. Treat other Destination Unreachable codes as reachability or policy failure.
7. Treat timeout as ambiguous and try the next smaller configured size.
8. Stop at the first successful configured payload.

### What It Measures

It measures the largest configured checkpoint that produced an ICMP Echo Reply,
not necessarily the largest size the path actually carries. It does not measure
the exact PMTU unless the true PMTU happens to equal one of the configured
checkpoints.

### Pros

- Simple implementation and simple mental model.
- Good for monitoring known operational thresholds such as 1500, 1420, 1400.
- Easy to explain in dashboards and alerts.
- Works without opening TCP or UDP application ports on the target.
- With Linux ping sockets, does not require `CAP_NET_RAW`; it requires
  `net.ipv4.ping_group_range`.

### Cons

- Coarse result. A real PMTU of 1460 collapses to the next lower configured
  checkpoint.
- Descending order is costly on black-hole paths when ICMP Fragmentation Needed
  is filtered end to end. If the error is received, Linux delivers it through the
  socket error queue quickly; if it is dropped, each oversized checkpoint can
  consume the full per-attempt timeout and retry budget.
- Result quality depends heavily on a hand-curated payload list.
- It can under-report when all higher successful-but-untested values are skipped.

With the current defaults, a full black-hole run can cost up to:

```text
6 checkpoints * 3 retries * 2s per attempt = 36s
```

That can exceed a typical 30s target timeout, so timeout budgets and checkpoint
lists must be chosen together.

### Risks

- Operators may read `probe_mtu_bytes` as exact PMTU when it is only a confirmed
  checkpoint.
- ICMP filtering can make MTU failures look like target silence or packet loss.
- A small ICMP sanity Echo can pass while larger DF probes timeout, so success of
  the sanity check does not prove PMTUD feedback works.

### NetSonar Decision

This is the current default. Keep this mode for threshold monitoring. Do not
present it as exact PMTU measurement.

## Option 2: ICMP Adaptive Upward Search

This approach keeps the current ICMP/DF backend but changes the search strategy.
It is inspired by PLPMTUD: first confirm a conservative lower size, then probe
larger sizes until the search converges or the probe budget is exhausted.

This is not full RFC 4821 or RFC 8899 PLPMTUD because NetSonar is not integrated
with a transport or application packetization layer. It is an active diagnostic
probe using similar search principles.

### How It Works

1. Resolve the target to IPv4.
2. Send a small ICMP sanity Echo.
3. Confirm a configured safe lower MTU, such as `min_mtu`.
4. Set `search_low` to the largest confirmed working size.
5. Set `search_high` to the configured maximum size, or to a too-large size when
   one is observed.
6. Probe larger sizes using a bounded strategy.
   Known common sizes such as 1400, 1420, 1500, or tunnel-aware values should be
   tried before binary refinement inside the remaining bracket.
7. On Echo Reply, raise `search_low`.
8. On validated too-large signal, lower `search_high`.
9. On timeout, retry within policy. After the retry budget, do not treat timeout
   as proof of a smaller PMTU unless the mode explicitly chooses conservative
   under-reporting. A safer default is to keep the last confirmed `search_low`,
   mark the search window as inconclusive, and stop or probe a lower common size.
10. Stop when the bracket is small enough or `max_search_probes` is reached.

### What It Measures

It estimates the largest confirmed working ICMP/DF packet size within configured
bounds and resolution. It can be more precise than checkpoints while still
keeping a hard ceiling on probe cost.

### Pros

- Better precision than static checkpoints.
- Avoids starting every probe at the largest possible size.
- More consistent with RFC 4821's search framework: prove a safe lower bound,
  then cautiously raise it.
- Can still prioritize operationally meaningful MTUs before doing refinement.
- Easier to bound than an open-ended search.

### Cons

- More implementation state than checkpoint scanning.
- Requires additional configuration or clear defaults:
  `min_mtu`, `max_mtu`, `search_resolution_bytes`, `max_search_probes`.
- Timeout semantics need careful design.
- Dashboards and docs must distinguish "confirmed PMTU within resolution" from
  "exact PMTU".

The probe budget must be explicit. For example, with `mtu_max_search_probes=8`,
`mtu_retries=3`, and `mtu_per_attempt_timeout=2s`, a worst-case run can cost:

```text
8 search probes * 3 retries * 2s per attempt = 48s
```

That is worse than the current checkpoint worst case unless the strategy stops
early, uses fewer retries for refinement, or has a smaller search-probe cap.

### Risks

- If timeouts are treated as hard too-large failures, packet loss or ICMP
  filtering can under-report PMTU.
- If timeouts are treated as purely inconclusive, the search may return a lower
  bound that is correct but less precise.
- If the target rate-limits or deprioritizes ICMP, repeated probing can produce
  unstable results.

### NetSonar Decision

This is the strongest future candidate if NetSonar wants `probe_mtu_bytes` to
mean a real PMTU estimate rather than only a threshold checkpoint. It is deferred
unless operator feedback shows that checkpoint granularity is not enough.

## Option 3: ICMP Pure Binary Search

Pure binary search tests the midpoint of a configured range and halves the range
after each conclusive result.

### How It Works

1. Set lower and upper bounds, for example 1100 and 1500.
2. Probe the midpoint.
3. On Echo Reply, move the lower bound up.
4. On too-large signal, move the upper bound down.
5. Repeat until the bracket is below the desired resolution.

### What It Measures

In a clean monotonic network, it converges quickly to the largest working size.

### Pros

- Efficient in clean test environments.
- Easy to reason about mathematically.
- Good when every probe result is reliable and monotonic.

### Cons

- Real networks do not always give clean boolean results.
- Timeout is ambiguous: it can mean too large, ICMP filtered, target silence,
  packet loss, policy drop, or rate limiting.
- A single high midpoint timeout can throw away a large part of the search space
  unless the implementation has fallback logic.
- It ignores common operational MTU sizes unless explicitly biased.

### Risks

- Under-reporting PMTU due to unrelated packet loss.
- Unstable results when the target or path treats ICMP inconsistently.
- More difficult operator explanation than either checkpoints or adaptive
  common-size probing.

### NetSonar Decision

Do not use pure binary search as the default. If implemented at all, it should be
a lab/debug mode or an internal refinement step after a reliable bracket exists.

## Option 4: UDP Error-Queue Probing

Linux can perform PMTU probing with ordinary UDP sockets by enabling PMTUD and
reading ICMP errors from the socket error queue with `IP_RECVERR`. This is the
family of technique used by tools such as `tracepath`.

### How It Works

1. Open a UDP socket.
2. Enable PMTUD-related socket options and `IP_RECVERR`.
3. Send UDP datagrams of controlled size.
4. Read normal UDP responses if the target service replies.
5. Read ICMP errors from the error queue.
6. Interpret ICMP Fragmentation Needed as too large.
7. Interpret ICMP Port Unreachable from the target as useful proof that the
   datagram reached the target host.

### What It Measures

It estimates the PMTU for UDP packets to a target and port under the current
network policy.

### Pros

- Does not require `CAP_NET_RAW`.
- Does not require Linux ping sockets or `ping_group_range`.
- A closed UDP port can be useful because ICMP Port Unreachable confirms host
  reachability for that datagram size.
- Similar to existing diagnostic tooling.

### Cons

- Requires UDP traffic to reach the target or a useful ICMP error to return.
- Many cloud networks and firewalls silently drop unsolicited UDP.
- A UDP path may have different policy from the ICMP path operators care about.
- If a target firewall drops UDP before the host sees it, the result is often
  only timeout.

### Risks

- Operators may need to open a UDP port or allow UDP to many targets, which can
  be less acceptable than allowing scoped ICMP diagnostics.
- ICMP Port Unreachable can be filtered, removing the useful success signal.
- NAT, firewalls, and security groups can make results environment-specific.

### NetSonar Decision

Useful as a secondary backend for environments where UDP is easier to permit
than ICMP ping sockets. It should not silently replace the ICMP backend because
the operational requirements and failure semantics are different.

## Option 5: Application-Layer PLPMTUD

Application-layer PLPMTUD uses real application or transport acknowledgments to
confirm that larger packets are delivered. RFC 4821 describes the general
packetization-layer approach; RFC 8899 adapts it for datagram transports.

### How It Works

1. A protocol sends real data or padded probe packets at controlled sizes.
2. The protocol confirms delivery using its own acknowledgment mechanism.
3. On success, it raises the usable packet size.
4. On validated Packet Too Big or probe loss, it lowers or caps the search.
5. It periodically revalidates because paths can change.

### What It Measures

It measures the effective PMTU for a specific protocol flow, including the
headers and behavior of that protocol.

### Pros

- Does not depend solely on ICMP.
- Measures the path as the application actually uses it.
- Can work through networks where ICMP Packet Too Big is filtered.
- Well aligned with modern transports such as QUIC when implemented by the
  transport.

### Cons

- Requires protocol integration and reliable delivery confirmation.
- Not generic across arbitrary targets.
- Can affect real traffic if not carefully isolated.
- Needs per-protocol state and timers.

### Risks

- Incorrect implementation can confuse congestion loss with MTU loss.
- Probe traffic may be visible to or affect the application.
- It is hard to expose as one generic `mtu` probe without choosing a specific
  application protocol.

### NetSonar Decision

Not a generic MTU backend for the current agent. It is relevant for future
protocol-specific probes, for example QUIC or a controlled HTTP/UDP service that
can acknowledge padded requests.

## Option 6: TCP MSS or TCP Connect Inference

TCP-related checks can inspect or infer maximum segment size behavior, but they
do not directly measure arbitrary path MTU.

### How It Works

Possible approaches:

- inspect TCP MSS options during handshakes,
- attempt TCP connections to a service and observe success/failure,
- send application data of controlled sizes and observe whether it completes.

### What It Measures

It measures TCP behavior for a specific service, not the path's general PMTU.
MSS can be clamped by hosts, firewalls, VPNs, or load balancers.

### Pros

- Uses traffic that many environments already allow.
- Useful for diagnosing TCP application connectivity.
- Can reveal MSS clamping or service-specific MTU problems.

### Cons

- TCP segmentation, MSS clamping, offloads, retransmission, and application
  buffering hide the raw PMTU.
- A successful TCP connect says little about larger packet delivery.
- Not useful for targets without a TCP service.

### Risks

- Easy to overinterpret as PMTU measurement.
- Results can be dominated by middlebox behavior rather than actual path MTU.

### NetSonar Decision

Useful as a TCP diagnostic, not as the primary MTU probe.

## Option 7: OS Interface, Route, or PMTU Cache Inspection

The agent could inspect local interface MTU, route MTU, or kernel PMTU cache
values.

### How It Works

Possible sources:

- local interface MTU,
- route attributes,
- socket `IP_MTU`, when a connected socket has observed PMTU information,
- platform-specific network APIs.

### What It Measures

It measures local host or kernel state. It does not independently confirm that a
packet of that size reaches the remote target.

On a fresh connected socket, `IP_MTU` may reflect local route or interface state
rather than an end-to-end PMTU learned from traffic. It becomes more meaningful
after the socket has observed PMTU feedback.

### Pros

- Low overhead.
- Useful for local diagnostics.
- Can explain local `EMSGSIZE` failures.

### Cons

- Not end-to-end.
- Platform-specific.
- PMTU cache can be stale, missing, or affected by previous traffic.
- Does not detect remote-path filtering or asymmetric return path problems.

### Risks

- Reporting local MTU as path MTU would be misleading.
- Cache-derived values may change outside NetSonar's probe lifecycle.

### NetSonar Decision

Useful as supporting diagnostic metadata, not as the source of
`probe_mtu_bytes`.

## Option 8: Passive Observation

Passive observation infers MTU behavior from existing traffic rather than active
probes.

### How It Works

The agent observes packets, segment sizes, retransmissions, ICMP Packet Too Big
messages, or flow behavior and infers likely MTU constraints.

### What It Measures

It measures what existing traffic happened to exercise. It does not guarantee
that a specific path or target supports a specific MTU.

### Pros

- No extra probe traffic.
- Can discover real production behavior.
- Useful in packet-capture or eBPF-heavy systems.

### Cons

- Requires visibility into traffic.
- Needs elevated privileges or kernel integrations in many deployments.
- Hard to attribute observations to configured probe targets.
- No signal when there is no relevant traffic.

### Risks

- Privacy and security concerns.
- High implementation complexity.
- Easy to produce misleading inferences.

### NetSonar Decision

Out of scope for the current agent.

## IPv6 Considerations

IPv6 is not just IPv4 with a different header size. IPv6 routers do not fragment
transit packets; Packet Too Big handling uses ICMPv6 and different socket
options and validation rules.

Any IPv6 MTU implementation should be designed explicitly rather than bolted
onto the current IPv4 ICMP code path.

Key differences:

- IPv6 minimum link MTU is 1280 bytes.
- ICMPv6 Packet Too Big is required for PMTUD.
- Header accounting differs from IPv4.
- Socket options and error handling differ by platform.
- Firewalls often handle ICMPv6 differently from ICMPv4.

Option mapping:

| IPv4 option | IPv6 equivalent |
|---|---|
| ICMP descending checkpoints | ICMPv6 Echo checkpoints with Packet Too Big handling and a natural lower floor of 1280. |
| ICMP adaptive upward search | Same search concept, usually with `search_low` seeded at or above 1280. |
| UDP error-queue probing | UDP with IPv6 PMTU socket options such as `IPV6_RECVPATHMTU` / `IPV6_PATHMTU`, subject to platform support. |
| Application-layer PLPMTUD | Still applicable; RFC 8899 is especially relevant for datagram transports. |
| TCP MSS/connect inference | Still diagnostic only; TCP and middlebox behavior can hide raw PMTU. |
| OS/interface/cache inspection | Still local state, not end-to-end confirmation. |
| Passive observation | Still possible but out of scope for the current agent. |

## Relationship to the Current Decision

This file is both the option catalog and the current MTU search-strategy
decision record.

- The current release ships Option 1, the ICMP descending checkpoint algorithm.
- `probe_mtu_bytes` means the largest successful configured ICMP payload plus 28,
  not exact PMTU.
- Operators who need finer granularity can add more entries to
  `icmp_payload_sizes`.
- Option 2, adaptive upward search, remains the most relevant future path if
  users need more precise PMTU estimates.
- Pure binary search is not a good default for NetSonar's monitoring use-case
  because production paths often turn failed probes into ambiguous timeouts, not
  clean too-large signals.
- `expected_min_mtu` is a health threshold, not a search lower bound.

## Final Position

The current checkpoint implementation is valid for operational threshold
monitoring. It is not a precise PMTU measurement algorithm.

For the current release, that is the intended contract. More precise PMTU
measurement belongs in future work after operator feedback shows that checkpoint
granularity is not enough.
