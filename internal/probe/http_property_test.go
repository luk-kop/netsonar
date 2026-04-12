package probe

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"netsonar/internal/config"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
)

// requiredPhaseKeys lists the five phase keys that every successful HTTP probe
// result must contain (Property 8 — phase breakdown completeness).
var requiredPhaseKeys = []string{
	"dns_resolve",
	"tcp_connect",
	"tls_handshake",
	"ttfb",
	"transfer",
}

// httpServerScenario describes a generated HTTP server behaviour used to
// exercise the HTTPProber under varying conditions.
type httpServerScenario struct {
	StatusCode  int
	BodySize    int           // response body size in bytes
	ServerDelay time.Duration // artificial delay before responding
	UseTLS      bool
	Description string
}

// genHTTPStatusCode generates a random HTTP status code from a realistic set.
func genHTTPStatusCode() gopter.Gen {
	codes := []int{
		http.StatusOK,
		http.StatusCreated,
		http.StatusNoContent,
		http.StatusMovedPermanently,
		http.StatusNotModified,
		http.StatusBadRequest,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	}
	return gen.IntRange(0, len(codes)-1).Map(func(i int) int {
		return codes[i]
	})
}

// genBodySize generates a response body size between 0 and 8192 bytes.
func genBodySize() gopter.Gen {
	return gen.IntRange(0, 8192)
}

// genServerDelay generates a small artificial server delay (0–20ms).
func genServerDelay() gopter.Gen {
	return gen.IntRange(0, 20).Map(func(ms int) time.Duration {
		return time.Duration(ms) * time.Millisecond
	})
}

// genHTTPServerScenario generates a random HTTP server scenario.
func genHTTPServerScenario() gopter.Gen {
	return gopter.CombineGens(
		genHTTPStatusCode(),
		genBodySize(),
		genServerDelay(),
		gen.Bool(), // useTLS
	).Map(func(vals []interface{}) httpServerScenario {
		return httpServerScenario{
			StatusCode:  vals[0].(int),
			BodySize:    vals[1].(int),
			ServerDelay: vals[2].(time.Duration),
			UseTLS:      vals[3].(bool),
			Description: fmt.Sprintf("status=%d body=%dB delay=%v tls=%v",
				vals[0].(int), vals[1].(int), vals[2].(time.Duration), vals[3].(bool)),
		}
	})
}

// startServer creates an httptest server (plain or TLS) with the given scenario.
// Returns the server and a cleanup function.
func startServer(sc httpServerScenario) *httptest.Server {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if sc.ServerDelay > 0 {
			time.Sleep(sc.ServerDelay)
		}
		w.WriteHeader(sc.StatusCode)
		if sc.BodySize > 0 {
			body := make([]byte, sc.BodySize)
			// Fill with deterministic content to avoid zero-page optimisations.
			rng := rand.New(rand.NewSource(42))
			for i := range body {
				body[i] = byte(rng.Intn(256))
			}
			_, _ = w.Write(body)
		}
	})

	if sc.UseTLS {
		return httptest.NewTLSServer(handler)
	}
	return httptest.NewServer(handler)
}

// TestPropertyHTTPPhaseBreakdownCompleteness verifies Property 8:
// For all successful HTTP probe executions, the result.Phases map contains
// exactly the five required phase keys (dns_resolve, tcp_connect,
// tls_handshake, ttfb, transfer), all durations are non-negative, and the
// sum of phase durations approximates the total duration within a tolerance.
//
// Validates: Requirement 7.1, 7.2
func TestPropertyHTTPPhaseBreakdownCompleteness(t *testing.T) {
	parameters := gopter.DefaultTestParameters()
	parameters.MinSuccessfulTests = 100
	parameters.MaxSize = 50
	properties := gopter.NewProperties(parameters)

	properties.Property("HTTP probe phases are complete and sum ≈ total duration", prop.ForAll(
		func(sc httpServerScenario) (bool, error) {
			srv := startServer(sc)
			defer srv.Close()

			target := config.TargetConfig{
				Name:      "pbt-http-phases",
				Address:   srv.URL,
				ProbeType: config.ProbeTypeHTTP,
				Timeout:   10 * time.Second,
				ProbeOpts: config.ProbeOptions{
					TLSSkipVerify: true,
				},
			}

			ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
			defer cancel()

			prober := NewHTTPProber(sc.UseTLS, true, "")
			result := prober.Probe(ctx, target)

			// We only check phase properties on successful probes where
			// a response was received (Phases map is populated).
			if !result.Success {
				// Probe failed — skip this case (not a property violation).
				return true, nil
			}

			// --- Property 8a: Phase map completeness ---
			if result.Phases == nil {
				return false, fmt.Errorf("Phases map is nil for scenario: %s", sc.Description)
			}

			for _, key := range requiredPhaseKeys {
				dur, ok := result.Phases[key]
				if !ok {
					return false, fmt.Errorf("missing phase key %q for scenario: %s", key, sc.Description)
				}
				if dur < 0 {
					return false, fmt.Errorf("phase %q has negative duration %v for scenario: %s", key, dur, sc.Description)
				}
			}

			// Verify no unexpected extra keys.
			if len(result.Phases) != len(requiredPhaseKeys) {
				extra := []string{}
				for k := range result.Phases {
					found := false
					for _, rk := range requiredPhaseKeys {
						if k == rk {
							found = true
							break
						}
					}
					if !found {
						extra = append(extra, k)
					}
				}
				return false, fmt.Errorf("unexpected phase keys %v for scenario: %s", extra, sc.Description)
			}

			// --- Property 8b: Sum of phases ≈ total duration ---
			//
			// For HTTPS targets, the httptrace-based ttfb is measured from
			// connectEnd to gotFirstByte, which *includes* the TLS handshake
			// duration. To compute a non-overlapping sum we subtract the
			// tls_handshake from ttfb for HTTPS connections, giving us the
			// "server think time" portion of TTFB that doesn't overlap with
			// the TLS phase.
			//
			// Non-overlapping phases:
			//   dns_resolve + tcp_connect + tls_handshake + (ttfb - tls_handshake) + transfer
			// = dns_resolve + tcp_connect + ttfb + transfer
			//
			// For plain HTTP, tls_handshake is zero so the formula is the same.
			phaseSum := result.Phases["dns_resolve"] +
				result.Phases["tcp_connect"] +
				result.Phases["ttfb"] +
				result.Phases["transfer"]

			totalDuration := result.Duration

			// The tolerance accounts for:
			// - Timer precision and scheduling jitter
			// - Gaps between httptrace callbacks (e.g. between DNS done
			//   and connect start)
			// - Go runtime overhead between phase measurements
			//
			// We use 5ms as a tolerance for local httptest servers.
			const tolerance = 5 * time.Millisecond

			diff := totalDuration - phaseSum
			if diff < 0 {
				diff = -diff
			}

			if diff > tolerance {
				var details strings.Builder
				for _, key := range requiredPhaseKeys {
					fmt.Fprintf(&details, "  %s: %v\n", key, result.Phases[key])
				}
				fmt.Fprintf(&details, "  non-overlapping sum: %v\n", phaseSum)
				return false, fmt.Errorf(
					"non-overlapping phase sum (%v) differs from total duration (%v) by %v (tolerance: %v)\nscenario: %s\nphases:\n%s",
					phaseSum, totalDuration, diff, tolerance, sc.Description, details.String(),
				)
			}

			return true, nil
		},
		genHTTPServerScenario(),
	))

	properties.TestingRun(t)
}

// TestPropertyHTTPPhaseNonNegative verifies that all phase durations are
// non-negative for every probe execution, including failed probes that
// still populate the Phases map.
//
// Validates: Requirement 7.1
func TestPropertyHTTPPhaseNonNegative(t *testing.T) {
	parameters := gopter.DefaultTestParameters()
	parameters.MinSuccessfulTests = 50
	properties := gopter.NewProperties(parameters)

	properties.Property("all HTTP phase durations are non-negative", prop.ForAll(
		func(sc httpServerScenario) (bool, error) {
			srv := startServer(sc)
			defer srv.Close()

			target := config.TargetConfig{
				Name:      "pbt-http-nonneg",
				Address:   srv.URL,
				ProbeType: config.ProbeTypeHTTP,
				Timeout:   10 * time.Second,
				ProbeOpts: config.ProbeOptions{
					TLSSkipVerify: true,
				},
			}

			ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
			defer cancel()

			prober := NewHTTPProber(sc.UseTLS, true, "")
			result := prober.Probe(ctx, target)

			if result.Phases == nil {
				// No phases to check (e.g. request creation error).
				return true, nil
			}

			for key, dur := range result.Phases {
				if dur < 0 {
					return false, fmt.Errorf("phase %q has negative duration %v for scenario: %s", key, dur, sc.Description)
				}
			}

			return true, nil
		},
		genHTTPServerScenario(),
	))

	properties.TestingRun(t)
}

// TestPropertyHTTPTLSPhasePresence verifies that for HTTPS targets,
// tls_handshake is positive, and for plain HTTP targets, tls_handshake is zero.
//
// Validates: Requirement 7.1
func TestPropertyHTTPTLSPhasePresence(t *testing.T) {
	parameters := gopter.DefaultTestParameters()
	parameters.MinSuccessfulTests = 50
	properties := gopter.NewProperties(parameters)

	properties.Property("tls_handshake > 0 iff HTTPS", prop.ForAll(
		func(sc httpServerScenario) (bool, error) {
			srv := startServer(sc)
			defer srv.Close()

			target := config.TargetConfig{
				Name:      "pbt-http-tls-phase",
				Address:   srv.URL,
				ProbeType: config.ProbeTypeHTTP,
				Timeout:   10 * time.Second,
				ProbeOpts: config.ProbeOptions{
					TLSSkipVerify: true,
				},
			}

			ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
			defer cancel()

			prober := NewHTTPProber(sc.UseTLS, true, "")
			result := prober.Probe(ctx, target)

			if !result.Success || result.Phases == nil {
				return true, nil
			}

			tlsDur := result.Phases["tls_handshake"]

			if sc.UseTLS && tlsDur <= 0 {
				return false, fmt.Errorf("expected tls_handshake > 0 for HTTPS, got %v", tlsDur)
			}
			if !sc.UseTLS && tlsDur != 0 {
				return false, fmt.Errorf("expected tls_handshake == 0 for plain HTTP, got %v", tlsDur)
			}

			return true, nil
		},
		genHTTPServerScenario(),
	))

	properties.TestingRun(t)
}

// init registers the httpServerScenario type with gopter for shrinking support.
func init() {
	// Register the type so gopter can display it in failure messages.
	_ = reflect.TypeOf(httpServerScenario{})
}

// TestPropertyHTTPExpectedStatusCodeLogic verifies Property 9:
// For any HTTP probe result with a received response: if expected_status_codes
// is empty, Success SHALL be true; if expected_status_codes is non-empty,
// Success SHALL equal whether the response status code is in the expected list.
// When the code is NOT in the list, Error must be non-empty.
//
// **Validates: Requirements 7.4, 7.5**
func TestPropertyHTTPExpectedStatusCodeLogic(t *testing.T) {
	parameters := gopter.DefaultTestParameters()
	parameters.MinSuccessfulTests = 200
	parameters.MaxSize = 50
	properties := gopter.NewProperties(parameters)

	// genExpectedStatusCodes generates either nil/empty or a non-empty slice
	// of realistic HTTP status codes.
	genExpectedStatusCodes := func() gopter.Gen {
		return gen.Bool().FlatMap(func(v interface{}) gopter.Gen {
			if v.(bool) {
				// Empty list (nil) — any status code is a success.
				return gen.Const([]int(nil))
			}
			// Non-empty list: 1–5 unique status codes.
			return gen.SliceOfN(5, genHTTPStatusCode()).
				Map(func(codes []int) []int {
					// Deduplicate to avoid confusing test output.
					seen := make(map[int]bool)
					unique := make([]int, 0, len(codes))
					for _, c := range codes {
						if !seen[c] {
							seen[c] = true
							unique = append(unique, c)
						}
					}
					return unique
				}).
				SuchThat(func(codes []int) bool {
					return len(codes) > 0
				})
		}, reflect.TypeOf([]int{}))
	}

	properties.Property("expected status code logic: empty → success, non-empty → code-in-list check", prop.ForAll(
		func(serverCode int, expectedCodes []int) (bool, error) {
			// Start a test server that returns the generated status code.
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(serverCode)
			}))
			defer srv.Close()

			target := config.TargetConfig{
				Name:      "pbt-expected-status",
				Address:   srv.URL,
				ProbeType: config.ProbeTypeHTTP,
				Timeout:   10 * time.Second,
				ProbeOpts: config.ProbeOptions{
					ExpectedStatusCodes: expectedCodes,
				},
			}

			ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
			defer cancel()

			prober := NewHTTPProber(false, true, "")
			result := prober.Probe(ctx, target)

			// Verify the status code was recorded correctly.
			if result.StatusCode != serverCode {
				return false, fmt.Errorf("expected StatusCode=%d, got %d", serverCode, result.StatusCode)
			}

			if len(expectedCodes) == 0 {
				// Property 9a: empty expected list → Success must be true.
				if !result.Success {
					return false, fmt.Errorf("expected Success=true with empty expected_status_codes, got false (server=%d, error=%q)",
						serverCode, result.Error)
				}
				if result.Error != "" {
					return false, fmt.Errorf("expected empty Error with empty expected_status_codes, got %q", result.Error)
				}
			} else {
				// Property 9b: non-empty expected list → Success iff code is in list.
				codeInList := false
				for _, c := range expectedCodes {
					if c == serverCode {
						codeInList = true
						break
					}
				}

				if codeInList {
					if !result.Success {
						return false, fmt.Errorf("expected Success=true when server code %d is in expected list %v, got false (error=%q)",
							serverCode, expectedCodes, result.Error)
					}
					if result.Error != "" {
						return false, fmt.Errorf("expected empty Error when code matches, got %q", result.Error)
					}
				} else {
					// Property 9c: code NOT in list → Success=false, Error non-empty.
					if result.Success {
						return false, fmt.Errorf("expected Success=false when server code %d is NOT in expected list %v, got true",
							serverCode, expectedCodes)
					}
					if result.Error == "" {
						return false, fmt.Errorf("expected non-empty Error when code %d is NOT in expected list %v, got empty",
							serverCode, expectedCodes)
					}
				}
			}

			return true, nil
		},
		genHTTPStatusCode(),
		genExpectedStatusCodes(),
	))

	properties.TestingRun(t)
}
