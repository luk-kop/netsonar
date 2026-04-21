// Package metrics handles Prometheus metric registration and exposition.
package metrics

import (
	"bytes"
	"crypto/x509"
	"net/http"
	"strconv"
	"time"

	"netsonar/internal/config"
	"netsonar/internal/probe"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// commonLabels are the Prometheus labels applied to every probe metric.
// The first four ("target", "target_name", "probe_type", "network_path") are
// always present; "network_path" is automatically set to "proxy" when the
// target uses a proxy_url, "direct" otherwise. The rest are derived
// dynamically from the tag keys found in the configuration.
var baseLabels = []string{"target", "target_name", "probe_type", "network_path"}

// MetricsExporter registers Prometheus metric descriptors, records probe
// results, and serves the /metrics HTTP endpoint using a custom registry.
type MetricsExporter struct {
	registry *prometheus.Registry
	tagKeys  []string // dynamic tag keys from config

	// Probe metrics (common labels).
	probeSuccess       *prometheus.GaugeVec
	probeDuration      *prometheus.GaugeVec
	probePhaseDuration *prometheus.GaugeVec

	// HTTP-specific.
	httpStatusCode *prometheus.GaugeVec

	// TLS-specific.
	tlsCertExpiry      *prometheus.GaugeVec
	tlsCertChainExpiry *prometheus.GaugeVec

	// ICMP-specific.
	icmpPacketLoss *prometheus.GaugeVec
	icmpAvgRTT     *prometheus.GaugeVec
	icmpStddevRTT  *prometheus.GaugeVec

	// MTU-specific.
	mtuBytes *prometheus.GaugeVec
	mtuState *prometheus.GaugeVec

	// DNS-specific.
	dnsResolveTime *prometheus.GaugeVec
	dnsResultMatch *prometheus.GaugeVec

	// HTTP body-specific.
	httpBodyMatch         *prometheus.GaugeVec
	httpResponseTruncated *prometheus.GaugeVec

	// Scheduler.
	probeSkippedOverlapTotal *prometheus.CounterVec

	// Agent metadata.
	agentInfo           *prometheus.GaugeVec
	agentConfigInfo     *prometheus.GaugeVec
	agentTargetsTotal   prometheus.Gauge
	agentConfigReloadTS prometheus.Gauge
}

// NewMetricsExporter creates a MetricsExporter with all metric descriptors
// registered on a custom prometheus.Registry. The tagKeys parameter defines
// which tag keys from the configuration become Prometheus labels — this is
// derived dynamically from config.CollectTagKeys.
func NewMetricsExporter(tagKeys []string) *MetricsExporter {
	reg := prometheus.NewRegistry()

	// Build the full label set: base labels + dynamic tag keys.
	commonLabels := make([]string, 0, len(baseLabels)+len(tagKeys))
	commonLabels = append(commonLabels, baseLabels...)
	commonLabels = append(commonLabels, tagKeys...)

	m := &MetricsExporter{
		registry: reg,
		tagKeys:  tagKeys,

		probeSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_success",
			Help: "1 if the probe succeeded, 0 if it failed.",
		}, commonLabels),

		probeDuration: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_duration_seconds",
			Help: "Total probe duration in seconds.",
		}, commonLabels),

		probePhaseDuration: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_phase_duration_seconds",
			Help: "Per-phase timing for probes that expose sub-phase breakdowns (HTTP: dns_resolve, tcp_connect, tls_handshake, ttfb, transfer; proxy: proxy_dial, proxy_tls, proxy_connect).",
		}, append(commonLabels, "phase")),

		httpStatusCode: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_http_status_code",
			Help: "HTTP response status code.",
		}, commonLabels),

		tlsCertExpiry: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_tls_cert_expiry_timestamp_seconds",
			Help: "Unix timestamp in seconds of the earliest TLS peer certificate expiry.",
		}, commonLabels),

		tlsCertChainExpiry: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_tls_cert_chain_expiry_timestamp_seconds",
			Help: "Unix timestamp in seconds of each TLS peer certificate expiry.",
		}, append(commonLabels, "cert_index", "cert_role")),

		icmpPacketLoss: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_icmp_packet_loss_ratio",
			Help: "ICMP packet loss ratio (0.0–1.0).",
		}, commonLabels),

		icmpAvgRTT: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_icmp_avg_rtt_seconds",
			Help: "Average ICMP echo round-trip time in seconds.",
		}, commonLabels),

		icmpStddevRTT: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_icmp_stddev_rtt_seconds",
			Help: "Population standard deviation of ICMP echo round-trip time in seconds.",
		}, commonLabels),

		mtuBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_mtu_bytes",
			Help: "Largest confirmed MTU size from the MTU probe in bytes.",
		}, commonLabels),

		mtuState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_mtu_state",
			Help: "MTU probe state as an info metric with state and detail labels (value is always 1).",
		}, append(commonLabels, "state", "detail")),

		dnsResolveTime: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_dns_resolve_seconds",
			Help: "DNS resolution time in seconds.",
		}, commonLabels),

		dnsResultMatch: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_dns_result_match",
			Help: "1 if DNS result matches expected values, 0 otherwise.",
		}, commonLabels),

		httpBodyMatch: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_http_body_match",
			Help: "1 if response body matches configured pattern, 0 otherwise.",
		}, commonLabels),

		httpResponseTruncated: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_http_response_truncated",
			Help: "1 if the HTTP response body exceeded the effective transfer limit, 0 otherwise.",
		}, commonLabels),

		probeSkippedOverlapTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "probe_skipped_overlap_total",
			Help: "Total probe executions skipped because the previous probe for the same target was still running when the next tick fired.",
		}, commonLabels),

		agentInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "agent_info",
			Help: "Agent build information (always 1).",
		}, []string{"version"}),

		agentConfigInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "agent_config_info",
			Help: "Hash of the effective configuration currently in use (always 1).",
		}, []string{"hash"}),

		agentTargetsTotal: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "agent_targets_total",
			Help: "Total number of configured targets.",
		}),

		agentConfigReloadTS: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "agent_config_reload_timestamp_seconds",
			Help: "Unix timestamp of last configuration reload.",
		}),
	}

	// Register all collectors with the custom registry.
	reg.MustRegister(
		m.probeSuccess,
		m.probeDuration,
		m.probePhaseDuration,
		m.httpStatusCode,
		m.tlsCertExpiry,
		m.tlsCertChainExpiry,
		m.icmpPacketLoss,
		m.icmpAvgRTT,
		m.icmpStddevRTT,
		m.mtuBytes,
		m.mtuState,
		m.probeSkippedOverlapTotal,
		m.dnsResolveTime,
		m.dnsResultMatch,
		m.httpBodyMatch,
		m.httpResponseTruncated,
		m.agentInfo,
		m.agentConfigInfo,
		m.agentTargetsTotal,
		m.agentConfigReloadTS,
	)

	return m
}

// Registry returns the custom prometheus.Registry used by this exporter.
func (m *MetricsExporter) Registry() *prometheus.Registry {
	return m.registry
}

// Handler returns an http.Handler that serves the /metrics endpoint
// using the custom registry.
func (m *MetricsExporter) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// buildLabels constructs the common Prometheus label map from a target's
// Tags map, plus the target address and probe_type. Tag keys are determined
// dynamically from the exporter's tagKeys slice.
func (m *MetricsExporter) buildLabels(target config.TargetConfig) prometheus.Labels {
	networkPath := "direct"
	if target.ProbeOpts.ProxyURL != "" {
		networkPath = "proxy"
	}
	labels := prometheus.Labels{
		"target":       target.Address,
		"target_name":  target.Name,
		"probe_type":   string(target.ProbeType),
		"network_path": networkPath,
	}
	for _, key := range m.tagKeys {
		val := ""
		if target.Tags != nil {
			if v, ok := target.Tags[key]; ok {
				val = v
			}
		}
		labels[key] = val
	}
	return labels
}

// Record updates all relevant metrics for a probe result.
func (m *MetricsExporter) Record(target config.TargetConfig, result probe.ProbeResult) {
	labels := m.buildLabels(target)

	// Common metrics: success and duration.
	successVal := 0.0
	if result.Success {
		successVal = 1.0
	}
	m.probeSuccess.With(labels).Set(successVal)
	m.probeDuration.With(labels).Set(result.Duration.Seconds())

	// Current-observation semantics: delete all known phase series first,
	// then set only phases present in this result. Missing phases (e.g.
	// tls_handshake when the probe failed before TLS) disappear from
	// /metrics rather than retaining stale values from a previous run.
	for _, phase := range knownPhases {
		phaseLabels := cloneLabels(labels)
		phaseLabels["phase"] = phase
		m.probePhaseDuration.Delete(phaseLabels)
	}
	for phase, dur := range result.Phases {
		phaseLabels := cloneLabels(labels)
		phaseLabels["phase"] = phase
		m.probePhaseDuration.With(phaseLabels).Set(dur.Seconds())
	}

	// Probe-type-specific metrics.
	switch target.ProbeType {
	case config.ProbeTypeHTTP:
		if result.HTTPResponseReceived {
			m.httpStatusCode.With(labels).Set(float64(result.StatusCode))
		} else {
			m.httpStatusCode.Delete(labels)
		}
		m.recordTLSResult(result, labels)
		if result.HTTPTruncationEvaluated {
			truncatedVal := 0.0
			if result.HTTPResponseTruncated {
				truncatedVal = 1.0
			}
			m.httpResponseTruncated.With(labels).Set(truncatedVal)
		} else {
			m.httpResponseTruncated.Delete(labels)
		}

	case config.ProbeTypeICMP:
		m.icmpPacketLoss.With(labels).Set(result.PacketLoss)
		recordICMPAvgRTT(result, labels, m.icmpAvgRTT)
		recordICMPStddevRTT(result, labels, m.icmpStddevRTT)

	case config.ProbeTypeMTU:
		m.recordMTUResult(result, labels)
		recordICMPAvgRTT(result, labels, m.icmpAvgRTT)

	case config.ProbeTypeDNS:
		m.dnsResolveTime.With(labels).Set(result.DNSResolveTime.Seconds())
		if result.DNSMatchEvaluated {
			matchVal := 0.0
			if result.DNSMatched {
				matchVal = 1.0
			}
			m.dnsResultMatch.With(labels).Set(matchVal)
		} else {
			m.dnsResultMatch.Delete(labels)
		}

	case config.ProbeTypeTLSCert:
		m.recordTLSResult(result, labels)

	case config.ProbeTypeHTTPBody:
		if result.HTTPBodyEvaluated {
			bodyVal := 0.0
			if result.BodyMatch {
				bodyVal = 1.0
			}
			m.httpBodyMatch.With(labels).Set(bodyVal)
		} else {
			m.httpBodyMatch.Delete(labels)
		}
		if result.HTTPResponseReceived {
			m.httpStatusCode.With(labels).Set(float64(result.StatusCode))
		} else {
			m.httpStatusCode.Delete(labels)
		}

	case config.ProbeTypeTCP, config.ProbeTypeProxy:
		// No type-specific metrics; phases are handled generically above.
	}
}

func (m *MetricsExporter) recordTLSResult(result probe.ProbeResult, labels prometheus.Labels) {
	m.tlsCertChainExpiry.DeletePartialMatch(labels)

	if result.CertObserved {
		m.tlsCertExpiry.With(labels).Set(float64(result.CertExpiry.Unix()))
	} else {
		m.tlsCertExpiry.Delete(labels)
	}

	for i, cert := range result.TLSCertificates {
		if cert == nil {
			continue
		}
		certLabels := cloneLabels(labels)
		certLabels["cert_index"] = strconv.Itoa(i)
		certLabels["cert_role"] = certificateRole(i, cert)
		m.tlsCertChainExpiry.With(certLabels).Set(float64(cert.NotAfter.Unix()))
	}
}

func certificateRole(index int, cert *x509.Certificate) string {
	if index == 0 {
		return "leaf"
	}
	if bytes.Equal(cert.RawIssuer, cert.RawSubject) {
		return "root"
	}
	return "intermediate"
}

func (m *MetricsExporter) recordMTUResult(result probe.ProbeResult, labels prometheus.Labels) {
	m.mtuState.DeletePartialMatch(labels)

	if result.PathMTU > 0 {
		m.mtuBytes.With(labels).Set(float64(result.PathMTU))
	} else {
		m.mtuBytes.Delete(labels)
	}

	state, detail := mtuStateDetail(result)
	stateLabels := cloneLabels(labels)
	stateLabels["state"] = state
	stateLabels["detail"] = detail
	m.mtuState.With(stateLabels).Set(1)
}

func mtuStateDetail(result probe.ProbeResult) (string, string) {
	return result.MTUState, result.MTUDetail
}

func recordICMPAvgRTT(result probe.ProbeResult, labels prometheus.Labels, avgVec *prometheus.GaugeVec) {
	if result.ICMPRepliesObserved >= 1 {
		avgVec.With(labels).Set(result.ICMPAvgRTT.Seconds())
	} else {
		avgVec.Delete(labels)
	}
}

func recordICMPStddevRTT(result probe.ProbeResult, labels prometheus.Labels, stddevVec *prometheus.GaugeVec) {
	if result.ICMPRepliesObserved >= 2 {
		stddevVec.With(labels).Set(result.ICMPStddevRTT.Seconds())
	} else {
		stddevVec.Delete(labels)
	}
}

// knownPhases are all phase label values used by probe_phase_duration_seconds.
// Record and DeleteTarget use this list for bounded exact deletes so stale
// current-observation phase series do not survive later results. Sourced from
// probe.AllPhases so there is a single source of truth with the probers.
var knownPhases = probe.AllPhases

// DeleteTarget removes all metric series associated with a target. This
// must be called when a target is removed or changed during a config reload
// so that /metrics stops emitting stale series for targets that no longer exist.
func (m *MetricsExporter) DeleteTarget(target config.TargetConfig) {
	labels := m.buildLabels(target)

	m.probeSuccess.Delete(labels)
	m.probeDuration.Delete(labels)
	m.httpStatusCode.Delete(labels)
	m.tlsCertExpiry.Delete(labels)
	m.tlsCertChainExpiry.DeletePartialMatch(labels)
	m.icmpPacketLoss.Delete(labels)
	m.icmpAvgRTT.Delete(labels)
	m.icmpStddevRTT.Delete(labels)
	m.mtuBytes.Delete(labels)
	m.mtuState.DeletePartialMatch(labels)
	m.probeSkippedOverlapTotal.Delete(labels)
	m.dnsResolveTime.Delete(labels)
	m.dnsResultMatch.Delete(labels)
	m.httpBodyMatch.Delete(labels)
	m.httpResponseTruncated.Delete(labels)

	for _, phase := range knownPhases {
		phaseLabels := cloneLabels(labels)
		phaseLabels["phase"] = phase
		m.probePhaseDuration.Delete(phaseLabels)
	}
}

// EnsureTarget pre-initializes event-driven counters that should exist for
// active targets even before the first increment occurs.
func (m *MetricsExporter) EnsureTarget(target config.TargetConfig) {
	labels := m.buildLabels(target)
	m.probeSkippedOverlapTotal.With(labels)
}

// IncrSkippedOverlap increments the probe_skipped_overlap_total counter
// for the given target. This is called by the scheduler when a stale tick
// is drained after a probe that ran longer than its interval.
func (m *MetricsExporter) IncrSkippedOverlap(target config.TargetConfig) {
	labels := m.buildLabels(target)
	m.probeSkippedOverlapTotal.With(labels).Inc()
}

// SetAgentInfo sets the agent_info gauge with the given version string.
// Only binary-level information lives here; configuration identity is
// exposed separately via SetConfigInfo.
func (m *MetricsExporter) SetAgentInfo(version string) {
	m.agentInfo.With(prometheus.Labels{
		"version": version,
	}).Set(1)
}

// SetConfigInfo publishes the short hash of the effective configuration as
// a single-series gauge. It resets any previously published hash so that
// /metrics only ever exposes the currently active configuration.
func (m *MetricsExporter) SetConfigInfo(hash string) {
	m.agentConfigInfo.Reset()
	m.agentConfigInfo.With(prometheus.Labels{"hash": hash}).Set(1)
}

// SetTargetsTotal sets the agent_targets_total gauge.
func (m *MetricsExporter) SetTargetsTotal(count int) {
	m.agentTargetsTotal.Set(float64(count))
}

// SetConfigReloadTimestamp sets the agent_config_reload_timestamp_seconds gauge.
func (m *MetricsExporter) SetConfigReloadTimestamp(t time.Time) {
	m.agentConfigReloadTS.Set(float64(t.Unix()))
}

// cloneLabels returns a shallow copy of a prometheus.Labels map.
func cloneLabels(src prometheus.Labels) prometheus.Labels {
	dst := make(prometheus.Labels, len(src)+1)
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
