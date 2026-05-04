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

// jitterAllowance is the maximum scheduling overhead we tolerate above the
// configured timeout. This accounts for goroutine scheduling, GC pauses,
// and timer imprecision in the test environment.
const jitterAllowance = 200 * time.Millisecond

// timeoutScenario describes a generated probe scenario for timeout
// enforcement testing.
type timeoutScenario struct {
	ProbeType   config.ProbeType
	TimeoutMs   int
	Description string
}

// genTimeoutProbeType generates one of the supported probe types.
func genTimeoutProbeType() gopter.Gen {
	types := []config.ProbeType{
		config.ProbeTypeTCP,
		config.ProbeTypeHTTP,
		config.ProbeTypeICMP,
		config.ProbeTypeMTU,
		config.ProbeTypeDNS,
		config.ProbeTypeTLSCert,
		config.ProbeTypeHTTPBody,
		config.ProbeTypeProxyConnect,
	}
	return gen.IntRange(0, len(types)-1).Map(func(i int) config.ProbeType {
		return types[i]
	})
}

// genEnforcementTimeoutMs generates a timeout in milliseconds. We use short
// timeouts (50ms–500ms) to keep the test fast while still exercising the
// timeout enforcement path.
func genEnforcementTimeoutMs() gopter.Gen {
	return gen.IntRange(50, 500)
}

// genTimeoutScenario generates a random timeout enforcement scenario.
func genTimeoutScenario() gopter.Gen {
	return gopter.CombineGens(
		genTimeoutProbeType(),
		genEnforcementTimeoutMs(),
	).Map(func(vals []interface{}) timeoutScenario {
		pt := vals[0].(config.ProbeType)
		tms := vals[1].(int)
		return timeoutScenario{
			ProbeType:   pt,
			TimeoutMs:   tms,
			Description: fmt.Sprintf("type=%s timeout=%dms", pt, tms),
		}
	})
}

// buildTimeoutTarget constructs a TargetConfig for a timeout enforcement
// scenario. Addresses are chosen to be slow or unreachable so that the
// probe is likely to hit the timeout rather than completing instantly.
//
// For TCP: connect to a non-routable address (192.0.2.1 — TEST-NET-1)
// For HTTP/HTTPBody: connect to a non-routable address
// For ICMP/MTU: connect to a non-routable address (will fail with
//
//	permission error or timeout)
//
// For DNS: resolve a non-existent domain
// For TLSCert: connect to a non-routable address
// For Proxy: connect through a non-existent proxy
//
// We also test the "fast success" path using local test servers to verify
// that successful probes also report duration ≤ timeout + jitter.
func buildTimeoutTarget(
	sc timeoutScenario,
	useSlowTarget bool,
	httpURL, httpsAddr string,
) config.TargetConfig {
	timeout := time.Duration(sc.TimeoutMs) * time.Millisecond

	target := config.TargetConfig{
		Name:      "pbt-timeout-enforcement",
		ProbeType: sc.ProbeType,
		Timeout:   timeout,
		Interval:  timeout * 2,
	}

	if useSlowTarget {
		// Use addresses that will cause the probe to block until timeout.
		// 192.0.2.1 is TEST-NET-1 (RFC 5737), packets are typically dropped.
		switch sc.ProbeType {
		case config.ProbeTypeTCP:
			target.Address = "192.0.2.1:80"
		case config.ProbeTypeHTTP:
			target.Address = "http://192.0.2.1:80"
		case config.ProbeTypeHTTPBody:
			target.Address = "http://192.0.2.1:80"
			target.ProbeOpts.BodyMatchString = "ok"
		case config.ProbeTypeICMP:
			target.Address = "192.0.2.1"
			target.ProbeOpts.PingCount = 3
			target.ProbeOpts.PingIntervalSec = 0.1
		case config.ProbeTypeMTU:
			target.Address = "192.0.2.1"
			target.ProbeOpts.ICMPPayloadSizes = []int{1472, 1372, 1272}
		case config.ProbeTypeDNS:
			target.Address = "this.host.does.not.exist.invalid"
			target.ProbeOpts.DNSQueryName = "this.host.does.not.exist.invalid"
			target.ProbeOpts.DNSQueryType = "A"
		case config.ProbeTypeTLSCert:
			target.Address = "192.0.2.1:443"
			target.ProbeOpts.TLSSkipVerify = true
		case config.ProbeTypeProxyConnect:
			target.Address = "https://192.0.2.1"
			target.ProbeOpts.ProxyURL = "http://192.0.2.1:8888"
		}
	} else {
		// Use local test servers for the fast-success path.
		switch sc.ProbeType {
		case config.ProbeTypeTCP:
			// Extract host:port from the HTTP test server URL.
			target.Address = extractHostPort(httpURL)
		case config.ProbeTypeHTTP:
			target.Address = httpURL
		case config.ProbeTypeHTTPBody:
			target.Address = httpURL
			target.ProbeOpts.BodyMatchString = "ok"
		case config.ProbeTypeICMP:
			target.Address = "127.0.0.1"
			target.ProbeOpts.PingCount = 1
			target.ProbeOpts.PingIntervalSec = 0.1
		case config.ProbeTypeMTU:
			target.Address = "127.0.0.1"
			target.ProbeOpts.ICMPPayloadSizes = []int{1472, 1372, 1272}
		case config.ProbeTypeDNS:
			target.Address = "localhost"
			target.ProbeOpts.DNSQueryName = "localhost"
			target.ProbeOpts.DNSQueryType = "A"
		case config.ProbeTypeTLSCert:
			target.Address = httpsAddr
			target.ProbeOpts.TLSSkipVerify = true
		case config.ProbeTypeProxyConnect:
			target.Address = "https://example.com"
			target.ProbeOpts.ProxyURL = "http://127.0.0.1:19999"
		}
	}

	return target
}

// extractHostPort extracts the host:port from an http:// URL.
func extractHostPort(rawURL string) string {
	// Strip "http://" prefix to get host:port.
	const prefix = "http://"
	if len(rawURL) > len(prefix) {
		hp := rawURL[len(prefix):]
		if _, _, err := net.SplitHostPort(hp); err == nil {
			return hp
		}
	}
	return "127.0.0.1:1"
}

// TestPropertyProbeTimeoutEnforcement verifies Property 6:
// For all probe executions against any target, the ProbeResult Duration
// SHALL NOT exceed the target's configured timeout plus a small scheduling
// jitter allowance.
//
// The test exercises two paths per probe type:
//   - Slow/unreachable targets: the probe should be cancelled by the context
//     timeout and return within timeout + jitter.
//   - Fast/reachable targets: the probe completes quickly, and the duration
//     is trivially within the timeout bound.
//
// **Validates: Requirements 6.3, 15.2**
func TestPropertyProbeTimeoutEnforcement(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow property-based test in short mode")
	}
	// Start test servers for the fast-success path.
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

	httpsAddr := httpsServer.Listener.Addr().String()

	parameters := gopter.DefaultTestParameters()
	parameters.MinSuccessfulTests = 200
	parameters.MaxSize = 50
	properties := gopter.NewProperties(parameters)

	// Sub-property: slow/unreachable targets must respect timeout.
	properties.Property(
		"probe duration <= timeout + jitter for slow/unreachable targets",
		prop.ForAll(
			func(sc timeoutScenario) (bool, error) {
				timeout := time.Duration(sc.TimeoutMs) * time.Millisecond
				target := buildTimeoutTarget(sc, true, httpServer.URL, httpsAddr)
				prober := proberForType(sc.ProbeType)

				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				defer cancel()

				result := prober.Probe(ctx, target)

				maxAllowed := timeout + jitterAllowance
				if result.Duration > maxAllowed {
					return false, fmt.Errorf(
						"duration %s exceeds timeout %s + jitter %s = %s for scenario: %s",
						result.Duration, timeout, jitterAllowance, maxAllowed, sc.Description,
					)
				}

				return true, nil
			},
			genTimeoutScenario(),
		),
	)

	// Sub-property: fast/reachable targets also respect timeout.
	properties.Property(
		"probe duration <= timeout + jitter for fast/reachable targets",
		prop.ForAll(
			func(sc timeoutScenario) (bool, error) {
				timeout := time.Duration(sc.TimeoutMs) * time.Millisecond
				target := buildTimeoutTarget(sc, false, httpServer.URL, httpsAddr)
				prober := proberForType(sc.ProbeType)

				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				defer cancel()

				result := prober.Probe(ctx, target)

				maxAllowed := timeout + jitterAllowance
				if result.Duration > maxAllowed {
					return false, fmt.Errorf(
						"duration %s exceeds timeout %s + jitter %s = %s for scenario: %s",
						result.Duration, timeout, jitterAllowance, maxAllowed, sc.Description,
					)
				}

				return true, nil
			},
			genTimeoutScenario(),
		),
	)

	properties.TestingRun(t)
}
