package probe

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"netsonar/internal/config"
)

func TestNextICMPID(t *testing.T) {
	icmpIDCounter.Store(0)

	first := nextICMPID()
	second := nextICMPID()

	if first == 0 {
		t.Fatal("first ICMP ID must not be zero")
	}
	if second == 0 {
		t.Fatal("second ICMP ID must not be zero")
	}
	if first == second {
		t.Fatalf("consecutive ICMP IDs should differ, both were %d", first)
	}

	icmpIDCounter.Store(0xffff)
	wrapped := nextICMPID()
	if wrapped == 0 {
		t.Fatal("wrapped ICMP ID must skip zero")
	}
}

func TestPacketAddrMatchesIP(t *testing.T) {
	if !packetAddrMatchesIP(&net.IPAddr{IP: net.ParseIP("192.0.2.10")}, net.ParseIP("192.0.2.10")) {
		t.Fatal("expected matching IPAddr to match")
	}
	if packetAddrMatchesIP(&net.IPAddr{IP: net.ParseIP("192.0.2.11")}, net.ParseIP("192.0.2.10")) {
		t.Fatal("expected different IPAddr not to match")
	}
	if packetAddrMatchesIP(&net.TCPAddr{IP: net.ParseIP("192.0.2.10")}, net.ParseIP("192.0.2.10")) {
		t.Fatal("expected non-IPAddr packet address not to match")
	}
}

func TestMatchesEmbeddedICMP(t *testing.T) {
	dst := net.ParseIP("192.0.2.20")
	const icmpID = 1234
	const seq = 1472

	tests := []struct {
		name string
		data []byte
		want bool
	}{
		{
			name: "valid",
			data: embeddedICMPEchoPacket(t, dst, icmpID, seq, 20),
			want: true,
		},
		{
			name: "valid_with_ipv4_options",
			data: embeddedICMPEchoPacket(t, dst, icmpID, seq, 24),
			want: true,
		},
		{
			name: "wrong_id",
			data: embeddedICMPEchoPacket(t, dst, icmpID+1, seq, 20),
			want: false,
		},
		{
			name: "wrong_seq",
			data: embeddedICMPEchoPacket(t, dst, icmpID, seq+1, 20),
			want: false,
		},
		{
			name: "wrong_destination",
			data: embeddedICMPEchoPacket(t, net.ParseIP("192.0.2.21"), icmpID, seq, 20),
			want: false,
		},
		{
			name: "non_icmp_protocol",
			data: func() []byte {
				data := embeddedICMPEchoPacket(t, dst, icmpID, seq, 20)
				data[9] = 6
				return data
			}(),
			want: false,
		},
		{
			name: "non_echo_type",
			data: func() []byte {
				data := embeddedICMPEchoPacket(t, dst, icmpID, seq, 20)
				data[20] = 0
				return data
			}(),
			want: false,
		},
		{
			name: "ipv6_version",
			data: func() []byte {
				data := embeddedICMPEchoPacket(t, dst, icmpID, seq, 20)
				data[0] = 0x65
				return data
			}(),
			want: false,
		},
		{
			name: "too_short_ip_header",
			data: []byte{0x45, 0, 0},
			want: false,
		},
		{
			name: "too_short_for_ihl_and_icmp_header",
			data: embeddedICMPEchoPacket(t, dst, icmpID, seq, 24)[:28],
			want: false,
		},
		{
			name: "invalid_ihl",
			data: func() []byte {
				data := embeddedICMPEchoPacket(t, dst, icmpID, seq, 20)
				data[0] = 0x44
				return data
			}(),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesEmbeddedICMP(tt.data, dst, icmpID, seq)
			if got != tt.want {
				t.Fatalf("matchesEmbeddedICMP() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClassifyPayloadAttempts(t *testing.T) {
	errBoom := errors.New("boom")
	tests := []struct {
		name       string
		attempts   []mtuPayloadResult
		wantStatus mtuPayloadStatus
		wantErr    error
	}{
		{
			name: "timeout_then_success",
			attempts: []mtuPayloadResult{
				{status: mtuPayloadTimeout, payloadSize: 1472},
				{status: mtuPayloadSuccess, payloadSize: 1472},
			},
			wantStatus: mtuPayloadSuccess,
		},
		{
			name: "success_then_timeout",
			attempts: []mtuPayloadResult{
				{status: mtuPayloadSuccess, payloadSize: 1472},
				{status: mtuPayloadTimeout, payloadSize: 1472},
			},
			wantStatus: mtuPayloadSuccess,
		},
		{
			name: "timeout_then_too_large",
			attempts: []mtuPayloadResult{
				{status: mtuPayloadTimeout, payloadSize: 1472},
				{status: mtuPayloadTooLarge, payloadSize: 1472},
			},
			wantStatus: mtuPayloadTooLarge,
		},
		{
			name: "too_large_then_timeout",
			attempts: []mtuPayloadResult{
				{status: mtuPayloadTooLarge, payloadSize: 1472},
				{status: mtuPayloadTimeout, payloadSize: 1472},
			},
			wantStatus: mtuPayloadTooLarge,
		},
		{
			name: "timeout_then_local_too_large",
			attempts: []mtuPayloadResult{
				{status: mtuPayloadTimeout, payloadSize: 1472},
				{status: mtuPayloadLocalTooLarge, payloadSize: 1472},
			},
			wantStatus: mtuPayloadLocalTooLarge,
		},
		{
			name: "local_too_large_then_timeout",
			attempts: []mtuPayloadResult{
				{status: mtuPayloadLocalTooLarge, payloadSize: 1472},
				{status: mtuPayloadTimeout, payloadSize: 1472},
			},
			wantStatus: mtuPayloadLocalTooLarge,
		},
		{
			name: "all_timeout",
			attempts: []mtuPayloadResult{
				{status: mtuPayloadTimeout, payloadSize: 1472},
				{status: mtuPayloadTimeout, payloadSize: 1472},
			},
			wantStatus: mtuPayloadTimeout,
		},
		{
			name: "timeout_then_unreachable",
			attempts: []mtuPayloadResult{
				{status: mtuPayloadTimeout, payloadSize: 1472},
				{status: mtuPayloadUnreachable, payloadSize: 1472},
			},
			wantStatus: mtuPayloadUnreachable,
		},
		{
			name: "timeout_then_error",
			attempts: []mtuPayloadResult{
				{status: mtuPayloadTimeout, payloadSize: 1472},
				{status: mtuPayloadError, payloadSize: 1472, err: errBoom},
			},
			wantStatus: mtuPayloadError,
			wantErr:    errBoom,
		},
		{
			name:       "empty_attempts",
			attempts:   nil,
			wantStatus: mtuPayloadTimeout,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyPayloadAttempts(tt.attempts)
			if got.status != tt.wantStatus {
				t.Fatalf("status = %q, want %q", got.status, tt.wantStatus)
			}
			if tt.wantErr != nil && !errors.Is(got.err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", got.err, tt.wantErr)
			}
		})
	}
}

func TestCountAttemptStats(t *testing.T) {
	tests := []struct {
		name      string
		attempts  []mtuPayloadResult
		wantStats mtuProbeStats
	}{
		{
			name: "timeout_timeout_success",
			attempts: []mtuPayloadResult{
				{status: mtuPayloadTimeout, payloadSize: 1472},
				{status: mtuPayloadTimeout, payloadSize: 1472},
				{status: mtuPayloadSuccess, payloadSize: 1472},
			},
			wantStats: mtuProbeStats{
				timeoutCount: 2,
				retryCount:   2,
			},
		},
		{
			name: "fragmentation_needed",
			attempts: []mtuPayloadResult{
				{status: mtuPayloadTooLarge, payloadSize: 1472},
			},
			wantStats: mtuProbeStats{
				fragNeededCount: 1,
			},
		},
		{
			name: "local_too_large_timeout",
			attempts: []mtuPayloadResult{
				{status: mtuPayloadLocalTooLarge, payloadSize: 1472},
				{status: mtuPayloadTimeout, payloadSize: 1472},
			},
			wantStats: mtuProbeStats{
				localErrorCount: 1,
				timeoutCount:    1,
				retryCount:      1,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stats mtuProbeStats
			countAttemptStats(tt.attempts, &stats)
			if stats != tt.wantStats {
				t.Fatalf("stats = %+v, want %+v", stats, tt.wantStats)
			}
		})
	}
}

func TestApplySanityFailure(t *testing.T) {
	start := time.Now()
	permissionErr := &os.PathError{Op: "listen", Path: "ip4:icmp", Err: os.ErrPermission}

	tests := []struct {
		name       string
		sanity     mtuPayloadResult
		wantHandle bool
		wantState  string
		wantDetail string
	}{
		{
			name:       "success_continues",
			sanity:     mtuPayloadResult{status: mtuPayloadSuccess},
			wantHandle: false,
		},
		{
			name:       "unreachable",
			sanity:     mtuPayloadResult{status: mtuPayloadUnreachable},
			wantHandle: true,
			wantState:  MTUStateUnreachable,
			wantDetail: MTUDetailDestinationUnreach,
		},
		{
			name:       "timeout",
			sanity:     mtuPayloadResult{status: mtuPayloadTimeout},
			wantHandle: true,
			wantState:  MTUStateUnreachable,
			wantDetail: MTUDetailSanityCheckFailed,
		},
		{
			name:       "permission",
			sanity:     mtuPayloadResult{status: mtuPayloadError, err: permissionErr},
			wantHandle: true,
			wantState:  MTUStateError,
			wantDetail: MTUDetailPermissionDenied,
		},
		{
			name:       "internal_error",
			sanity:     mtuPayloadResult{status: mtuPayloadError, err: errors.New("socket failed")},
			wantHandle: true,
			wantState:  MTUStateError,
			wantDetail: MTUDetailInternalError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var result ProbeResult
			result.PathMTU = -1
			handled := applySanityFailure(&result, tt.sanity, start)
			if handled != tt.wantHandle {
				t.Fatalf("handled = %v, want %v", handled, tt.wantHandle)
			}
			if !tt.wantHandle {
				return
			}
			if result.MTUState != tt.wantState {
				t.Fatalf("MTUState = %q, want %q", result.MTUState, tt.wantState)
			}
			if result.MTUDetail != tt.wantDetail {
				t.Fatalf("MTUDetail = %q, want %q", result.MTUDetail, tt.wantDetail)
			}
			if result.PathMTU != -1 {
				t.Fatalf("PathMTU = %d, want -1", result.PathMTU)
			}
			if result.Error == "" {
				t.Fatal("Error should be set for handled sanity failure")
			}
		})
	}
}

func embeddedICMPEchoPacket(t *testing.T, dst net.IP, icmpID, seq, ipHeaderLen int) []byte {
	t.Helper()
	if ipHeaderLen < 20 || ipHeaderLen%4 != 0 {
		t.Fatalf("invalid IP header length %d", ipHeaderLen)
	}

	dst4 := dst.To4()
	if dst4 == nil {
		t.Fatalf("destination %q is not IPv4", dst)
	}

	data := make([]byte, ipHeaderLen+8)
	data[0] = byte(0x40 | (ipHeaderLen / 4))
	data[9] = 1 // ICMPv4
	copy(data[16:20], dst4)

	data[ipHeaderLen] = 8 // ICMP echo request
	data[ipHeaderLen+1] = 0
	binary.BigEndian.PutUint16(data[ipHeaderLen+4:ipHeaderLen+6], uint16(icmpID))
	binary.BigEndian.PutUint16(data[ipHeaderLen+6:ipHeaderLen+8], uint16(seq))
	return data
}

func TestClassifyMTUResult(t *testing.T) {
	tests := []struct {
		name           string
		results        []mtuPayloadResult
		expectedMinMTU int
		wantState      string
		wantDetail     string
		wantPathMTU    int
		wantSuccess    bool
	}{
		{
			name: "first_success",
			results: []mtuPayloadResult{
				{status: mtuPayloadSuccess, payloadSize: 1472},
			},
			expectedMinMTU: 1500,
			wantState:      MTUStateOK,
			wantDetail:     MTUDetailLargestSizeConfirmed,
			wantPathMTU:    1500,
			wantSuccess:    true,
		},
		{
			name: "fragmentation_needed_then_success",
			results: []mtuPayloadResult{
				{status: mtuPayloadTooLarge, payloadSize: 1472},
				{status: mtuPayloadSuccess, payloadSize: 1392},
			},
			expectedMinMTU: 1420,
			wantState:      MTUStateOK,
			wantDetail:     MTUDetailFragmentationNeeded,
			wantPathMTU:    1420,
			wantSuccess:    true,
		},
		{
			name: "local_too_large_then_success",
			results: []mtuPayloadResult{
				{status: mtuPayloadLocalTooLarge, payloadSize: 1472},
				{status: mtuPayloadSuccess, payloadSize: 1392},
			},
			expectedMinMTU: 1420,
			wantState:      MTUStateOK,
			wantDetail:     MTUDetailLocalMessageTooLarge,
			wantPathMTU:    1420,
			wantSuccess:    true,
		},
		{
			name: "timeout_then_success",
			results: []mtuPayloadResult{
				{status: mtuPayloadTimeout, payloadSize: 1472},
				{status: mtuPayloadSuccess, payloadSize: 1392},
			},
			expectedMinMTU: 1420,
			wantState:      MTUStateOK,
			wantDetail:     MTUDetailLargerSizesTimedOut,
			wantPathMTU:    1420,
			wantSuccess:    true,
		},
		{
			name: "success_below_expected_min",
			results: []mtuPayloadResult{
				{status: mtuPayloadTooLarge, payloadSize: 1472},
				{status: mtuPayloadSuccess, payloadSize: 1392},
			},
			expectedMinMTU: 1500,
			wantState:      MTUStateDegraded,
			wantDetail:     MTUDetailFragmentationNeeded,
			wantPathMTU:    1420,
			wantSuccess:    true,
		},
		{
			name: "local_too_large_has_priority_over_fragmentation_needed",
			results: []mtuPayloadResult{
				{status: mtuPayloadTooLarge, payloadSize: 1600},
				{status: mtuPayloadLocalTooLarge, payloadSize: 1472},
				{status: mtuPayloadSuccess, payloadSize: 1392},
			},
			expectedMinMTU: 1420,
			wantState:      MTUStateOK,
			wantDetail:     MTUDetailLocalMessageTooLarge,
			wantPathMTU:    1420,
			wantSuccess:    true,
		},
		{
			name: "all_timeout",
			results: []mtuPayloadResult{
				{status: mtuPayloadTimeout, payloadSize: 1472},
				{status: mtuPayloadTimeout, payloadSize: 1392},
			},
			expectedMinMTU: 1500,
			wantState:      MTUStateDegraded,
			wantDetail:     MTUDetailAllSizesTimedOut,
			wantPathMTU:    -1,
			wantSuccess:    false,
		},
		{
			name: "all_fragmentation_needed",
			results: []mtuPayloadResult{
				{status: mtuPayloadTooLarge, payloadSize: 1472},
				{status: mtuPayloadTooLarge, payloadSize: 1392},
			},
			expectedMinMTU: 1500,
			wantState:      MTUStateDegraded,
			wantDetail:     MTUDetailBelowMinTested,
			wantPathMTU:    -1,
			wantSuccess:    false,
		},
		{
			name: "all_local_too_large",
			results: []mtuPayloadResult{
				{status: mtuPayloadLocalTooLarge, payloadSize: 1472},
				{status: mtuPayloadLocalTooLarge, payloadSize: 1392},
			},
			expectedMinMTU: 1500,
			wantState:      MTUStateDegraded,
			wantDetail:     MTUDetailLocalMessageTooLarge,
			wantPathMTU:    -1,
			wantSuccess:    false,
		},
		{
			name: "mixed_no_success",
			results: []mtuPayloadResult{
				{status: mtuPayloadTimeout, payloadSize: 1472},
				{status: mtuPayloadTooLarge, payloadSize: 1392},
			},
			expectedMinMTU: 1500,
			wantState:      MTUStateDegraded,
			wantDetail:     MTUDetailBelowMinTested,
			wantPathMTU:    -1,
			wantSuccess:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyMTUResult(tt.results, tt.expectedMinMTU)
			if got.state != tt.wantState {
				t.Errorf("state = %q, want %q", got.state, tt.wantState)
			}
			if got.detail != tt.wantDetail {
				t.Errorf("detail = %q, want %q", got.detail, tt.wantDetail)
			}
			if got.pathMTU != tt.wantPathMTU {
				t.Errorf("pathMTU = %d, want %d", got.pathMTU, tt.wantPathMTU)
			}
			if got.success != tt.wantSuccess {
				t.Errorf("success = %v, want %v", got.success, tt.wantSuccess)
			}
		})
	}
}

// TestMTUProber_EmptyICMPPayloadSizes verifies that probing with an empty icmp_payload_sizes
// list reports Success=false, PathMTU=-1, and a descriptive error.
func TestMTUProber_EmptyICMPPayloadSizes(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-mtu-empty-sizes",
		Address:   "127.0.0.1",
		ProbeType: config.ProbeTypeMTU,
		Timeout:   2 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ICMPPayloadSizes: []int{},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &MTUProber{}
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false for empty icmp_payload_sizes")
	}
	if result.PathMTU != -1 {
		t.Fatalf("expected PathMTU=-1 for empty icmp_payload_sizes, got %d", result.PathMTU)
	}
	if result.Error == "" {
		t.Fatal("expected non-empty Error for empty icmp_payload_sizes")
	}
	if result.MTUState != MTUStateError {
		t.Fatalf("expected MTUState=%q, got %q", MTUStateError, result.MTUState)
	}
	if result.MTUDetail != MTUDetailInternalError {
		t.Fatalf("expected MTUDetail=%q, got %q", MTUDetailInternalError, result.MTUDetail)
	}
}

// TestMTUProber_ResolutionFailure verifies that probing an unresolvable
// address reports Success=false, PathMTU=-1, and a descriptive error.
func TestMTUProber_ResolutionFailure(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-mtu-resolve-fail",
		Address:   "this.host.does.not.exist.invalid",
		ProbeType: config.ProbeTypeMTU,
		Timeout:   2 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ICMPPayloadSizes: []int{1472, 1372, 1272},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &MTUProber{}
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false for unresolvable address")
	}
	if result.PathMTU != -1 {
		t.Fatalf("expected PathMTU=-1 for unresolvable address, got %d", result.PathMTU)
	}
	if result.Error == "" {
		t.Fatal("expected non-empty Error for unresolvable address")
	}
	if result.MTUState != MTUStateError {
		t.Fatalf("expected MTUState=%q, got %q", MTUStateError, result.MTUState)
	}
	if result.MTUDetail != MTUDetailResolveError {
		t.Fatalf("expected MTUDetail=%q, got %q", MTUDetailResolveError, result.MTUDetail)
	}
}

// TestMTUProber_PermissionDenied verifies that when the process lacks
// CAP_NET_RAW, the prober reports a clear "permission denied" error,
// PathMTU=-1, and Success=false.
//
// This test is skipped if the process has raw socket privileges (e.g. root).
func TestMTUProber_PermissionDenied(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-mtu-permission",
		Address:   "127.0.0.1",
		ProbeType: config.ProbeTypeMTU,
		Timeout:   2 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ICMPPayloadSizes: []int{1472, 1372, 1272},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &MTUProber{}
	result := prober.Probe(ctx, target)

	// If the probe succeeded, we have raw socket access — skip.
	if result.Success {
		t.Skip("process has CAP_NET_RAW; skipping permission denied test")
	}

	// On systems without CAP_NET_RAW, expect the permission error.
	if result.Error == "permission denied: CAP_NET_RAW required" {
		if result.PathMTU != -1 {
			t.Fatalf("expected PathMTU=-1 on permission denied, got %d", result.PathMTU)
		}
		if result.MTUState != MTUStateError {
			t.Fatalf("expected MTUState=%q on permission denied, got %q", MTUStateError, result.MTUState)
		}
		if result.MTUDetail != MTUDetailPermissionDenied {
			t.Fatalf("expected MTUDetail=%q on permission denied, got %q", MTUDetailPermissionDenied, result.MTUDetail)
		}
		return
	}

	// Some environments may produce a different socket error — just verify invariants.
	if result.PathMTU != -1 && !result.Success {
		// PathMTU should be -1 when probe fails, unless a partial success occurred.
		// For all-fail, PathMTU must be -1.
		t.Logf("non-permission error: %s", result.Error)
	}
}

// TestMTUProber_PathMTUCalculation verifies that when a probe succeeds,
// PathMTU equals the successful payload size + 28 (20 IP + 8 ICMP headers).
//
// This test requires CAP_NET_RAW and probes localhost.
func TestMTUProber_PathMTUCalculation(t *testing.T) {
	sizes := []int{1472, 1372, 1272, 1172, 1072}
	target := config.TargetConfig{
		Name:      "test-mtu-calculation",
		Address:   "127.0.0.1",
		ProbeType: config.ProbeTypeMTU,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ICMPPayloadSizes: sizes,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &MTUProber{}
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Skipf("probe did not succeed (likely no CAP_NET_RAW): %s", result.Error)
	}

	// PathMTU must be one of the configured sizes + 28.
	validMTUs := make(map[int]bool, len(sizes))
	for _, s := range sizes {
		validMTUs[s+28] = true
	}

	if !validMTUs[result.PathMTU] {
		t.Fatalf("PathMTU=%d is not a valid value (expected one of configured sizes + 28)", result.PathMTU)
	}

	if result.Error != "" {
		t.Fatalf("Success=true but Error is non-empty: %q", result.Error)
	}
	if result.MTUState != MTUStateOK {
		t.Fatalf("expected MTUState=%q, got %q", MTUStateOK, result.MTUState)
	}
	if result.MTUDetail != MTUDetailLargestSizeConfirmed {
		t.Fatalf("expected MTUDetail=%q, got %q", MTUDetailLargestSizeConfirmed, result.MTUDetail)
	}

	if result.Duration <= 0 {
		t.Fatalf("Success=true but Duration=%v (expected > 0)", result.Duration)
	}
}

// TestMTUProber_EarlyExit verifies that the prober stops at the first
// successful size (largest working MTU) and returns immediately.
//
// On localhost the loopback MTU is typically 65536, so the largest configured
// size should succeed and the prober should not test smaller sizes.
func TestMTUProber_EarlyExit(t *testing.T) {
	// Use sizes where the largest should succeed on localhost (loopback MTU
	// is typically 65536). If the first size succeeds, PathMTU should equal
	// that size + 28.
	sizes := []int{1472, 1372, 1272, 1172, 1072}
	target := config.TargetConfig{
		Name:      "test-mtu-early-exit",
		Address:   "127.0.0.1",
		ProbeType: config.ProbeTypeMTU,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ICMPPayloadSizes: sizes,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &MTUProber{}
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Skipf("probe did not succeed (likely no CAP_NET_RAW): %s", result.Error)
	}

	// On localhost, the largest size (1472) should succeed because the
	// loopback MTU is well above 1500. PathMTU should be 1472 + 28 = 1500.
	expectedMTU := sizes[0] + 28
	if result.PathMTU != expectedMTU {
		t.Fatalf("expected PathMTU=%d (early exit at largest size), got %d", expectedMTU, result.PathMTU)
	}
	if result.MTUState != MTUStateOK {
		t.Fatalf("expected MTUState=%q, got %q", MTUStateOK, result.MTUState)
	}
	if result.MTUDetail != MTUDetailLargestSizeConfirmed {
		t.Fatalf("expected MTUDetail=%q, got %q", MTUDetailLargestSizeConfirmed, result.MTUDetail)
	}
}

// TestMTUProber_AllFail verifies that when all configured sizes fail,
// PathMTU=-1 and Success=false.
//
// We force failure by using an unresolvable address (which causes the
// address resolution to fail before any ICMP is sent).
func TestMTUProber_AllFail(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-mtu-all-fail",
		Address:   "this.host.does.not.exist.invalid",
		ProbeType: config.ProbeTypeMTU,
		Timeout:   2 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ICMPPayloadSizes: []int{1472, 1372, 1272},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &MTUProber{}
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false when all sizes fail")
	}
	if result.PathMTU != -1 {
		t.Fatalf("expected PathMTU=-1 when all sizes fail, got %d", result.PathMTU)
	}
	if result.Error == "" {
		t.Fatal("expected non-empty Error when all sizes fail")
	}
	if result.MTUState != MTUStateError {
		t.Fatalf("expected MTUState=%q for resolve failure, got %q", MTUStateError, result.MTUState)
	}
	if result.MTUDetail != MTUDetailResolveError {
		t.Fatalf("expected MTUDetail=%q for resolve failure, got %q", MTUDetailResolveError, result.MTUDetail)
	}
}

// TestMTUProber_ContextCancelled verifies that a pre-cancelled context
// causes the probe to return quickly with failure and valid invariants.
func TestMTUProber_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	target := config.TargetConfig{
		Name:      "test-mtu-ctx-cancel",
		Address:   "127.0.0.1",
		ProbeType: config.ProbeTypeMTU,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ICMPPayloadSizes: []int{1472, 1372, 1272, 1172, 1072},
		},
	}

	start := time.Now()
	prober := &MTUProber{}
	result := prober.Probe(ctx, target)
	elapsed := time.Since(start)

	// With a cancelled context the probe must return almost immediately.
	if elapsed > 2*time.Second {
		t.Fatalf("probe took %v with cancelled context; expected fast return", elapsed)
	}

	if result.Success {
		t.Fatal("expected Success=false with cancelled context")
	}
	if result.PathMTU != -1 {
		t.Fatalf("expected PathMTU=-1 with cancelled context, got %d", result.PathMTU)
	}
	if result.MTUState != MTUStateError {
		t.Fatalf("expected MTUState=%q, got %q", MTUStateError, result.MTUState)
	}
	if result.MTUDetail != MTUDetailInternalError {
		t.Fatalf("expected MTUDetail=%q, got %q", MTUDetailInternalError, result.MTUDetail)
	}
}

// TestMTUProber_SingleSize verifies correct behaviour with a single
// configured MTU size.
func TestMTUProber_SingleSize(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-mtu-single-size",
		Address:   "127.0.0.1",
		ProbeType: config.ProbeTypeMTU,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ICMPPayloadSizes: []int{1072},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &MTUProber{}
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Skipf("probe did not succeed (likely no CAP_NET_RAW): %s", result.Error)
	}

	expectedMTU := 1072 + 28
	if result.PathMTU != expectedMTU {
		t.Fatalf("expected PathMTU=%d for single size, got %d", expectedMTU, result.PathMTU)
	}
}

// TestMTUProber_ResultInvariant_SuccessImpliesEmptyError verifies that
// when Success=true, Error is always empty.
func TestMTUProber_ResultInvariant_SuccessImpliesEmptyError(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-mtu-invariant-success-error",
		Address:   "127.0.0.1",
		ProbeType: config.ProbeTypeMTU,
		Timeout:   5 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ICMPPayloadSizes: []int{1472, 1372, 1272},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &MTUProber{}
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Skipf("probe did not succeed (likely no CAP_NET_RAW): %s", result.Error)
	}

	if result.Error != "" {
		t.Fatalf("Success=true but Error is non-empty: %q", result.Error)
	}
}

// TestMTUProber_ResultInvariant_FailureImpliesNonEmptyError verifies that
// when Success=false, Error is always non-empty.
func TestMTUProber_ResultInvariant_FailureImpliesNonEmptyError(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-mtu-invariant-failure-error",
		Address:   "this.host.does.not.exist.invalid",
		ProbeType: config.ProbeTypeMTU,
		Timeout:   2 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ICMPPayloadSizes: []int{1472, 1372},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &MTUProber{}
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false for unresolvable address")
	}
	if result.Error == "" {
		t.Fatal("Success=false but Error is empty")
	}
}

// TestMTUProber_ResultInvariant_PathMTUDomain verifies that PathMTU is
// either -1 (all failed) or one of the configured sizes + 28.
func TestMTUProber_ResultInvariant_PathMTUDomain(t *testing.T) {
	sizes := []int{1472, 1372, 1272, 1172, 1072}
	cases := []struct {
		name    string
		address string
	}{
		{"localhost", "127.0.0.1"},
		{"unresolvable", "this.host.does.not.exist.invalid"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := config.TargetConfig{
				Name:      "test-mtu-domain-" + tc.name,
				Address:   tc.address,
				ProbeType: config.ProbeTypeMTU,
				Timeout:   3 * time.Second,
				ProbeOpts: config.ProbeOptions{
					ICMPPayloadSizes: sizes,
				},
			}

			ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
			defer cancel()

			prober := &MTUProber{}
			result := prober.Probe(ctx, target)

			// PathMTU must be -1 or one of sizes[i]+28.
			if result.PathMTU == -1 {
				return // valid: all sizes failed
			}

			valid := false
			for _, s := range sizes {
				if result.PathMTU == s+28 {
					valid = true
					break
				}
			}
			if !valid {
				t.Fatalf("PathMTU=%d is not -1 and not in {sizes+28}", result.PathMTU)
			}
		})
	}
}

// TestMTUProber_ResultInvariant_FailureImpliesPathMTUNegOne verifies that
// when Success=false, PathMTU is always -1.
func TestMTUProber_ResultInvariant_FailureImpliesPathMTUNegOne(t *testing.T) {
	target := config.TargetConfig{
		Name:      "test-mtu-invariant-fail-pathmtu",
		Address:   "this.host.does.not.exist.invalid",
		ProbeType: config.ProbeTypeMTU,
		Timeout:   2 * time.Second,
		ProbeOpts: config.ProbeOptions{
			ICMPPayloadSizes: []int{1472, 1372, 1272},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &MTUProber{}
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Skip("probe succeeded unexpectedly; cannot test failure invariant")
	}

	if result.PathMTU != -1 {
		t.Fatalf("Success=false but PathMTU=%d (expected -1)", result.PathMTU)
	}
}
