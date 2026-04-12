// Package probe — MTUProber implementation.
package probe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"

	"netsonar/internal/config"
)

// MTUProber detects the path MTU to a target by sending ICMP echo requests
// with the Don't Fragment (DF) bit set, stepping down through configured
// payload sizes until one succeeds.
type MTUProber struct{}

var icmpIDCounter atomic.Uint32

const (
	mtuSanityPayloadSize = 64
	mtuSanitySeq         = 0
)

type mtuPayloadStatus string

const (
	mtuPayloadSuccess       mtuPayloadStatus = "success"
	mtuPayloadTooLarge      mtuPayloadStatus = "too_large"
	mtuPayloadLocalTooLarge mtuPayloadStatus = "local_too_large"
	mtuPayloadTimeout       mtuPayloadStatus = "timeout"
	mtuPayloadUnreachable   mtuPayloadStatus = "unreachable"
	mtuPayloadError         mtuPayloadStatus = "error"
)

type mtuPayloadResult struct {
	status      mtuPayloadStatus
	payloadSize int
	err         error
}

type mtuClassification struct {
	state   string
	detail  string
	pathMTU int
	success bool
}

type mtuProbeStats struct {
	fragNeededCount int
	timeoutCount    int
	retryCount      int
	localErrorCount int
}

func nextICMPID() int {
	id := icmpIDCounter.Add(1) & 0xffff
	if id == 0 {
		id = icmpIDCounter.Add(1) & 0xffff
	}
	return int(id)
}

// Probe executes an MTU/PMTUD probe against target.Address.
//
// Preconditions:
//   - target.Address is a valid hostname or IP address (no port)
//   - target.ProbeOpts.ICMPPayloadSizes is sorted descending and non-empty
//   - The process has CAP_NET_RAW capability or runs as root
//
// Postconditions:
//   - result.PathMTU = largest_successful_payload + 28 (IP + ICMP headers)
//   - If no payload size succeeds: result.PathMTU = -1 and result.Success = false
//   - result.Success is true if at least one payload size succeeded
//   - Tests are executed in descending order; stops at first success
//   - All ICMP sockets are closed before returning
//   - result.Error is non-empty when Success is false
//
// Loop invariant: all sizes[0..i-1] have failed (packet too large or unreachable)
func (p *MTUProber) Probe(ctx context.Context, target config.TargetConfig) ProbeResult {
	var result ProbeResult
	result.PathMTU = -1

	sizes := target.ProbeOpts.ICMPPayloadSizes
	if len(sizes) == 0 {
		result.Error = "no icmp_payload_sizes configured"
		result.MTUState = MTUStateError
		result.MTUDetail = MTUDetailInternalError
		return result
	}

	// Resolve the target address to an IPv4 address.
	dst, err := net.ResolveIPAddr("ip4", target.Address)
	if err != nil {
		result.Error = fmt.Sprintf("resolve IPv4 address: %s", err)
		result.MTUState = MTUStateError
		result.MTUDetail = MTUDetailResolveError
		return result
	}

	start := time.Now()
	payloadResults := make([]mtuPayloadResult, 0, len(sizes))
	icmpID := nextICMPID()
	retries := effectiveMTURetries(target.ProbeOpts.MTURetries)
	perAttemptTimeout := effectiveMTUPerAttemptTimeout(target.ProbeOpts.MTUPerAttemptTimeout)
	var stats mtuProbeStats

	sanity := p.probeSmallICMPEchoWithRetries(ctx, dst, perAttemptTimeout, retries, icmpID, &stats)
	if sanity.status == mtuPayloadTimeout && ctx.Err() != nil {
		setMTUInconclusive(&result, start, ctx.Err())
		applyMTUStats(&result, stats)
		return result
	}
	if handled := applySanityFailure(&result, sanity, start); handled {
		applyMTUStats(&result, stats)
		return result
	}

	// Step down through configured sizes (descending). Stop at first success.
	for _, payloadSize := range sizes {
		if ctx.Err() != nil {
			setMTUInconclusive(&result, start, ctx.Err())
			applyMTUStats(&result, stats)
			return result
		}

		payloadResult := p.probePayloadWithDFRetries(ctx, dst, payloadSize, icmpID, perAttemptTimeout, retries, &stats)
		payloadResults = append(payloadResults, payloadResult)
		if payloadResult.status == mtuPayloadTimeout && ctx.Err() != nil {
			setMTUInconclusive(&result, start, ctx.Err())
			applyMTUStats(&result, stats)
			return result
		}

		switch payloadResult.status {
		case mtuPayloadSuccess:
			classification := classifyMTUResult(payloadResults, target.ProbeOpts.ExpectedMinMTU)
			result.PathMTU = classification.pathMTU
			result.Success = classification.success
			result.MTUState = classification.state
			result.MTUDetail = classification.detail
			applyMTUStats(&result, stats)
			result.Duration = time.Since(start)
			return result

		case mtuPayloadUnreachable:
			result.Duration = time.Since(start)
			result.Error = "destination unreachable"
			result.MTUState = MTUStateUnreachable
			result.MTUDetail = MTUDetailDestinationUnreach
			applyMTUStats(&result, stats)
			return result

		case mtuPayloadError:
			result.Duration = time.Since(start)
			if payloadResult.err != nil && os.IsPermission(payloadResult.err) {
				result.Error = "permission denied: CAP_NET_RAW required"
				result.MTUState = MTUStateError
				result.MTUDetail = MTUDetailPermissionDenied
				applyMTUStats(&result, stats)
				return result
			}
			errText := "unknown error"
			if payloadResult.err != nil {
				errText = payloadResult.err.Error()
			}
			result.Error = fmt.Sprintf("mtu probe error: %s", errText)
			result.MTUState = MTUStateError
			result.MTUDetail = MTUDetailInternalError
			applyMTUStats(&result, stats)
			return result

		case mtuPayloadTooLarge, mtuPayloadLocalTooLarge, mtuPayloadTimeout:
			continue
		}
	}

	// All sizes failed.
	classification := classifyMTUResult(payloadResults, target.ProbeOpts.ExpectedMinMTU)
	result.PathMTU = classification.pathMTU
	result.Success = classification.success
	result.MTUState = classification.state
	result.MTUDetail = classification.detail
	applyMTUStats(&result, stats)
	result.Duration = time.Since(start)
	result.Error = "all MTU sizes failed — target unreachable or path MTU below minimum test size"
	return result
}

func applyMTUStats(result *ProbeResult, stats mtuProbeStats) {
	result.MTUFragNeededCount = stats.fragNeededCount
	result.MTUTimeoutCount = stats.timeoutCount
	result.MTURetryCount = stats.retryCount
	result.MTULocalErrorCount = stats.localErrorCount
}

func effectiveMTURetries(retries int) int {
	if retries < 1 {
		return config.DefaultMTURetries
	}
	return retries
}

func effectiveMTUPerAttemptTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return config.DefaultMTUPerAttemptTimeout
	}
	return timeout
}

func (p *MTUProber) probeSmallICMPEchoWithRetries(
	ctx context.Context,
	dst *net.IPAddr,
	perAttemptTimeout time.Duration,
	retries int,
	icmpID int,
	stats *mtuProbeStats,
) mtuPayloadResult {
	attempts := make([]mtuPayloadResult, 0, retries)
	for attempt := 0; attempt < retries; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, perAttemptTimeout)
		result := p.probeSmallICMPEcho(attemptCtx, dst, mtuSanityPayloadSize, icmpID, mtuSanitySeq)
		cancel()
		attempts = append(attempts, result)
		if result.status == mtuPayloadSuccess || result.status == mtuPayloadUnreachable || result.status == mtuPayloadError {
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	countAttemptStats(attempts, stats)
	return classifyPayloadAttempts(attempts)
}

func (p *MTUProber) probePayloadWithDFRetries(
	ctx context.Context,
	dst *net.IPAddr,
	payloadSize int,
	icmpID int,
	perAttemptTimeout time.Duration,
	retries int,
	stats *mtuProbeStats,
) mtuPayloadResult {
	attempts := make([]mtuPayloadResult, 0, retries)
	for attempt := 0; attempt < retries; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, perAttemptTimeout)
		result := p.probePayloadWithDF(attemptCtx, dst, payloadSize, icmpID)
		cancel()
		attempts = append(attempts, result)
		if result.status == mtuPayloadSuccess || result.status == mtuPayloadUnreachable || result.status == mtuPayloadError {
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	countAttemptStats(attempts, stats)
	return classifyPayloadAttempts(attempts)
}

func countAttemptStats(attempts []mtuPayloadResult, stats *mtuProbeStats) {
	if stats == nil {
		return
	}
	for i, attempt := range attempts {
		if i > 0 {
			stats.retryCount++
		}
		switch attempt.status {
		case mtuPayloadTooLarge:
			stats.fragNeededCount++
		case mtuPayloadTimeout:
			stats.timeoutCount++
		case mtuPayloadLocalTooLarge:
			stats.localErrorCount++
		}
	}
}

func classifyPayloadAttempts(attempts []mtuPayloadResult) mtuPayloadResult {
	if len(attempts) == 0 {
		return mtuPayloadResult{status: mtuPayloadTimeout}
	}
	payloadSize := attempts[0].payloadSize
	for _, attempt := range attempts {
		if attempt.status == mtuPayloadSuccess {
			return attempt
		}
	}
	for _, attempt := range attempts {
		if attempt.status == mtuPayloadUnreachable {
			return attempt
		}
	}
	for _, attempt := range attempts {
		if attempt.status == mtuPayloadError {
			return attempt
		}
	}
	for _, attempt := range attempts {
		if attempt.status == mtuPayloadLocalTooLarge {
			return attempt
		}
	}
	for _, attempt := range attempts {
		if attempt.status == mtuPayloadTooLarge {
			return attempt
		}
	}
	return mtuPayloadResult{status: mtuPayloadTimeout, payloadSize: payloadSize}
}

func applySanityFailure(result *ProbeResult, sanity mtuPayloadResult, start time.Time) bool {
	switch sanity.status {
	case mtuPayloadSuccess:
		return false
	case mtuPayloadUnreachable:
		result.Duration = time.Since(start)
		result.PathMTU = -1
		result.Error = "sanity check destination unreachable"
		result.MTUState = MTUStateUnreachable
		result.MTUDetail = MTUDetailDestinationUnreach
		return true
	case mtuPayloadError:
		result.Duration = time.Since(start)
		result.PathMTU = -1
		if sanity.err != nil && os.IsPermission(sanity.err) {
			result.Error = "permission denied: CAP_NET_RAW required"
			result.MTUState = MTUStateError
			result.MTUDetail = MTUDetailPermissionDenied
			return true
		}
		errText := "unknown error"
		if sanity.err != nil {
			errText = sanity.err.Error()
		}
		result.Error = fmt.Sprintf("mtu sanity check error: %s", errText)
		result.MTUState = MTUStateError
		result.MTUDetail = MTUDetailInternalError
		return true
	case mtuPayloadTimeout:
		result.Duration = time.Since(start)
		result.PathMTU = -1
		result.Error = "sanity check failed"
		result.MTUState = MTUStateUnreachable
		result.MTUDetail = MTUDetailSanityCheckFailed
		return true
	default:
		result.Duration = time.Since(start)
		result.PathMTU = -1
		result.Error = "sanity check failed"
		result.MTUState = MTUStateUnreachable
		result.MTUDetail = MTUDetailSanityCheckFailed
		return true
	}
}

func setMTUInconclusive(result *ProbeResult, start time.Time, err error) {
	result.Duration = time.Since(start)
	result.PathMTU = -1
	result.Success = false
	result.MTUState = MTUStateDegraded
	result.MTUDetail = MTUDetailInconclusive
	if err != nil {
		result.Error = fmt.Sprintf("mtu probe inconclusive: %s", err)
	} else {
		result.Error = "mtu probe inconclusive"
	}
}

func classifyMTUResult(results []mtuPayloadResult, expectedMinMTU int) mtuClassification {
	classification := mtuClassification{
		state:   MTUStateDegraded,
		detail:  MTUDetailAllSizesTimedOut,
		pathMTU: -1,
	}

	successIndex := -1
	for i, result := range results {
		if result.status == mtuPayloadSuccess {
			successIndex = i
			classification.pathMTU = result.payloadSize + 28
			classification.success = true
			break
		}
	}

	if successIndex >= 0 {
		classification.state = MTUStateOK
		if expectedMinMTU > 0 && classification.pathMTU < expectedMinMTU {
			classification.state = MTUStateDegraded
		}
		classification.detail = detailForPriorPayloadFailures(results[:successIndex])
		return classification
	}

	classification.detail = detailForFailedPayloads(results)
	return classification
}

func detailForPriorPayloadFailures(results []mtuPayloadResult) string {
	switch {
	case hasPayloadStatus(results, mtuPayloadLocalTooLarge):
		return MTUDetailLocalMessageTooLarge
	case hasPayloadStatus(results, mtuPayloadTooLarge):
		return MTUDetailFragmentationNeeded
	case hasPayloadStatus(results, mtuPayloadTimeout):
		return MTUDetailLargerSizesTimedOut
	default:
		return MTUDetailLargestSizeConfirmed
	}
}

func detailForFailedPayloads(results []mtuPayloadResult) string {
	switch {
	case hasPayloadStatus(results, mtuPayloadLocalTooLarge):
		return MTUDetailLocalMessageTooLarge
	case hasPayloadStatus(results, mtuPayloadTooLarge):
		return MTUDetailBelowMinTested
	default:
		return MTUDetailAllSizesTimedOut
	}
}

func hasPayloadStatus(results []mtuPayloadResult, status mtuPayloadStatus) bool {
	for _, result := range results {
		if result.status == status {
			return true
		}
	}
	return false
}

func (p *MTUProber) probeSmallICMPEcho(
	ctx context.Context,
	dst *net.IPAddr,
	payloadSize int,
	icmpID int,
	seq int,
) mtuPayloadResult {
	result := mtuPayloadResult{
		status:      mtuPayloadError,
		payloadSize: payloadSize,
	}

	rawPC, err := net.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		result.err = err
		return result
	}
	defer func() { _ = rawPC.Close() }()

	msg := &icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{
			ID:   icmpID,
			Seq:  seq,
			Data: make([]byte, payloadSize),
		},
	}

	msgBytes, err := msg.Marshal(nil)
	if err != nil {
		result.err = fmt.Errorf("marshal icmp: %w", err)
		return result
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	if err := rawPC.SetDeadline(deadline); err != nil {
		result.err = fmt.Errorf("set deadline: %w", err)
		return result
	}

	if _, err := rawPC.WriteTo(msgBytes, dst); err != nil {
		result.err = err
		return result
	}

	readBuf := make([]byte, 1500)
	for {
		if ctx.Err() != nil {
			result.status = mtuPayloadTimeout
			return result
		}

		n, addr, err := rawPC.ReadFrom(readBuf)
		if err != nil {
			result.status = mtuPayloadTimeout
			result.err = err
			return result
		}

		reply, err := icmp.ParseMessage(1, readBuf[:n])
		if err != nil {
			continue
		}

		if reply.Type == ipv4.ICMPTypeDestinationUnreachable {
			dstUnreach, ok := reply.Body.(*icmp.DstUnreach)
			if !ok || !matchesEmbeddedICMP(dstUnreach.Data, dst.IP, icmpID, seq) {
				continue
			}
			result.status = mtuPayloadUnreachable
			return result
		}

		if reply.Type != ipv4.ICMPTypeEchoReply {
			continue
		}
		if !packetAddrMatchesIP(addr, dst.IP) {
			continue
		}
		echo, ok := reply.Body.(*icmp.Echo)
		if !ok {
			continue
		}
		if echo.ID != icmpID || echo.Seq != seq {
			continue
		}

		result.status = mtuPayloadSuccess
		return result
	}
}

// probePayloadWithDF sends a single ICMP echo request with the Don't Fragment
// bit set and waits for a reply. It classifies the observed result for this
// payload size; it does not retry.
//
// Preconditions:
//   - dst is a valid resolved IPv4 address
//   - payloadSize > 0
//   - ctx carries the probe timeout
//
// Postconditions:
//   - The ICMP socket is always closed before returning
//   - Returns mtuPayloadSuccess only if an echo reply matching our ID/seq is received
func (p *MTUProber) probePayloadWithDF(
	ctx context.Context,
	dst *net.IPAddr,
	payloadSize int,
	icmpID int,
) mtuPayloadResult {
	result := mtuPayloadResult{
		status:      mtuPayloadError,
		payloadSize: payloadSize,
	}

	// Open a privileged raw ICMP socket (requires CAP_NET_RAW).
	// We use net.ListenPacket directly (instead of icmp.ListenPacket) so
	// that we get a *net.IPConn which implements syscall.Conn, allowing
	// us to set the IP_MTU_DISCOVER socket option for the DF bit.
	rawPC, err := net.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		result.err = err
		return result
	}
	defer func() { _ = rawPC.Close() }()

	// Set the Don't Fragment bit via IP_MTU_DISCOVER socket option.
	// IP_PMTUDISC_PROBE sends DF probes without being blocked by cached PMTU.
	sc, ok := rawPC.(interface {
		SyscallConn() (syscall.RawConn, error)
	})
	if !ok {
		result.err = fmt.Errorf("packet conn does not support SyscallConn")
		return result
	}
	rawConn, err := sc.SyscallConn()
	if err != nil {
		result.err = fmt.Errorf("get raw conn: %w", err)
		return result
	}

	var sockErr error
	err = rawConn.Control(func(fd uintptr) {
		sockErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_MTU_DISCOVER, syscall.IP_PMTUDISC_PROBE)
	})
	if err != nil {
		result.err = fmt.Errorf("raw conn control: %w", err)
		return result
	}
	if sockErr != nil {
		result.err = fmt.Errorf("set IP_MTU_DISCOVER: %w", sockErr)
		return result
	}

	// Build the ICMP echo request with the specified payload size.
	seq := payloadSize // Use payload size as sequence number for easy identification.

	msg := &icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{
			ID:   icmpID,
			Seq:  seq,
			Data: make([]byte, payloadSize),
		},
	}

	msgBytes, err := msg.Marshal(nil)
	if err != nil {
		result.err = fmt.Errorf("marshal icmp: %w", err)
		return result
	}

	// Derive deadline from context.
	deadline, ok2 := ctx.Deadline()
	if !ok2 {
		deadline = time.Now().Add(5 * time.Second)
	}
	if err := rawPC.SetDeadline(deadline); err != nil {
		result.err = fmt.Errorf("set deadline: %w", err)
		return result
	}

	// Send the echo request.
	if _, err := rawPC.WriteTo(msgBytes, dst); err != nil {
		if errors.Is(err, syscall.EMSGSIZE) {
			result.status = mtuPayloadLocalTooLarge
			result.err = err
			return result
		}
		result.err = err
		return result
	}

	// Read replies until we find our echo reply or the deadline expires.
	readBuf := make([]byte, 1500)
	for {
		if ctx.Err() != nil {
			result.status = mtuPayloadTimeout
			return result
		}

		n, addr, err := rawPC.ReadFrom(readBuf)
		if err != nil {
			// Timeout or read error — this size didn't work.
			result.status = mtuPayloadTimeout
			result.err = err
			return result
		}

		reply, err := icmp.ParseMessage(1, readBuf[:n]) // protocol 1 = ICMPv4
		if err != nil {
			continue
		}

		// Check for "Destination Unreachable" with code 4 (Fragmentation Needed).
		// This means the packet was too large for the path.
		if reply.Type == ipv4.ICMPTypeDestinationUnreachable {
			dstUnreach, ok := reply.Body.(*icmp.DstUnreach)
			if !ok || !matchesEmbeddedICMP(dstUnreach.Data, dst.IP, icmpID, seq) {
				continue
			}
			if reply.Code == 4 {
				result.status = mtuPayloadTooLarge
				return result
			}
			result.status = mtuPayloadUnreachable
			return result
		}

		// Accept echo replies matching our ID and sequence.
		if reply.Type != ipv4.ICMPTypeEchoReply {
			continue
		}
		if !packetAddrMatchesIP(addr, dst.IP) {
			continue
		}
		echo, ok := reply.Body.(*icmp.Echo)
		if !ok {
			continue
		}
		if echo.ID != icmpID || echo.Seq != seq {
			continue
		}

		result.status = mtuPayloadSuccess
		return result
	}
}

func packetAddrMatchesIP(addr net.Addr, ip net.IP) bool {
	ipAddr, ok := addr.(*net.IPAddr)
	if !ok {
		return false
	}
	return ipAddr.IP.Equal(ip)
}

func matchesEmbeddedICMP(data []byte, dstIP net.IP, icmpID, seq int) bool {
	dst4 := dstIP.To4()
	if dst4 == nil || len(data) < 20 {
		return false
	}

	version := data[0] >> 4
	if version != 4 {
		return false
	}

	ihl := int(data[0]&0x0f) * 4
	if ihl < 20 || len(data) < ihl+8 {
		return false
	}

	if data[9] != 1 { // ICMPv4
		return false
	}
	if !net.IP(data[16:20]).Equal(dst4) {
		return false
	}
	if data[ihl] != byte(ipv4.ICMPTypeEcho) || data[ihl+1] != 0 {
		return false
	}

	embeddedID := int(binary.BigEndian.Uint16(data[ihl+4 : ihl+6]))
	embeddedSeq := int(binary.BigEndian.Uint16(data[ihl+6 : ihl+8]))
	return embeddedID == icmpID && embeddedSeq == seq
}
