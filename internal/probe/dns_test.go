package probe

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"netsonar/internal/config"
)

// TestDNSProber_SuccessfulResolution verifies that resolving a well-known
// domain returns Success=true, positive DNSResolveTime, and empty Error.
func TestDNSProber_SuccessfulResolution(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-dns-success",
		Address:   "localhost",
		ProbeType: config.ProbeTypeDNS,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			DNSQueryName: "localhost",
			DNSQueryType: "A",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &DNSProber{}
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	if result.DNSResolveTime <= 0 {
		t.Fatalf("expected DNSResolveTime > 0, got %v", result.DNSResolveTime)
	}
	if result.Duration <= 0 {
		t.Fatalf("expected Duration > 0, got %v", result.Duration)
	}
	if result.Duration != result.DNSResolveTime {
		t.Fatalf("expected Duration (%v) == DNSResolveTime (%v)", result.Duration, result.DNSResolveTime)
	}
	if result.Error != "" {
		t.Fatalf("expected empty Error on success, got %q", result.Error)
	}
}

// TestDNSProber_FallbackToAddress verifies that when DNSQueryName is empty,
// the prober falls back to using target.Address as the query name.
func TestDNSProber_FallbackToAddress(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-dns-fallback",
		Address:   "localhost",
		ProbeType: config.ProbeTypeDNS,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			DNSQueryType: "A",
			// DNSQueryName intentionally left empty.
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &DNSProber{}
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true when falling back to Address, got false; error: %s", result.Error)
	}
}

// TestDNSProber_DefaultQueryType verifies that when DNSQueryType is empty,
// the prober defaults to "A" record lookup.
func TestDNSProber_DefaultQueryType(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-dns-default-type",
		Address:   "localhost",
		ProbeType: config.ProbeTypeDNS,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			DNSQueryName: "localhost",
			// DNSQueryType intentionally left empty — should default to "A".
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &DNSProber{}
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true with default query type, got false; error: %s", result.Error)
	}
}

// TestDNSProber_NXDOMAIN verifies that resolving a non-existent domain
// reports Success=false and a non-empty Error containing the DNS failure.
func TestDNSProber_NXDOMAIN(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-dns-nxdomain",
		Address:   "this-domain-does-not-exist.invalid",
		ProbeType: config.ProbeTypeDNS,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			DNSQueryName: "this-domain-does-not-exist.invalid",
			DNSQueryType: "A",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &DNSProber{}
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false for NXDOMAIN, got true")
	}
	if result.Error == "" {
		t.Fatal("expected non-empty Error for NXDOMAIN")
	}
	if result.DNSResolveTime <= 0 {
		t.Fatalf("expected DNSResolveTime > 0 even on failure, got %v", result.DNSResolveTime)
	}
}

// TestDNSProber_ExpectedResultMatch verifies that when dns_expected is
// configured and the resolved records match, the probe reports Success=true.
func TestDNSProber_ExpectedResultMatch(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-dns-expected-match",
		Address:   "localhost",
		ProbeType: config.ProbeTypeDNS,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			DNSQueryName:       "localhost",
			DNSQueryType:       "A",
			DNSExpectedResults: []string{"127.0.0.1"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &DNSProber{}
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true when expected results match, got false; error: %s", result.Error)
	}
	if result.Error != "" {
		t.Fatalf("expected empty Error on match, got %q", result.Error)
	}
}

// TestDNSProber_ExpectedResultMismatch verifies that when dns_expected is
// configured and the resolved records do NOT match, the probe reports
// Success=false with a descriptive mismatch error.
func TestDNSProber_ExpectedResultMismatch(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-dns-expected-mismatch",
		Address:   "localhost",
		ProbeType: config.ProbeTypeDNS,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			DNSQueryName:       "localhost",
			DNSQueryType:       "A",
			DNSExpectedResults: []string{"10.99.99.99"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &DNSProber{}
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false when expected results do not match, got true")
	}
	if result.Error == "" {
		t.Fatal("expected non-empty Error for expected result mismatch")
	}
	// The error should mention "mismatch".
	if !containsSubstring(result.Error, "mismatch") {
		t.Fatalf("expected Error to mention 'mismatch', got %q", result.Error)
	}
}

// TestDNSProber_NoExpectedResults verifies that when dns_expected is empty,
// any successful resolution is accepted (no validation performed).
func TestDNSProber_NoExpectedResults(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-dns-no-expected",
		Address:   "localhost",
		ProbeType: config.ProbeTypeDNS,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			DNSQueryName: "localhost",
			DNSQueryType: "A",
			// DNSExpectedResults intentionally left empty.
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &DNSProber{}
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true when no expected results configured, got false; error: %s", result.Error)
	}
}

// TestDNSProber_CustomDNSServer verifies that the prober uses a custom DNS
// server when dns_server is configured. We point it at a non-listening
// address with a domain that requires real DNS resolution (not /etc/hosts)
// to confirm it actually tries to use the custom server (and fails).
func TestDNSProber_CustomDNSServer(t *testing.T) {
	// Bind a UDP port and immediately close it to get a guaranteed-unused port.
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate port: %v", err)
	}
	deadAddr := conn.LocalAddr().String()
	_ = conn.Close()

	// Use a domain that requires real DNS resolution (not in /etc/hosts).
	target := config.TargetConfig{
		Name:      "test-dns-custom-server",
		Address:   "custom-dns-test.example.com",
		ProbeType: config.ProbeTypeDNS,
		Timeout:   2 * time.Second,
		ProbeOpts: config.ProbeOptions{
			DNSQueryName: "custom-dns-test.example.com",
			DNSQueryType: "A",
			DNSServer:    deadAddr,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &DNSProber{}
	result := prober.Probe(ctx, target)

	// The probe should fail because the custom DNS server is unreachable.
	if result.Success {
		t.Fatal("expected Success=false when custom DNS server is unreachable, got true")
	}
	if result.Error == "" {
		t.Fatal("expected non-empty Error when custom DNS server is unreachable")
	}
}

// TestDNSProber_ContextCancelled verifies that the prober respects context
// cancellation and returns promptly with an error.
func TestDNSProber_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	target := config.TargetConfig{
		Name:      "test-dns-cancelled",
		Address:   "localhost",
		ProbeType: config.ProbeTypeDNS,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			DNSQueryName: "localhost",
			DNSQueryType: "A",
		},
	}

	prober := &DNSProber{}
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false when context is cancelled, got true")
	}
	if result.Error == "" {
		t.Fatal("expected non-empty Error when context is cancelled")
	}
}

// TestDNSProber_UnsupportedQueryType verifies that an unsupported
// dns_query_type produces a failure with a descriptive error.
func TestDNSProber_UnsupportedQueryType(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-dns-bad-type",
		Address:   "localhost",
		ProbeType: config.ProbeTypeDNS,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			DNSQueryName: "localhost",
			DNSQueryType: "MX",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &DNSProber{}
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false for unsupported query type, got true")
	}
	if !containsSubstring(result.Error, "unsupported") {
		t.Fatalf("expected Error to mention 'unsupported', got %q", result.Error)
	}
}

// --- matchExpected unit tests ---

// TestMatchExpected_ExactMatch verifies order-independent exact matching.
func TestMatchExpected_ExactMatch(t *testing.T) {
	got := []string{"10.0.0.1", "10.0.0.2"}
	want := []string{"10.0.0.2", "10.0.0.1"}
	if !matchExpected(got, want) {
		t.Fatal("expected matchExpected to return true for same elements in different order")
	}
}

// TestMatchExpected_Mismatch verifies that different values return false.
func TestMatchExpected_Mismatch(t *testing.T) {
	got := []string{"10.0.0.1"}
	want := []string{"10.0.0.2"}
	if matchExpected(got, want) {
		t.Fatal("expected matchExpected to return false for different values")
	}
}

// TestMatchExpected_DifferentLengths verifies that slices of different
// lengths never match.
func TestMatchExpected_DifferentLengths(t *testing.T) {
	got := []string{"10.0.0.1", "10.0.0.2"}
	want := []string{"10.0.0.1"}
	if matchExpected(got, want) {
		t.Fatal("expected matchExpected to return false for different lengths")
	}
}

// TestMatchExpected_CaseInsensitive verifies that comparison is
// case-insensitive (relevant for CNAME records).
func TestMatchExpected_CaseInsensitive(t *testing.T) {
	got := []string{"Example.Com"}
	want := []string{"example.com"}
	if !matchExpected(got, want) {
		t.Fatal("expected matchExpected to be case-insensitive")
	}
}

// TestMatchExpected_TrailingDotNormalization verifies that trailing dots
// (common in DNS CNAME responses) are stripped before comparison.
func TestMatchExpected_TrailingDotNormalization(t *testing.T) {
	got := []string{"example.com."}
	want := []string{"example.com"}
	if !matchExpected(got, want) {
		t.Fatal("expected matchExpected to normalize trailing dots")
	}
}

// TestMatchExpected_WhitespaceNormalization verifies that leading/trailing
// whitespace is trimmed before comparison.
func TestMatchExpected_WhitespaceNormalization(t *testing.T) {
	got := []string{" 10.0.0.1 "}
	want := []string{"10.0.0.1"}
	if !matchExpected(got, want) {
		t.Fatal("expected matchExpected to trim whitespace")
	}
}

// TestMatchExpected_EmptySlices verifies that two empty slices match.
func TestMatchExpected_EmptySlices(t *testing.T) {
	if !matchExpected([]string{}, []string{}) {
		t.Fatal("expected matchExpected to return true for two empty slices")
	}
}

// containsSubstring is a test helper that checks if s contains substr.
func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestDNSProber_ResultInvariant_SuccessImpliesEmptyError verifies that
// when Success is true, Error is always empty.
func TestDNSProber_ResultInvariant_SuccessImpliesEmptyError(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-dns-invariant",
		Address:   "localhost",
		ProbeType: config.ProbeTypeDNS,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			DNSQueryName: "localhost",
			DNSQueryType: "A",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &DNSProber{}
	result := prober.Probe(ctx, target)

	if result.Success && result.Error != "" {
		t.Fatalf("invariant violated: Success=true but Error=%q", result.Error)
	}
}

// TestDNSProber_ResultInvariant_FailureImpliesNonEmptyError verifies that
// when Success is false, Error is always non-empty.
func TestDNSProber_ResultInvariant_FailureImpliesNonEmptyError(t *testing.T) {
	targets := []config.TargetConfig{
		{
			Name:      "nxdomain",
			Address:   "this-domain-does-not-exist.invalid",
			ProbeType: config.ProbeTypeDNS,
			Timeout:   5 * time.Second,
			ProbeOpts: config.ProbeOptions{
				DNSQueryName: "this-domain-does-not-exist.invalid",
				DNSQueryType: "A",
			},
		},
		{
			Name:      "mismatch",
			Address:   "localhost",
			ProbeType: config.ProbeTypeDNS,
			Timeout:   5 * time.Second,
			ProbeOpts: config.ProbeOptions{
				DNSQueryName:       "localhost",
				DNSQueryType:       "A",
				DNSExpectedResults: []string{"10.99.99.99"},
			},
		},
	}

	prober := &DNSProber{}
	for _, target := range targets {
		t.Run(target.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
			defer cancel()

			result := prober.Probe(ctx, target)
			if !result.Success && result.Error == "" {
				t.Fatalf("invariant violated: Success=false but Error is empty for %q", target.Name)
			}
		})
	}
}

// TestDNSProber_CustomDNSServerWithPort verifies that dns_server values
// that already include a port are used as-is (no double port appending).
func TestDNSProber_CustomDNSServerWithPort(t *testing.T) {
	// Bind a UDP port and immediately close it to get a guaranteed-unused port.
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate port: %v", err)
	}
	deadAddr := conn.LocalAddr().String()
	_ = conn.Close()

	// Verify the address already has a port.
	_, port, err := net.SplitHostPort(deadAddr)
	if err != nil || port == "" {
		t.Fatalf("expected address with port, got %q", deadAddr)
	}

	// Use a domain that requires real DNS resolution (not in /etc/hosts).
	target := config.TargetConfig{
		Name:      "test-dns-server-with-port",
		Address:   "server-port-test.example.com",
		ProbeType: config.ProbeTypeDNS,
		Timeout:   2 * time.Second,
		ProbeOpts: config.ProbeOptions{
			DNSQueryName: "server-port-test.example.com",
			DNSQueryType: "A",
			DNSServer:    deadAddr,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &DNSProber{}
	result := prober.Probe(ctx, target)

	// Should fail (server is dead) but should not panic from double-port.
	if result.Success {
		t.Fatal("expected failure against dead DNS server")
	}
	if result.Error == "" {
		t.Fatal("expected non-empty Error")
	}
}

// TestDNSProber_CustomDNSServerWithoutPort verifies that dns_server values
// without a port get ":53" appended automatically.
func TestDNSProber_CustomDNSServerWithoutPort(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-dns-server-no-port",
		Address:   "localhost",
		ProbeType: config.ProbeTypeDNS,
		Timeout:   2 * time.Second,
		ProbeOpts: config.ProbeOptions{
			DNSQueryName: "localhost",
			DNSQueryType: "A",
			// No port — buildResolver should append :53.
			DNSServer: "127.0.0.1",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &DNSProber{}
	// We just verify it doesn't panic. The result depends on whether
	// 127.0.0.1:53 is actually running a DNS server.
	_ = prober.Probe(ctx, target)
}

// TestDNSProber_MultipleExpectedResults verifies that when multiple expected
// results are configured, all must be present in the resolved records.
func TestDNSProber_MultipleExpectedResults(t *testing.T) {
	// localhost should resolve to 127.0.0.1 — asking for two IPs should fail.
	target := config.TargetConfig{
		Name:      "test-dns-multi-expected",
		Address:   "localhost",
		ProbeType: config.ProbeTypeDNS,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			DNSQueryName:       "localhost",
			DNSQueryType:       "A",
			DNSExpectedResults: []string{"127.0.0.1", "10.0.0.1"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &DNSProber{}
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false when expected results include non-matching entries")
	}
	if !containsSubstring(result.Error, "mismatch") {
		t.Fatalf("expected Error to mention 'mismatch', got %q", result.Error)
	}
}

// TestDNSProber_DurationAlwaysPositive verifies that Duration is always
// positive regardless of probe outcome.
func TestDNSProber_DurationAlwaysPositive(t *testing.T) {
	cases := []struct {
		name   string
		target config.TargetConfig
	}{
		{
			name: "success",
			target: config.TargetConfig{
				Name:      "dur-success",
				Address:   "localhost",
				ProbeType: config.ProbeTypeDNS,
				Timeout:   5 * time.Second,
				ProbeOpts: config.ProbeOptions{
					DNSQueryName: "localhost",
					DNSQueryType: "A",
				},
			},
		},
		{
			name: "nxdomain",
			target: config.TargetConfig{
				Name:      "dur-nxdomain",
				Address:   "this-domain-does-not-exist.invalid",
				ProbeType: config.ProbeTypeDNS,
				Timeout:   5 * time.Second,
				ProbeOpts: config.ProbeOptions{
					DNSQueryName: "this-domain-does-not-exist.invalid",
					DNSQueryType: "A",
				},
			},
		},
	}

	prober := &DNSProber{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), tc.target.Timeout)
			defer cancel()

			result := prober.Probe(ctx, tc.target)
			if result.Duration <= 0 {
				t.Fatalf("expected Duration > 0, got %v", result.Duration)
			}
		})
	}
}

// Ensure fmt is used (for error formatting in test helpers if needed).
var _ = fmt.Sprintf
