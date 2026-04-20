# Operations Guide

## Multiple Agents on One Host

Running two NetSonar agents on the same VM is supported when you want to
isolate ingestion paths, for example one agent scraped by local VictoriaMetrics
and another publishing the same probe set to a remote pipeline.

Use separate HTTP listen addresses. Two processes cannot bind the same
`agent.listen_addr`, so each config needs a distinct port:

```yaml
# Agent A
agent:
  listen_addr: ":9275"

# Agent B
agent:
  listen_addr: ":9276"
```

Both agents execute their own probes. With identical target configs this
doubles probe traffic and can make probe bursts line up if both processes start
at the same time. Account for this when choosing intervals and timeouts.

Probe-type notes:

- TCP, HTTP, HTTP body, DNS, TLS certificate, and proxy probes are independent
  per process.
- ICMP probes use unprivileged datagram ICMP sockets. The kernel manages ICMP
  IDs and filters replies per socket, so cross-talk between agents is not
  expected.
- MTU probes use connected Linux ICMP ping sockets. The kernel manages ICMP IDs
  and filters replies by connected peer, while the prober also checks Echo
  sequence numbers. Raw-socket host-wide ICMP cross-talk is not expected.

For duplicate agents with identical MTU targets, prefer one of these operating
models:

- Use different startup timing or supervision delays so the two agents do not
  send identical probe bursts at the same time.
- Use longer MTU intervals than lightweight TCP/HTTP checks.
- Set `agent.initial_probe_jitter` to spread each target's first probe after
  startup or reload.

## Scrape Interval Alignment

Most NetSonar probe metrics are current-observation Gauges: each probe run overwrites the previous value for that target. To preserve per-probe visibility, scrape at the shortest probe interval or faster.

**Rule of thumb: set your scrape interval equal to, or a divisor of, the shortest probe interval.**

For example, with `default_interval: 30s`:

- **30s scrape interval** — recommended baseline; usually gives one sample per probe cycle, subject to normal scheduler/scrape timing drift.
- **15s scrape interval** — gives extra samples between probe runs. Gauge values repeat until the next probe result, which is expected.
- **>30s scrape interval** (e.g. 60s) — loses per-cycle visibility for Gauge metrics because intermediate probe results can be overwritten before Prometheus scrapes them. Counter rates still work, but with lower time resolution and more sensitivity to sparse range windows or reload resets.

For targets with longer intervals (e.g. MTU or TLS cert probes at `300s`), a 30s scrape interval is still fine — you simply read the same value multiple times between probe executions, which is correct behavior for Gauges.

If you run a mix of probe intervals (e.g. 30s for TCP/HTTP and 300s for MTU), scrape at the shortest interval (30s).

## Counter Resets on Reload

`probe_skipped_overlap_total` is a Prometheus counter, but NetSonar deletes a
target's metric series when that target is removed or materially changed during
a config reload. If the same Prometheus series appears again after reload
(same scrape labels such as `job`/`instance`, plus the same NetSonar labels such
as `target`, `target_name`, `probe_type`, `network_path`, and dynamic tag
labels), Prometheus may observe the series starting from zero and treat it as a
counter reset.

This reset is expected after reloads that recreate target series. Use PromQL
functions such as `increase()` or `rate()` over a range that tolerates counter
resets when alerting on skipped overlaps.

## Dashboard Layout

Two Grafana dashboards are provisioned from `grafana/dashboards/`:

- **NetSonar** (`netsonar.json`, uid `netsonar`) — overview of all probe types.
  The HTTP section includes overview panels (topk, phase breakdown table, bar
  gauge) that answer *"what is slow?"* across all HTTP targets.
- **NetSonar — HTTP Details** (`netsonar-http-details.json`, uid
  `netsonar-http-details`) — per-target HTTP drill-down with duration stats,
  stacked phase timing, status codes, and success rate.

Navigation: click a `target_name` cell in the phase breakdown table (B2) on the
main dashboard to jump to the details dashboard with that target pre-selected
and the time range preserved. The details dashboard has a "← Back to Overview"
link in the top bar.
