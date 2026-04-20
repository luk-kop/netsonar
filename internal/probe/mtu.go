// Package probe — MTUProber implementation.
package probe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"netsonar/internal/config"
)

// MTUProber detects the path MTU to a target by sending ICMP echo requests
// with the Don't Fragment (DF) bit set, stepping down through configured
// payload sizes until one succeeds.
type MTUProber struct {
	backend mtuProbeBackend
}

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

type mtuProbeBackend interface {
	probeSmallEcho(ctx context.Context, dst *net.IPAddr, payloadSize, seq int) mtuPayloadResult
	probePayloadWithDF(ctx context.Context, dst *net.IPAddr, payloadSize, seq int) mtuPayloadResult
}

// Probe executes an MTU/PMTUD probe against target.Address.
//
// Preconditions:
//   - target.Address is a valid hostname or IP address (no port)
//   - target.ProbeOpts.ICMPPayloadSizes is sorted descending and non-empty
//   - On Linux, net.ipv4.ping_group_range includes the process effective GID or a supplementary GID
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
	if ctx.Err() != nil {
		setMTUInconclusive(&result, start, ctx.Err())
		return result
	}

	backend := p.backend
	if backend == nil {
		backend = defaultMTUBackend()
	}
	retries := effectiveMTURetries(target.ProbeOpts.MTURetries)
	perAttemptTimeout := effectiveMTUPerAttemptTimeout(target.ProbeOpts.MTUPerAttemptTimeout)

	sanity := p.probeSmallICMPEchoWithRetries(ctx, backend, dst, perAttemptTimeout, retries)
	if sanity.status == mtuPayloadTimeout && ctx.Err() != nil {
		setMTUInconclusive(&result, start, ctx.Err())
		return result
	}
	if handled := applySanityFailure(&result, sanity, start); handled {
		return result
	}

	// Step down through configured sizes (descending). Stop at first success.
	for _, payloadSize := range sizes {
		if ctx.Err() != nil {
			setMTUInconclusive(&result, start, ctx.Err())
			return result
		}

		payloadResult := p.probePayloadWithDFRetries(ctx, backend, dst, payloadSize, perAttemptTimeout, retries)
		payloadResults = append(payloadResults, payloadResult)
		if payloadResult.status == mtuPayloadTimeout && ctx.Err() != nil {
			setMTUInconclusive(&result, start, ctx.Err())
			return result
		}

		switch payloadResult.status {
		case mtuPayloadSuccess:
			classification := classifyMTUResult(payloadResults, target.ProbeOpts.ExpectedMinMTU)
			result.PathMTU = classification.pathMTU
			result.Success = classification.success
			result.MTUState = classification.state
			result.MTUDetail = classification.detail
			result.Duration = time.Since(start)
			return result

		case mtuPayloadUnreachable:
			result.Duration = time.Since(start)
			result.Error = "destination unreachable"
			result.MTUState = MTUStateUnreachable
			result.MTUDetail = MTUDetailDestinationUnreach
			return result

		case mtuPayloadError:
			result.Duration = time.Since(start)
			if isPermissionError(payloadResult.err) {
				result.Error = mtuPermissionDeniedError()
				result.MTUState = MTUStateError
				result.MTUDetail = MTUDetailPermissionDenied
				return result
			}
			errText := "unknown error"
			if payloadResult.err != nil {
				errText = payloadResult.err.Error()
			}
			result.Error = fmt.Sprintf("mtu probe error: %s", errText)
			result.MTUState = MTUStateError
			result.MTUDetail = MTUDetailInternalError
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
	result.Duration = time.Since(start)
	result.Error = "all MTU sizes failed — target unreachable or path MTU below minimum test size"
	return result
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
	backend mtuProbeBackend,
	dst *net.IPAddr,
	perAttemptTimeout time.Duration,
	retries int,
) mtuPayloadResult {
	attempts := make([]mtuPayloadResult, 0, retries)
	for attempt := 0; attempt < retries; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, perAttemptTimeout)
		result := backend.probeSmallEcho(attemptCtx, dst, mtuSanityPayloadSize, mtuSanitySeq)
		cancel()
		attempts = append(attempts, result)
		if result.status == mtuPayloadSuccess || result.status == mtuPayloadUnreachable || result.status == mtuPayloadError {
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	return classifyPayloadAttempts(attempts)
}

func (p *MTUProber) probePayloadWithDFRetries(
	ctx context.Context,
	backend mtuProbeBackend,
	dst *net.IPAddr,
	payloadSize int,
	perAttemptTimeout time.Duration,
	retries int,
) mtuPayloadResult {
	attempts := make([]mtuPayloadResult, 0, retries)
	for attempt := 0; attempt < retries; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, perAttemptTimeout)
		result := backend.probePayloadWithDF(attemptCtx, dst, payloadSize, payloadSize)
		cancel()
		attempts = append(attempts, result)
		if result.status == mtuPayloadSuccess || result.status == mtuPayloadUnreachable || result.status == mtuPayloadError {
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	return classifyPayloadAttempts(attempts)
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
		if isPermissionError(sanity.err) {
			result.Error = mtuPermissionDeniedError()
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
			classification.success = false
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

func isPermissionError(err error) bool {
	return errors.Is(err, os.ErrPermission)
}

func mtuPermissionDeniedError() string {
	return "permission denied: check net.ipv4.ping_group_range for the process effective or supplementary GID"
}
