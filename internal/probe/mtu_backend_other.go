//go:build !linux

package probe

import (
	"context"
	"fmt"
	"net"
)

type unsupportedMTUBackend struct{}

func defaultMTUBackend() mtuProbeBackend {
	return unsupportedMTUBackend{}
}

func (unsupportedMTUBackend) probeSmallEcho(
	context.Context,
	*net.IPAddr,
	int,
	int,
) mtuPayloadResult {
	return mtuPayloadResult{
		status: mtuPayloadError,
		err:    fmt.Errorf("mtu probing is supported on Linux only"),
	}
}

func (unsupportedMTUBackend) probePayloadWithDF(
	context.Context,
	*net.IPAddr,
	int,
	int,
) mtuPayloadResult {
	return mtuPayloadResult{
		status: mtuPayloadError,
		err:    fmt.Errorf("mtu probing is supported on Linux only"),
	}
}
