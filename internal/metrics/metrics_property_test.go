package metrics

import (
	"reflect"
	"testing"
	"time"

	"netsonar/internal/config"
	"netsonar/internal/probe"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	dto "github.com/prometheus/client_model/go"
)

// genAlphaString generates a non-empty alphanumeric string of length 1–maxLen.
func genAlphaString(maxLen int) gopter.Gen {
	return gen.IntRange(1, maxLen).FlatMap(func(v interface{}) gopter.Gen {
		n := v.(int)
		return gen.SliceOfN(n, gen.AlphaChar()).Map(func(chars []rune) string {
			return string(chars)
		})
	}, reflect.TypeOf(""))
}

// genProbeType picks a random valid ProbeType.
func genProbeType() gopter.Gen {
	probeTypes := []config.ProbeType{
		config.ProbeTypeTCP, config.ProbeTypeHTTP, config.ProbeTypeICMP,
		config.ProbeTypeMTU, config.ProbeTypeDNS, config.ProbeTypeTLSCert,
		config.ProbeTypeHTTPBody, config.ProbeTypeProxyConnect,
	}
	return gen.IntRange(0, len(probeTypes)-1).Map(func(i int) config.ProbeType {
		return probeTypes[i]
	})
}

// genTagValue generates either an empty string (simulating a missing tag) or
// a short alphanumeric value.
func genTagValue() gopter.Gen {
	return gen.Weighted([]gen.WeightedGen{
		{Weight: 3, Gen: genAlphaString(12)},
		{Weight: 1, Gen: gen.Const("")},
	})
}

// targetWithTags holds a generated target config together with the expected
// label map for verification.
type targetWithTags struct {
	Target         config.TargetConfig
	ExpectedLabels map[string]string
}

// genTargetWithTags generates a TargetConfig with random tag values for all 8
// tag keys, plus a random address and probe type. It also computes the
// expected Prometheus label map so the property can verify it.
func genTargetWithTags() gopter.Gen {
	return gopter.CombineGens(
		genAlphaString(10), // address host
		genProbeType(),     // probe type
		genTagValue(),      // service
		genTagValue(),      // scope
		genTagValue(),      // provider
		genTagValue(),      // target_region
		genTagValue(),      // target_partition
		genTagValue(),      // visibility
		genTagValue(),      // port
		genTagValue(),      // impact
		gen.Bool(),         // has proxy URL
	).Map(func(vals []interface{}) targetWithTags {
		address := vals[0].(string) + ".example.com:443"
		pt := vals[1].(config.ProbeType)
		hasProxy := vals[10].(bool)

		tagValues := make([]string, 8)
		for i := 0; i < 8; i++ {
			tagValues[i] = vals[i+2].(string)
		}

		tagKeyNames := []string{
			"service", "scope", "provider", "target_region",
			"target_partition", "visibility", "port", "impact",
		}

		// Build the Tags map, only including non-empty values (to simulate
		// sparse tag maps that the real config would have).
		tags := make(map[string]string)
		for i, key := range tagKeyNames {
			if tagValues[i] != "" {
				tags[key] = tagValues[i]
			}
		}

		// Build the expected label map: target + target_name + probe_type + network_path + all 8 tag keys.
		// Missing tags should default to "".
		networkPath := "direct"
		if hasProxy {
			networkPath = "proxy"
		}
		expected := map[string]string{
			"target":       address,
			"target_name":  "prop-test",
			"probe_type":   string(pt),
			"network_path": networkPath,
		}
		for i, key := range tagKeyNames {
			expected[key] = tagValues[i]
		}

		target := config.TargetConfig{
			Name:      "prop-test",
			Address:   address,
			ProbeType: pt,
			Interval:  30 * time.Second,
			Timeout:   5 * time.Second,
			Tags:      tags,
		}
		if hasProxy {
			target.ProbeOpts.ProxyURL = "http://proxy.example.com:3128"
		}

		return targetWithTags{Target: target, ExpectedLabels: expected}
	})
}

// metricLabelsMap converts a dto.Metric's label pairs into a map.
func metricLabelsMap(m *dto.Metric) map[string]string {
	labels := make(map[string]string, len(m.GetLabel()))
	for _, lp := range m.GetLabel() {
		labels[lp.GetName()] = lp.GetValue()
	}
	return labels
}

// probeResultForType returns a minimal valid ProbeResult that will cause
// Record() to emit metrics for the given probe type.
func probeResultForType(pt config.ProbeType) probe.ProbeResult {
	base := probe.ProbeResult{
		Success:  true,
		Duration: 42 * time.Millisecond,
	}
	switch pt {
	case config.ProbeTypeHTTP:
		base.StatusCode = 200
		base.HTTPResponseReceived = true
		base.Phases = map[string]time.Duration{
			probe.PhaseDNSResolve:   5 * time.Millisecond,
			probe.PhaseTCPConnect:   10 * time.Millisecond,
			probe.PhaseTLSHandshake: 12 * time.Millisecond,
			probe.PhaseTTFB:         10 * time.Millisecond,
			probe.PhaseTransfer:     5 * time.Millisecond,
		}
	case config.ProbeTypeICMP:
		base.PacketLoss = 0.0
		base.ICMPRepliesObserved = 1
		base.ICMPAvgRTT = 5 * time.Millisecond
	case config.ProbeTypeMTU:
		base.PathMTU = 1500
	case config.ProbeTypeDNS:
		base.DNSResolveTime = 8 * time.Millisecond
	case config.ProbeTypeHTTPBody:
		base.StatusCode = 200
		base.HTTPResponseReceived = true
		base.HTTPBodyEvaluated = true
		base.BodyMatch = true
	case config.ProbeTypeTLSCert:
		base.CertObserved = true
		base.CertExpiry = time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return base
}

// TestPropertyMetricsLabelsMatchTargetTags verifies Property 12:
// For all valid targets with arbitrary tag combinations, the Prometheus
// labels on recorded metrics must contain exactly the target's address as
// "target", the probe type as "probe_type", and each of the 8 tag keys
// mapped from the target's Tags map (missing tags default to "").
func TestPropertyMetricsLabelsMatchTargetTags(t *testing.T) {
	parameters := gopter.DefaultTestParameters()
	parameters.MinSuccessfulTests = 200
	properties := gopter.NewProperties(parameters)

	properties.Property("recorded metric labels match target tags", prop.ForAll(
		func(tw targetWithTags) bool {
			m := NewMetricsExporter(testTagKeys, ExporterOptions{})
			result := probeResultForType(tw.Target.ProbeType)
			m.Record(tw.Target, result)

			families, err := m.registry.Gather()
			if err != nil {
				t.Logf("Gather error: %v", err)
				return false
			}

			// Find probe_success — it is always emitted for every probe type.
			var successFamily *dto.MetricFamily
			for _, f := range families {
				if f.GetName() == "probe_success" {
					successFamily = f
					break
				}
			}
			if successFamily == nil {
				t.Log("probe_success metric family not found")
				return false
			}
			if len(successFamily.GetMetric()) == 0 {
				t.Log("probe_success has no time series")
				return false
			}

			actual := metricLabelsMap(successFamily.GetMetric()[0])

			// Verify every expected label is present with the correct value.
			for key, want := range tw.ExpectedLabels {
				got, ok := actual[key]
				if !ok {
					t.Logf("label %q missing from metric", key)
					return false
				}
				if got != want {
					t.Logf("label %q: got %q, want %q", key, got, want)
					return false
				}
			}

			// Verify no unexpected labels (beyond the expected set).
			for key := range actual {
				if _, ok := tw.ExpectedLabels[key]; !ok {
					t.Logf("unexpected label %q in metric", key)
					return false
				}
			}

			return true
		},
		genTargetWithTags(),
	))

	properties.TestingRun(t)
}
