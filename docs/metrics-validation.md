# Metrics Validation

NetSonar metrics are validated against independent reference tools, not only
against internal implementation details. The goal is to prove that emitted
values describe reality: timings map to the same phases used by common tooling,
binary match metrics mean what operators expect, and edge cases such as TLS
certificate chains or DNS CNAME normalization behave predictably.

## Validation Layers

| Layer | Tooling | Runs | Catches |
|---|---|---|---|
| CI integration tests | `httptest.Server`, controlled TLS/DNS servers, e2e lab | Every push | Regressions, off-by-one errors, phase attribution bugs |
| Local side-by-side lab | NetSonar and Prometheus Blackbox Exporter in `lab/metrics-validation/` | Manually on demand | Systematic HTTP phase drift, semantic mismatches |
| Production spot checks | `curl`, `openssl`, `dig`, `ping`, `tcpdump` | One-shot when needed | Environment-specific anomalies and real-world edge cases |

These layers are complementary. CI tests give deterministic regression coverage,
the local lab checks NetSonar against an independent exporter, and manual tools
remain useful when investigating a specific endpoint or network path.

## Reference Tool Mapping

| NetSonar metric | Reference tool | Expected relationship |
|---|---|---|
| `probe_duration_seconds` for HTTP | `curl -w '%{time_total}'` | Direct comparison when redirects and response body limits do not change semantics |
| `probe_http_status_code` | `curl -w '%{http_code}'` | Direct integer comparison |
| HTTP `dns_resolve` phase | `curl -w '%{time_namelookup}'` | Direct comparison |
| HTTP `tcp_connect` phase | `curl -w` values `time_connect` and `time_namelookup` | Delta comparison: subtract `time_namelookup` from `time_connect` |
| HTTP `tls_handshake` phase | `curl -w` values `time_appconnect` and `time_connect` | Delta comparison: subtract `time_connect` from `time_appconnect` |
| HTTP `ttfb` phase | Blackbox `probe_http_duration_seconds{phase="processing"}` | Direct comparison for empty/small requests; generated upload time is split into NetSonar `request_write` |
| HTTP `transfer` phase | `curl -w` values `time_total` and `time_starttransfer` | Delta comparison: subtract `time_starttransfer` from `time_total` |
| `probe_icmp_avg_rtt_seconds` | `ping -c N` average RTT | Direct comparison for the same ICMP type |
| `probe_icmp_stddev_rtt_seconds` | Per-reply `ping -D` or `fping -C N` RTTs | Compute population standard deviation; do not compare to Linux `ping` mdev |
| `probe_icmp_packet_loss_ratio` | `ping -c N` loss percentage | Convert percent to ratio |
| `probe_mtu_bytes` | `ping -M do -s N` with binary search | Use ICMP with DF, not `tracepath` UDP |
| `probe_dns_resolve_seconds` | `dig @server +stats` query time | Compare after converting milliseconds to seconds, using the same DNS server and query type |
| `probe_dns_result_match` | `dig @server <name> <type> +short` | Binary match after applying NetSonar's normalization rules |
| `probe_tls_cert_expiry_timestamp_seconds` | `openssl s_client -showcerts` plus `openssl x509 -noout -enddate` for every cert | NetSonar should report the earliest `NotAfter` across the peer chain |
| `probe_tls_cert_chain_expiry_timestamp_seconds` | `openssl s_client -showcerts` plus per-cert `openssl x509 -enddate` | Per-certificate timestamp comparison |
| `probe_http_body_match` | `curl -s URL` plus `grep -E` or fixed-string comparison | Binary match/mismatch; status validation is separate |
| `probe_http_response_truncated` | Controlled server/test only | Internal transfer-limit behavior; no external one-command equivalent |
| `probe_success` for proxy probes | `curl -x http://proxy:3128 -w '%{http_code}'` | Success/failure only; duration is not comparable |

For DNS result matching, compare A and AAAA probes only against IP address
answers. `dig +short A` may include CNAME lines before final IPs, while
NetSonar's IP lookup path returns only IP strings. For CNAME probes, compare the
canonical name after lowercasing and stripping a trailing dot.

For TLS certificate expiry, do not compare only the leaf certificate. NetSonar
records the earliest expiry across the full peer chain, so the reference check
must inspect every certificate returned by `openssl s_client -showcerts`.

## HTTP Phase Semantics

`curl` reports cumulative times from the start of the request. NetSonar reports
per-phase deltas. Use these mappings:

| NetSonar value | curl value |
|---|---|
| `dns_resolve` | `time_namelookup` |
| `dns_resolve + tcp_connect` | `time_connect` |
| `dns_resolve + tcp_connect + tls_handshake` | `time_appconnect` |
| `probe_duration_seconds` | `time_total` |
| HTTPS `ttfb` | `time_starttransfer - time_appconnect` for empty/small requests |
| Plain HTTP `ttfb` | `time_starttransfer - time_connect` for empty/small requests |
| `transfer` | `time_total - time_starttransfer` |

NetSonar's `request_write` starts after TCP connect and, for HTTPS, after TLS
handshake. NetSonar's `ttfb` starts after request write completion. For
empty/small requests this remains close to Prometheus Blackbox Exporter's
`processing` phase and browser-style Navigation Timing semantics; for generated
request bodies, upload time is separated into `request_write`.

## Timing Thresholds

Single probe timings vary substantially on noisy systems. Use medians or p50
windows rather than single samples when comparing user-space tools.

The metrics validation dashboard uses p50 over a 5-minute range. For the local
Docker-network HTTP/HTTPS validation targets, the expected pass/fail guidance is:

- p50 absolute phase delta should be near or below `5ms` for tiny local phases.
- p50 normalized phase delta should be near or below `20%` when the reference
  phase p50 exceeds `25ms`.
- Isolated spikes are not semantic failures. Investigate phases that stay above
  threshold for 10-15 minutes.

## Current Automated Coverage

The following coverage is currently implemented:

| Area | Coverage |
|---|---|
| TLS certificate expiry | Controlled single-cert and chain tests in `internal/probe/tls_cert_test.go`, including intermediate-earliest and leaf-earliest cases |
| TLS expiry metric export | `TestRecord_TLSCertExpiry` in `internal/metrics/metrics_test.go` asserts the exported Unix timestamp |
| HTTP phase attribution | Property coverage for non-overlapping phase sums plus targeted controlled-delay tests for TTFB and transfer attribution in `internal/probe/http_test.go` |
| DNS result matching | Property tests for normalization plus integration tests through `DNSProber` with controlled A, AAAA, CNAME, match, and mismatch responses |
| ICMP and MTU happy path | Docker e2e lab assertions in `lab/e2e/` with controlled `ping_group_range` |
| HTTP phase side-by-side comparison | `lab/metrics-validation/` runs NetSonar and Blackbox Exporter against the same local HTTP/HTTPS targets |

## Metrics Validation Lab

Use `lab/metrics-validation/` when you want to check whether NetSonar's
comparable HTTP phases still match an independent reference prober:

```sh
make lab-mv
```

Open Grafana at `http://localhost:3000` and use the **Metrics Validation**
dashboard. The lab compares these phase pairs:

| NetSonar phase | Blackbox phase |
|---|---|
| `dns_resolve` | `resolve` |
| `tcp_connect` | `connect` |
| `tls_handshake` | `tls` |
| `request_write` | Included in `processing` |
| `ttfb` | `processing` minus request-write time |
| `transfer` | `transfer` |

The lab intentionally validates only local HTTP and HTTPS probes. It does not
validate DNS, ICMP, MTU, proxy, Internet targets, or TLS certificate expiry.
Those areas are covered by targeted tests, e2e checks, or manual reference
commands.

Stop the lab with:

```sh
make lab-mv-down
```

## Manual Spot Checks

Manual spot checks are useful when a production target looks suspicious or when
validating behavior outside the local lab.

For HTTP timings, take multiple samples and compare medians. Avoid redirects
unless redirect semantics are the specific thing being investigated.

For TLS expiry, extract the full certificate chain and compare the minimum
`NotAfter` timestamp to `probe_tls_cert_expiry_timestamp_seconds`.

For DNS, force the same query type and server on both sides, for example by
matching NetSonar's `dns_server` with `dig @server`.

For MTU, use `ping -M do -s N` and binary search packet sizes. Do not use
`tracepath` as the reference because it uses UDP while NetSonar's MTU prober
uses ICMP with DF.

For packet-level ground truth, capture traffic from a local lab NetSonar
container with `tcpdump`, then compare TCP handshake, TLS handshake, and first
response byte timestamps with NetSonar's reported phase durations. This is too
heavy for routine automation but useful when locking down phase semantics.

## Caveats

**Timing variance.** Single samples can differ by multiples even on the same
host. Prefer medians or p50 windows.

**Redirects.** When `follow_redirects: true` is enabled, NetSonar's phase fields
reflect the last connection in the redirect chain while total duration includes
the whole chain. Direct curl phase comparison is only clean with
non-redirecting targets or `follow_redirects: false`.

**HTTP transfer cap.** NetSonar reads at most `response_body_limit_bytes` from the
response body. If the body exceeds that cap, total duration and transfer phase
describe the capped read, while curl normally reads the full body.

**DNS resolver caching and transport.** NetSonar measures the Go resolver path,
including the dial to a configured DNS server. `dig +stats` reports query time
from a different implementation. For UDP this should be small noise; TCP
fallback should not be used for routine timing comparisons unless explicitly
tested.

**DNS query type.** `dig` defaults to A records. Match the configured
`dns_query_type` explicitly when comparing A, AAAA, or CNAME probes.

**ICMP and MTU address family.** NetSonar's ICMP and MTU probers use IPv4
transports. Reference commands should use IPv4 addresses or force IPv4 with
`ping -4`.

**TLS expiry timezone.** OpenSSL and NetSonar both operate on absolute UTC
timestamps for certificate expiry. Timestamp comparisons should be exact; day
bucket differences usually indicate dashboard or presentation logic.
