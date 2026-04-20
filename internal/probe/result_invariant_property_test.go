package probe

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"netsonar/internal/config"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
)

// probeScenario describes a generated probe scenario used to exercise
// the success-implies-empty-error invariant across all probe types.
type probeScenario struct {
	ProbeType   config.ProbeType
	Address     string
	TimeoutMs   int
	Description string

	// Probe-type-specific options.
	PingCount        int
	PingIntervalSec  float64
	ICMPPayloadSizes []int
	DNSQueryName     string
	DNSQueryType     string
	ProxyURL         string
	BodyMatchString  string
}

// genProbeType generates one of the supported probe types.
func genProbeType() gopter.Gen {
	types := []config.ProbeType{
		config.ProbeTypeTCP,
		config.ProbeTypeHTTP,
		config.ProbeTypeICMP,
		config.ProbeTypeMTU,
		config.ProbeTypeDNS,
		config.ProbeTypeTLSCert,
		config.ProbeTypeHTTPBody,
		config.ProbeTypeProxy,
	}
	return gen.IntRange(0, len(types)-1).Map(func(i int) config.ProbeType {
		return types[i]
	})
}

// genProbeAddress generates addresses that exercise different code paths
// depending on the probe type. Includes reachable (localhost), unreachable,
// and invalid addresses.
func genProbeAddress() gopter.Gen {
	addresses := []string{
		"127.0.0.1",
		"localhost",
		"this.host.does.not.exist.invalid",
		"192.0.2.1", // TEST-NET-1, unlikely to respond
		"0.0.0.0",
	}
	return gen.IntRange(0, len(addresses)-1).Map(func(i int) string {
		return addresses[i]
	})
}

// genProbeTimeoutMs generates a timeout in milliseconds (50ms–1500ms).
func genProbeTimeoutMs() gopter.Gen {
	return gen.IntRange(50, 1500)
}

// genProbeScenario generates a random probe scenario by combining probe
// type, address, and timeout generators with type-specific options.
func genProbeScenario() gopter.Gen {
	return gopter.CombineGens(
		genProbeType(),
		genProbeAddress(),
		genProbeTimeoutMs(),
		gen.IntRange(1, 5),         // ping count
		gen.Float64Range(0.1, 1.0), // ping interval
	).Map(func(vals []interface{}) probeScenario {
		pt := vals[0].(config.ProbeType)
		addr := vals[1].(string)
		tms := vals[2].(int)
		pc := vals[3].(int)
		pi := vals[4].(float64)

		return probeScenario{
			ProbeType:        pt,
			Address:          addr,
			TimeoutMs:        tms,
			PingCount:        pc,
			PingIntervalSec:  pi,
			ICMPPayloadSizes: []int{1472, 1372, 1272},
			DNSQueryName:     addr,
			DNSQueryType:     "A",
			ProxyURL:         "http://127.0.0.1:19999", // non-existent proxy
			BodyMatchString:  "ok",
			Description: fmt.Sprintf("type=%s addr=%s timeout=%dms",
				pt, addr, tms),
		}
	})
}

// buildTarget constructs a TargetConfig from a probeScenario, adapting the
// address format to what each probe type expects.
func buildTarget(sc probeScenario, httpURL, httpsURL string) config.TargetConfig {
	timeout := time.Duration(sc.TimeoutMs) * time.Millisecond

	target := config.TargetConfig{
		Name:      "pbt-result-invariant",
		Address:   sc.Address,
		ProbeType: sc.ProbeType,
		Timeout:   timeout,
		Interval:  timeout * 2,
	}

	switch sc.ProbeType {
	case config.ProbeTypeTCP:
		// TCP needs host:port. Use a port that is likely closed.
		if _, _, err := net.SplitHostPort(sc.Address); err != nil {
			target.Address = net.JoinHostPort(sc.Address, "1")
		}

	case config.ProbeTypeHTTP:
		// Use the test HTTP server URL for localhost, otherwise a URL
		// that will fail (exercising the error path).
		if sc.Address == "127.0.0.1" || sc.Address == "localhost" {
			target.Address = httpURL
		} else {
			target.Address = fmt.Sprintf("http://%s:1", sc.Address)
		}

	case config.ProbeTypeHTTPBody:
		if sc.Address == "127.0.0.1" || sc.Address == "localhost" {
			target.Address = httpURL
		} else {
			target.Address = fmt.Sprintf("http://%s:1", sc.Address)
		}
		target.ProbeOpts.BodyMatchString = sc.BodyMatchString

	case config.ProbeTypeICMP:
		target.ProbeOpts.PingCount = sc.PingCount
		target.ProbeOpts.PingIntervalSec = sc.PingIntervalSec

	case config.ProbeTypeMTU:
		target.ProbeOpts.ICMPPayloadSizes = sc.ICMPPayloadSizes

	case config.ProbeTypeDNS:
		target.ProbeOpts.DNSQueryName = sc.DNSQueryName
		target.ProbeOpts.DNSQueryType = sc.DNSQueryType

	case config.ProbeTypeTLSCert:
		// TLS needs host:port. Use the HTTPS test server for localhost.
		if sc.Address == "127.0.0.1" || sc.Address == "localhost" {
			target.Address = httpsURL
		} else {
			if _, _, err := net.SplitHostPort(sc.Address); err != nil {
				target.Address = net.JoinHostPort(sc.Address, "443")
			}
		}
		target.ProbeOpts.TLSSkipVerify = true

	case config.ProbeTypeProxy:
		target.ProbeOpts.ProxyURL = sc.ProxyURL
		if sc.Address == "127.0.0.1" || sc.Address == "localhost" {
			target.Address = "https://example.com"
		} else {
			target.Address = fmt.Sprintf("https://%s", sc.Address)
		}
	}

	return target
}

// proberForType returns the appropriate Prober implementation for the given
// probe type. HTTP-based probers are configured with TLS skip-verify and
// no-follow-redirects for test isolation.
func proberForType(pt config.ProbeType) Prober {
	switch pt {
	case config.ProbeTypeTCP:
		return &TCPProber{}
	case config.ProbeTypeHTTP:
		return NewHTTPProber(true, false, "")
	case config.ProbeTypeICMP:
		return &ICMPProber{}
	case config.ProbeTypeMTU:
		return &MTUProber{}
	case config.ProbeTypeDNS:
		return &DNSProber{}
	case config.ProbeTypeTLSCert:
		return &TLSCertProber{}
	case config.ProbeTypeHTTPBody:
		return NewHTTPBodyProber(true, false, "", "")
	case config.ProbeTypeProxy:
		return &ProxyProber{}
	default:
		return &TCPProber{}
	}
}

// TestPropertySuccessImpliesEmptyError verifies Property 7:
// For all probe executions, if the ProbeResult Success field is true then
// the Error field SHALL be empty.
//
// The test exercises every probe type with varying generated inputs
// (addresses, timeouts) and verifies the invariant holds in all cases.
// It also checks the converse: when Error is non-empty, Success must be
// false.
//
// **Validates: Requirement 15.1**
func TestPropertySuccessImpliesEmptyError(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow property-based test in short mode")
	}
	// Start a minimal HTTP/HTTPS test server so that HTTP, HTTPBody, and
	// TLSCert probes have a reachable target for the success path.
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer httpServer.Close()

	httpsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer httpsServer.Close()

	// Extract host:port from the HTTPS server URL for TLS cert probes.
	httpsAddr := httpsServer.Listener.Addr().String()

	parameters := gopter.DefaultTestParameters()
	parameters.MinSuccessfulTests = 300
	parameters.MaxSize = 50
	properties := gopter.NewProperties(parameters)

	properties.Property("Success == true implies Error == empty string", prop.ForAll(
		func(sc probeScenario) (bool, error) {
			target := buildTarget(sc, httpServer.URL, httpsAddr)
			prober := proberForType(sc.ProbeType)

			timeout := time.Duration(sc.TimeoutMs) * time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()

			result := prober.Probe(ctx, target)

			// --- Property 7a: Success implies empty Error ---
			if result.Success && result.Error != "" {
				return false, fmt.Errorf(
					"Success=true but Error=%q for scenario: %s",
					result.Error, sc.Description)
			}

			// --- Property 7b (converse): non-empty Error implies !Success ---
			if result.Error != "" && result.Success {
				return false, fmt.Errorf(
					"Error=%q but Success=true for scenario: %s",
					result.Error, sc.Description)
			}

			return true, nil
		},
		genProbeScenario(),
	))

	properties.TestingRun(t)
}
