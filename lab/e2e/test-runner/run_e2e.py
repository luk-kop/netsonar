import os
import re
import sys
import time
import urllib.request


METRICS_URL = os.environ.get("METRICS_URL", "http://netsonar:9275/metrics")


EXPECTATIONS = [
    ("probe_success", {"target_name": "tcp-open"}, 1.0),
    ("probe_success", {"target_name": "tcp-closed"}, 0.0),
    ("probe_success", {"target_name": "http-ok"}, 1.0),
    ("probe_http_status_code", {"target_name": "http-ok"}, 200.0),
    ("probe_success", {"target_name": "http-500-expected-200"}, 0.0),
    ("probe_http_status_code", {"target_name": "http-500-expected-200"}, 500.0),
    ("probe_success", {"target_name": "http-500-any-status"}, 1.0),
    ("probe_http_status_code", {"target_name": "http-500-any-status"}, 500.0),
    ("probe_success", {"target_name": "http-no-response"}, 0.0),
    ("probe_success", {"target_name": "http-upload-64k"}, 1.0),
    ("probe_http_status_code", {"target_name": "http-upload-64k"}, 200.0),
    ("probe_success", {"target_name": "http-body-ok"}, 1.0),
    ("probe_http_body_match", {"target_name": "http-body-ok"}, 1.0),
    ("probe_http_status_code", {"target_name": "http-body-ok"}, 200.0),
    ("probe_success", {"target_name": "http-body-mismatch"}, 0.0),
    ("probe_http_body_match", {"target_name": "http-body-mismatch"}, 0.0),
    ("probe_http_status_code", {"target_name": "http-body-mismatch"}, 200.0),
    ("probe_success", {"target_name": "http-body-bad-status"}, 0.0),
    ("probe_http_body_match", {"target_name": "http-body-bad-status"}, 1.0),
    ("probe_http_status_code", {"target_name": "http-body-bad-status"}, 500.0),
    ("probe_success", {"target_name": "http-via-proxy"}, 1.0),
    ("probe_http_status_code", {"target_name": "http-via-proxy"}, 200.0),
    ("probe_success", {"target_name": "proxy-connect-ok"}, 1.0),
    ("probe_success", {"target_name": "proxy-connect-denied"}, 0.0),
    ("probe_success", {"target_name": "tls-cert-via-proxy"}, 1.0),
    ("probe_success", {"target_name": "tls-cert-connect-fail"}, 0.0),
    ("probe_success", {"target_name": "dns-fake-targets"}, 1.0),
    ("probe_success", {"target_name": "dns-localhost-match"}, 1.0),
    ("probe_dns_result_match", {"target_name": "dns-localhost-match"}, 1.0),
    ("probe_success", {"target_name": "dns-localhost-mismatch"}, 0.0),
    ("probe_dns_result_match", {"target_name": "dns-localhost-mismatch"}, 0.0),
    ("probe_success", {"target_name": "icmp-fake-targets"}, 1.0),
    ("probe_icmp_packet_loss_ratio", {"target_name": "icmp-fake-targets"}, 0.0),
    ("probe_success", {"target_name": "icmp-single-reply"}, 1.0),
    ("probe_icmp_packet_loss_ratio", {"target_name": "icmp-single-reply"}, 0.0),
    ("probe_success", {"target_name": "mtu-fake-targets"}, 1.0),
    ("probe_success", {"target_name": "mtu-no-resolve"}, 0.0),
    ("agent_info", {"version": "e2e"}, 1.0),
]

RANGE_EXPECTATIONS = [
    ("probe_dns_resolve_seconds", {"target_name": "dns-fake-targets"}, 0.0, 2.0),
    ("probe_duration_seconds", {"target_name": "icmp-fake-targets"}, 0.0, 3.0),
    ("probe_icmp_avg_rtt_seconds", {"target_name": "icmp-fake-targets"}, 0.0, 3.0),
    ("probe_icmp_stddev_rtt_seconds", {"target_name": "icmp-fake-targets"}, 0.0, 3.0),
    ("probe_duration_seconds", {"target_name": "icmp-single-reply"}, 0.0, 3.0),
    ("probe_icmp_avg_rtt_seconds", {"target_name": "icmp-single-reply"}, 0.0, 3.0),
    ("probe_phase_duration_seconds", {"target_name": "http-upload-64k", "phase": "request_write"}, 0.0, 2.0),
    ("probe_duration_seconds", {"target_name": "mtu-fake-targets"}, 0.0, 8.0),
    ("probe_mtu_bytes", {"target_name": "mtu-fake-targets"}, 1200.0, 10000.0),
    ("probe_tls_cert_expiry_timestamp_seconds", {"target_name": "tls-cert-via-proxy"}, 1700000000.0, 2300000000.0),
]

STATE_EXPECTATIONS = [
    ("probe_mtu_state", {"target_name": "mtu-fake-targets", "state": "ok"}, 1.0),
    ("probe_mtu_state", {"target_name": "mtu-no-resolve", "state": "error", "detail": "resolve_error"}, 1.0),
]

ABSENT_EXPECTATIONS = [
    ("probe_http_status_code", {"target_name": "http-no-response"}),
    ("probe_http_response_truncated", {"target_name": "http-no-response"}),
    ("probe_tls_cert_expiry_timestamp_seconds", {"target_name": "tls-cert-connect-fail"}),
    ("probe_dns_result_match", {"target_name": "dns-fake-targets"}),
    ("probe_icmp_stddev_rtt_seconds", {"target_name": "icmp-single-reply"}),
    ("probe_mtu_bytes", {"target_name": "mtu-no-resolve"}),
    ("probe_icmp_avg_rtt_seconds", {"target_name": "mtu-no-resolve"}),
]


def scrape():
    with urllib.request.urlopen(METRICS_URL, timeout=2) as response:
        return response.read().decode("utf-8")


def parse_metrics(text):
    metrics = []
    pattern = re.compile(r"^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{([^}]*)\})?\s+(-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)$")
    for raw_line in text.splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#"):
            continue
        match = pattern.match(line)
        if not match:
            continue
        name, raw_labels, raw_value = match.groups()
        metrics.append((name, parse_labels(raw_labels or ""), float(raw_value)))
    return metrics


def parse_labels(raw_labels):
    labels = {}
    if not raw_labels:
        return labels

    for match in re.finditer(r'([a-zA-Z_][a-zA-Z0-9_]*)="((?:\\.|[^"])*)"', raw_labels):
        value = match.group(2).replace(r'\\', '\x00').replace(r'\"', '"').replace(r'\n', '\n').replace('\x00', '\\')
        labels[match.group(1)] = value
    return labels


def metric_value(metrics, name, wanted_labels):
    for metric_name, labels, value in metrics:
        if metric_name != name:
            continue
        if all(labels.get(key) == wanted for key, wanted in wanted_labels.items()):
            return value
    return None


def check(metrics):
    failures = []
    for name, labels, want in EXPECTATIONS:
        got = metric_value(metrics, name, labels)
        if got is None:
            failures.append(f"{name}{labels} missing, want {want}")
            continue
        if got != want:
            failures.append(f"{name}{labels} = {got}, want {want}")

    for name, labels, min_want, max_want in RANGE_EXPECTATIONS:
        got = metric_value(metrics, name, labels)
        if got is None:
            failures.append(f"{name}{labels} missing, want range [{min_want}, {max_want}]")
            continue
        if got < min_want or got > max_want:
            failures.append(f"{name}{labels} = {got}, want range [{min_want}, {max_want}]")

    for name, labels, want in STATE_EXPECTATIONS:
        got = metric_value(metrics, name, labels)
        if got is None:
            failures.append(f"{name}{labels} missing, want {want}")
            continue
        if got != want:
            failures.append(f"{name}{labels} = {got}, want {want}")

    for name, labels in ABSENT_EXPECTATIONS:
        got = metric_value(metrics, name, labels)
        if got is not None:
            failures.append(f"{name}{labels} present, want absent")

    return failures


deadline = time.time() + 45
last_error = None

while time.time() < deadline:
    try:
        body = scrape()
        metrics = parse_metrics(body)
        failures = check(metrics)
        if not failures:
            print("e2e metrics assertions passed")
            sys.exit(0)
        last_error = "\n".join(failures)
    except Exception as exc:
        last_error = str(exc)
    time.sleep(1)

print("e2e metrics assertions failed:", file=sys.stderr)
print(last_error, file=sys.stderr)
sys.exit(1)
