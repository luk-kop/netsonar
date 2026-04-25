# Metrics Validation Lab

Local side-by-side validation for NetSonar HTTP phase metrics.

This lab runs NetSonar and Prometheus Blackbox Exporter against the same
controlled fake targets, then provisions Grafana with the **Metrics Validation**
dashboard.

## Purpose

Use this lab to check whether NetSonar's comparable HTTP phase metrics have the
same semantics as an independent reference prober:

| NetSonar phase | Blackbox phase |
|---|---|
| `dns_resolve` | `resolve` |
| `tcp_connect` | `connect` |
| `tls_handshake` | `tls` |
| `request_write` | Included in `processing` |
| `ttfb` | `processing` minus request-write time |
| `transfer` | `transfer` |

The lab is intentionally local and deterministic. It is not an Internet
benchmark and it does not validate DNS, ICMP, MTU, proxy, or certificate-expiry
metrics.

## Run

```sh
make lab-mv
```

Open Grafana at [http://localhost:3000](http://localhost:3000), then open the
**Metrics Validation** dashboard under the **NetSonar** folder.

Prometheus is available at [http://localhost:9090](http://localhost:9090).

## Targets

The validation compares only two local targets:

- `http-ok` → `http://fake-targets:8080/ok`
- `https-ok` → `https://fake-targets:9443/ok`

The HTTPS Blackbox module uses `fail_if_not_ssl: true` and
`insecure_skip_verify: true` so it validates TLS timing against the lab's
self-signed target without turning the check into a trust-chain test.

## Interpretation

Use 5-15 minutes of data before interpreting deltas. The dashboard uses p50 over
a 5-minute window so occasional scheduler or Docker-network outliers do not
dominate the verdict.

Expected result:

- `Blackbox Success` is `1`.
- p50 absolute phase delta is near or below `5ms` for tiny Docker-network
  phases.
- p50 normalized phase delta is near or below `20%`.
- NetSonar and Blackbox total durations are in the same scale; exact equality
  is not expected.

Investigate only phases that remain above threshold for 10-15 minutes. Isolated
spikes are useful debugging data, but they are not a semantic validation failure
by themselves.

## Stop

```sh
make lab-mv-down
```
