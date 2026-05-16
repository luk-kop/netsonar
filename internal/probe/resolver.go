// Package probe — DNS resolver helper shared across probe implementations.
package probe

import (
	"context"
	"net"
)

// BuildResolver returns a *net.Resolver that dials the given IP:port for
// DNS queries, bypassing the system resolver path entirely. If addr is
// empty, the system default resolver (net.DefaultResolver) is returned so
// callers can use the result unconditionally.
//
// Precondition: addr is either empty or a validated IP:port literal.
// config.validateDNSResolver guards this at config load time.
//
// The explicit Dial callback bypasses any cgo-based resolver path
// regardless of build flags. PreferGo: true is belt-and-suspenders —
// NetSonar binaries are built with CGO_ENABLED=0, which already forces the
// pure-Go resolver, but the explicit callback keeps the override correct
// independent of build tags.
func BuildResolver(addr string) *net.Resolver {
	if addr == "" {
		return net.DefaultResolver
	}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
}

// resolverFor returns the effective *net.Resolver for a target. It safely
// handles DNSResolver pointers that haven't been propagated by
// applyDefaults yet (nil → system resolver), so it can also be called from
// tests that build TargetConfig values manually.
func resolverFor(dnsResolver *string) *net.Resolver {
	if dnsResolver == nil {
		return net.DefaultResolver
	}
	return BuildResolver(*dnsResolver)
}
