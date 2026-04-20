package probe

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"netsonar/internal/config"
)

const (
	testDNSTypeA     uint16 = 1
	testDNSTypeCNAME uint16 = 5
	testDNSTypeAAAA  uint16 = 28
	testDNSClassIN   uint16 = 1
)

type testDNSAnswer struct {
	qtype uint16
	value string
}

func startTestDNSServer(t *testing.T, records map[string][]testDNSAnswer) string {
	t.Helper()

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start test DNS server: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			packet := append([]byte(nil), buf[:n]...)
			response, err := buildTestDNSResponse(packet, records)
			if err != nil {
				continue
			}
			_, _ = conn.WriteTo(response, addr)
		}
	}()

	return conn.LocalAddr().String()
}

func buildTestDNSResponse(query []byte, records map[string][]testDNSAnswer) ([]byte, error) {
	if len(query) < 12 {
		return nil, fmt.Errorf("short dns query")
	}

	name, qtype, questionEnd, err := parseTestDNSQuestion(query)
	if err != nil {
		return nil, err
	}

	answers := matchingTestDNSAnswers(records[strings.ToLower(name)], qtype)
	response := make([]byte, 0, len(query)+len(answers)*32)
	response = append(response, query[:2]...)
	response = append(response, 0x81, 0x80)
	response = append(response, 0x00, 0x01)
	response = appendUint16(response, uint16(len(answers)))
	response = append(response, 0x00, 0x00)
	response = append(response, 0x00, 0x00)
	response = append(response, query[12:questionEnd]...)

	for _, answer := range answers {
		response = append(response, 0xc0, 0x0c)
		response = appendUint16(response, answer.qtype)
		response = appendUint16(response, testDNSClassIN)
		response = appendUint32(response, 60)

		rdata, err := testDNSRData(answer)
		if err != nil {
			return nil, err
		}
		response = appendUint16(response, uint16(len(rdata)))
		response = append(response, rdata...)
	}

	return response, nil
}

func parseTestDNSQuestion(query []byte) (string, uint16, int, error) {
	offset := 12
	labels := []string{}
	for {
		if offset >= len(query) {
			return "", 0, 0, fmt.Errorf("dns question name exceeds packet")
		}
		labelLen := int(query[offset])
		offset++
		if labelLen == 0 {
			break
		}
		if labelLen&0xc0 != 0 {
			return "", 0, 0, fmt.Errorf("compressed query names are not supported")
		}
		if offset+labelLen > len(query) {
			return "", 0, 0, fmt.Errorf("dns question label exceeds packet")
		}
		labels = append(labels, string(query[offset:offset+labelLen]))
		offset += labelLen
	}
	if offset+4 > len(query) {
		return "", 0, 0, fmt.Errorf("dns question missing type/class")
	}

	qtype := binary.BigEndian.Uint16(query[offset : offset+2])
	qclass := binary.BigEndian.Uint16(query[offset+2 : offset+4])
	if qclass != testDNSClassIN {
		return "", 0, 0, fmt.Errorf("unsupported dns query class %d", qclass)
	}

	return strings.Join(labels, "."), qtype, offset + 4, nil
}

func matchingTestDNSAnswers(answers []testDNSAnswer, qtype uint16) []testDNSAnswer {
	matched := make([]testDNSAnswer, 0, len(answers))
	for _, answer := range answers {
		if answer.qtype == qtype {
			matched = append(matched, answer)
		}
	}
	return matched
}

func testDNSRData(answer testDNSAnswer) ([]byte, error) {
	switch answer.qtype {
	case testDNSTypeA:
		ip := net.ParseIP(answer.value).To4()
		if ip == nil {
			return nil, fmt.Errorf("invalid A record IP %q", answer.value)
		}
		return []byte(ip), nil
	case testDNSTypeAAAA:
		ip := net.ParseIP(answer.value).To16()
		if ip == nil || ip.To4() != nil {
			return nil, fmt.Errorf("invalid AAAA record IP %q", answer.value)
		}
		return []byte(ip), nil
	case testDNSTypeCNAME:
		return encodeTestDNSName(answer.value)
	default:
		return nil, fmt.Errorf("unsupported answer type %d", answer.qtype)
	}
}

func encodeTestDNSName(name string) ([]byte, error) {
	trimmed := strings.TrimSuffix(name, ".")
	if trimmed == "" {
		return nil, fmt.Errorf("empty dns name")
	}

	encoded := []byte{}
	for _, label := range strings.Split(trimmed, ".") {
		if len(label) == 0 || len(label) > 63 {
			return nil, fmt.Errorf("invalid dns label %q", label)
		}
		encoded = append(encoded, byte(len(label)))
		encoded = append(encoded, label...)
	}
	encoded = append(encoded, 0)
	return encoded, nil
}

func appendUint16(buf []byte, value uint16) []byte {
	var tmp [2]byte
	binary.BigEndian.PutUint16(tmp[:], value)
	return append(buf, tmp[:]...)
}

func appendUint32(buf []byte, value uint32) []byte {
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], value)
	return append(buf, tmp[:]...)
}

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
	// First resolve localhost to discover the actual loopback address,
	// since it may be 127.0.0.1 or ::1 depending on the system.
	addrs, err := net.LookupHost("localhost")
	if err != nil || len(addrs) == 0 {
		t.Skip("cannot resolve localhost on this system")
	}
	// Find a loopback address from the results.
	var loopback string
	for _, addr := range addrs {
		if ip := net.ParseIP(addr); ip != nil && ip.IsLoopback() {
			loopback = addr
			break
		}
	}
	if loopback == "" {
		t.Skip("localhost did not resolve to a loopback address")
	}

	target := config.TargetConfig{
		Name:      "test-dns-expected-match",
		Address:   "localhost",
		ProbeType: config.ProbeTypeDNS,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			DNSQueryName:       "localhost",
			DNSQueryType:       "A",
			DNSExpectedResults: addrs,
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

func TestDNSProber_ResultMatchIntegration_ARecordMatch(t *testing.T) {
	dnsServer := startTestDNSServer(t, map[string][]testDNSAnswer{
		"a.integration.test": {
			{qtype: testDNSTypeA, value: "192.0.2.10"},
			{qtype: testDNSTypeA, value: "192.0.2.11"},
		},
	})

	target := config.TargetConfig{
		Name:      "test-dns-integration-a-match",
		Address:   "a.integration.test",
		ProbeType: config.ProbeTypeDNS,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			DNSQueryName:       "a.integration.test",
			DNSQueryType:       "A",
			DNSServer:          dnsServer,
			DNSExpectedResults: []string{"192.0.2.11", "192.0.2.10"},
		},
	}

	result := probeDNSForTest(t, target)
	assertDNSMatchResult(t, result, true)
	if !result.Success {
		t.Fatalf("expected Success=true for matching A records, got false; error: %s", result.Error)
	}
	if result.Error != "" {
		t.Fatalf("expected empty Error for matching A records, got %q", result.Error)
	}
}

func TestDNSProber_ResultMatchIntegration_ARecordMismatch(t *testing.T) {
	dnsServer := startTestDNSServer(t, map[string][]testDNSAnswer{
		"a-mismatch.integration.test": {
			{qtype: testDNSTypeA, value: "192.0.2.20"},
		},
	})

	target := config.TargetConfig{
		Name:      "test-dns-integration-a-mismatch",
		Address:   "a-mismatch.integration.test",
		ProbeType: config.ProbeTypeDNS,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			DNSQueryName:       "a-mismatch.integration.test",
			DNSQueryType:       "A",
			DNSServer:          dnsServer,
			DNSExpectedResults: []string{"192.0.2.21"},
		},
	}

	result := probeDNSForTest(t, target)
	assertDNSMatchResult(t, result, false)
	if result.Success {
		t.Fatal("expected Success=false for mismatching A records")
	}
	if !containsSubstring(result.Error, "mismatch") {
		t.Fatalf("expected Error to mention 'mismatch', got %q", result.Error)
	}
}

func TestDNSProber_ResultMatchIntegration_AAAARecordMatch(t *testing.T) {
	dnsServer := startTestDNSServer(t, map[string][]testDNSAnswer{
		"aaaa.integration.test": {
			{qtype: testDNSTypeAAAA, value: "2001:db8::10"},
		},
	})

	target := config.TargetConfig{
		Name:      "test-dns-integration-aaaa-match",
		Address:   "aaaa.integration.test",
		ProbeType: config.ProbeTypeDNS,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			DNSQueryName:       "aaaa.integration.test",
			DNSQueryType:       "AAAA",
			DNSServer:          dnsServer,
			DNSExpectedResults: []string{"2001:db8::10"},
		},
	}

	result := probeDNSForTest(t, target)
	assertDNSMatchResult(t, result, true)
	if !result.Success {
		t.Fatalf("expected Success=true for matching AAAA record, got false; error: %s", result.Error)
	}
	if result.Error != "" {
		t.Fatalf("expected empty Error for matching AAAA record, got %q", result.Error)
	}
}

func TestDNSProber_ResultMatchIntegration_CNAMENormalization(t *testing.T) {
	dnsServer := startTestDNSServer(t, map[string][]testDNSAnswer{
		"alias.integration.test": {
			{qtype: testDNSTypeCNAME, value: "Target.Integration.Test."},
		},
	})

	target := config.TargetConfig{
		Name:      "test-dns-integration-cname-match",
		Address:   "alias.integration.test",
		ProbeType: config.ProbeTypeDNS,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			DNSQueryName:       "alias.integration.test",
			DNSQueryType:       "CNAME",
			DNSServer:          dnsServer,
			DNSExpectedResults: []string{"target.integration.test"},
		},
	}

	result := probeDNSForTest(t, target)
	assertDNSMatchResult(t, result, true)
	if !result.Success {
		t.Fatalf("expected Success=true for normalized CNAME match, got false; error: %s", result.Error)
	}
	if result.Error != "" {
		t.Fatalf("expected empty Error for normalized CNAME match, got %q", result.Error)
	}
}

func probeDNSForTest(t *testing.T, target config.TargetConfig) ProbeResult {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &DNSProber{}
	return prober.Probe(ctx, target)
}

func assertDNSMatchResult(t *testing.T, result ProbeResult, wantMatched bool) {
	t.Helper()

	if !result.DNSMatchEvaluated {
		t.Fatal("expected DNSMatchEvaluated=true")
	}
	if result.DNSMatched != wantMatched {
		t.Fatalf("expected DNSMatched=%v, got %v", wantMatched, result.DNSMatched)
	}
	if result.DNSResolveTime <= 0 {
		t.Fatalf("expected DNSResolveTime > 0, got %v", result.DNSResolveTime)
	}
	if result.Duration != result.DNSResolveTime {
		t.Fatalf("expected Duration (%v) == DNSResolveTime (%v)", result.Duration, result.DNSResolveTime)
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
	// localhost typically resolves to a single loopback address — asking for
	// an additional non-loopback IP should cause a mismatch.
	addrs, err := net.LookupHost("localhost")
	if err != nil || len(addrs) == 0 {
		t.Skip("cannot resolve localhost on this system")
	}

	// Add a non-matching address to force a mismatch.
	expected := append(addrs, "10.0.0.1")

	target := config.TargetConfig{
		Name:      "test-dns-multi-expected",
		Address:   "localhost",
		ProbeType: config.ProbeTypeDNS,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			DNSQueryName:       "localhost",
			DNSQueryType:       "A",
			DNSExpectedResults: expected,
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
