// Package metrics handles Prometheus metric registration and exposition.
package metrics

import (
	"net/http"
	"strings"
	"time"

	"netsonar/internal/config"
	"netsonar/internal/probe"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// commonLabels are the Prometheus labels applied to every probe metric.
// The first four ("target", "target_name", "probe_type", "proxied") are
// always present; "proxied" is automatically set to "true" when the target
// uses a proxy_url, "false" otherwise. The rest are derived dynamically
// from the tag keys found in the configuration.
var baseLabels = []string{"target", "target_name", "probe_type", "proxied"}

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
	tlsCertExpiry *prometheus.GaugeVec

	// ICMP-specific.
	icmpPacketLoss *prometheus.GaugeVec
	icmpAvgRTT     *prometheus.GaugeVec
	icmpHopCount   *prometheus.GaugeVec

	// MTU-specific.
	mtuPathMTU          *prometheus.GaugeVec
	mtuBytes            *prometheus.GaugeVec
	mtuState            *prometheus.GaugeVec
	mtuFragNeededTotal  *prometheus.CounterVec
	mtuTimeoutsTotal    *prometheus.CounterVec
	mtuRetriesTotal     *prometheus.CounterVec
	mtuLocalErrorsTotal *prometheus.CounterVec

	// DNS-specific.
	dnsResolveTime *prometheus.GaugeVec
	dnsResultMatch *prometheus.GaugeVec

	// HTTP body-specific.
	httpBodyMatch *prometheus.GaugeVec

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
			Name: "probe_tls_cert_expiry_timestamp",
			Help: "Unix timestamp of TLS certificate expiry.",
		}, commonLabels),

		icmpPacketLoss: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_icmp_packet_loss_ratio",
			Help: "ICMP packet loss ratio (0.0–1.0).",
		}, commonLabels),

		icmpAvgRTT: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_icmp_avg_rtt_seconds",
			Help: "Average ICMP echo round-trip time in seconds.",
		}, commonLabels),

		icmpHopCount: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_icmp_hop_count",
			Help: "TTL / hop count from ICMP echo reply.",
		}, commonLabels),

		mtuPathMTU: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_mtu_path_bytes",
			Help: "Detected path MTU in bytes (-1 if all sizes failed).",
		}, commonLabels),

		mtuBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_mtu_bytes",
			Help: "Largest confirmed MTU size from the MTU probe in bytes.",
		}, commonLabels),

		mtuState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_mtu_state",
			Help: "MTU probe state as an info metric with state and detail labels (value is always 1).",
		}, append(commonLabels, "state", "detail")),

		mtuFragNeededTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "probe_mtu_frag_needed_total",
			Help: "Total ICMP fragmentation-needed responses matched by MTU probes.",
		}, commonLabels),

		mtuTimeoutsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "probe_mtu_timeouts_total",
			Help: "Total MTU probe attempts that timed out.",
		}, commonLabels),

		mtuRetriesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "probe_mtu_retries_total",
			Help: "Total additional MTU probe attempts performed after the first attempt in a retry group.",
		}, commonLabels),

		mtuLocalErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "probe_mtu_local_errors_total",
			Help: "Total local host/kernel send errors observed by MTU probes, such as EMSGSIZE.",
		}, commonLabels),

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
			Name: "agent_config_reload_timestamp",
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
		m.icmpPacketLoss,
		m.icmpAvgRTT,
		m.icmpHopCount,
		m.mtuPathMTU,
		m.mtuBytes,
		m.mtuState,
		m.mtuFragNeededTotal,
		m.mtuTimeoutsTotal,
		m.mtuRetriesTotal,
		m.mtuLocalErrorsTotal,
		m.probeSkippedOverlapTotal,
		m.dnsResolveTime,
		m.dnsResultMatch,
		m.httpBodyMatch,
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
	proxied := "false"
	if target.ProbeOpts.ProxyURL != "" {
		proxied = "true"
	}
	labels := prometheus.Labels{
		"target":      target.Address,
		"target_name": target.Name,
		"probe_type":  string(target.ProbeType),
		"proxied":     proxied,
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

	for phase, dur := range result.Phases {
		phaseLabels := cloneLabels(labels)
		phaseLabels["phase"] = phase
		m.probePhaseDuration.With(phaseLabels).Set(dur.Seconds())
	}

	// Probe-type-specific metrics.
	switch target.ProbeType {
	case config.ProbeTypeHTTP:
		m.httpStatusCode.With(labels).Set(float64(result.StatusCode))
		if !result.CertExpiry.IsZero() {
			m.tlsCertExpiry.With(labels).Set(float64(result.CertExpiry.Unix()))
		}

	case config.ProbeTypeICMP:
		m.icmpPacketLoss.With(labels).Set(result.PacketLoss)
		m.icmpAvgRTT.With(labels).Set(result.ICMPAvgRTT.Seconds())
		m.icmpHopCount.With(labels).Set(float64(result.HopCount))

	case config.ProbeTypeMTU:
		m.mtuPathMTU.With(labels).Set(float64(result.PathMTU))
		m.recordMTUResult(target, result, labels)
		m.mtuFragNeededTotal.With(labels).Add(float64(result.MTUFragNeededCount))
		m.mtuTimeoutsTotal.With(labels).Add(float64(result.MTUTimeoutCount))
		m.mtuRetriesTotal.With(labels).Add(float64(result.MTURetryCount))
		m.mtuLocalErrorsTotal.With(labels).Add(float64(result.MTULocalErrorCount))

	case config.ProbeTypeDNS:
		m.dnsResolveTime.With(labels).Set(result.DNSResolveTime.Seconds())
		if len(target.ProbeOpts.DNSExpectedResults) > 0 {
			matchVal := 0.0
			if result.Success {
				matchVal = 1.0
			}
			m.dnsResultMatch.With(labels).Set(matchVal)
		}

	case config.ProbeTypeTLSCert:
		if !result.CertExpiry.IsZero() {
			m.tlsCertExpiry.With(labels).Set(float64(result.CertExpiry.Unix()))
		}

	case config.ProbeTypeHTTPBody:
		bodyVal := 0.0
		if result.BodyMatch {
			bodyVal = 1.0
		}
		m.httpBodyMatch.With(labels).Set(bodyVal)
		m.httpStatusCode.With(labels).Set(float64(result.StatusCode))

	case config.ProbeTypeTCP, config.ProbeTypeProxy:
		// No type-specific metrics; phases are handled generically above.
	}
}

func (m *MetricsExporter) recordMTUResult(target config.TargetConfig, result probe.ProbeResult, labels prometheus.Labels) {
	m.mtuState.DeletePartialMatch(labels)

	if result.PathMTU > 0 {
		m.mtuBytes.With(labels).Set(float64(result.PathMTU))
	} else {
		m.mtuBytes.Delete(labels)
	}

	state, detail := mtuStateDetail(target, result)
	stateLabels := cloneLabels(labels)
	stateLabels["state"] = state
	stateLabels["detail"] = detail
	m.mtuState.With(stateLabels).Set(1)
}

func mtuStateDetail(target config.TargetConfig, result probe.ProbeResult) (string, string) {
	if result.MTUState != "" && result.MTUDetail != "" {
		return result.MTUState, result.MTUDetail
	}
	return legacyMTUStateDetail(target, result)
}

func legacyMTUStateDetail(target config.TargetConfig, result probe.ProbeResult) (string, string) {
	if result.Success && result.PathMTU > 0 {
		state := probe.MTUStateOK
		if target.ProbeOpts.ExpectedMinMTU > 0 && result.PathMTU < target.ProbeOpts.ExpectedMinMTU {
			state = probe.MTUStateDegraded
		}
		return state, probe.MTUDetailLargestSizeConfirmed
	}

	switch {
	case strings.Contains(result.Error, "permission denied"):
		return probe.MTUStateError, probe.MTUDetailPermissionDenied
	case strings.Contains(result.Error, "resolve IPv4 address"):
		return probe.MTUStateError, probe.MTUDetailResolveError
	case strings.Contains(result.Error, "all MTU sizes failed"), strings.Contains(result.Error, "all sizes failed"):
		return probe.MTUStateDegraded, probe.MTUDetailAllSizesTimedOut
	case strings.Contains(result.Error, "context cancelled"):
		return probe.MTUStateError, probe.MTUDetailInternalError
	default:
		return probe.MTUStateError, probe.MTUDetailInternalError
	}
}

// knownPhases are the phase label values used by probe_phase_duration_seconds.
var knownPhases = []string{
	"dns_resolve",
	"tcp_connect",
	"tls_handshake",
	"ttfb",
	"transfer",
	"proxy_dial",
	"proxy_tls",
	"proxy_connect",
}

// DeleteTarget removes all metric series associated with a target. This
// must be called when a target is removed or changed during a config reload
// so that /metrics stops emitting stale series for targets that no longer exist.
func (m *MetricsExporter) DeleteTarget(target config.TargetConfig) {
	labels := m.buildLabels(target)

	m.probeSuccess.Delete(labels)
	m.probeDuration.Delete(labels)
	m.httpStatusCode.Delete(labels)
	m.tlsCertExpiry.Delete(labels)
	m.icmpPacketLoss.Delete(labels)
	m.icmpAvgRTT.Delete(labels)
	m.icmpHopCount.Delete(labels)
	m.mtuPathMTU.Delete(labels)
	m.mtuBytes.Delete(labels)
	m.mtuState.DeletePartialMatch(labels)
	m.mtuFragNeededTotal.Delete(labels)
	m.mtuTimeoutsTotal.Delete(labels)
	m.mtuRetriesTotal.Delete(labels)
	m.mtuLocalErrorsTotal.Delete(labels)
	m.probeSkippedOverlapTotal.Delete(labels)
	m.dnsResolveTime.Delete(labels)
	m.dnsResultMatch.Delete(labels)
	m.httpBodyMatch.Delete(labels)

	for _, phase := range knownPhases {
		phaseLabels := cloneLabels(labels)
		phaseLabels["phase"] = phase
		m.probePhaseDuration.Delete(phaseLabels)
	}
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

// SetConfigReloadTimestamp sets the agent_config_reload_timestamp gauge.
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
