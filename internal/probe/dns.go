// Package probe — DNSProber implementation.
package probe

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"netsonar/internal/config"
)

// DNSProber probes DNS resolution by resolving a configured query name and
// optionally validating the results against expected values.
type DNSProber struct{}

// Probe executes a DNS resolution against the configured query name.
//
// Preconditions:
//   - target.ProbeOpts.DNSQueryName is a non-empty valid domain name
//   - target.ProbeOpts.DNSQueryType is one of: A, AAAA, CNAME
//   - ctx carries the probe timeout (set by the scheduler)
//
// Postconditions:
//   - result.DNSResolveTime is the time taken for DNS resolution
//   - result.Success is true if DNS query returned at least one result
//   - If DNSExpectedResults is set: result includes validation against expected values
//   - result.Error contains the DNS error message if resolution failed
//   - result.Duration equals result.DNSResolveTime
func (p *DNSProber) Probe(ctx context.Context, target config.TargetConfig) ProbeResult {
	var result ProbeResult

	queryName := target.ProbeOpts.DNSQueryName
	if queryName == "" {
		queryName = target.Address
	}

	queryType := target.ProbeOpts.DNSQueryType
	if queryType == "" {
		queryType = "A"
	}

	resolver := p.buildResolver(target.ProbeOpts.DNSServer)

	start := time.Now()
	records, err := p.resolve(ctx, resolver, queryName, queryType)
	elapsed := time.Since(start)

	result.DNSResolveTime = elapsed
	result.Duration = elapsed

	if err != nil {
		result.Error = fmt.Sprintf("dns resolve: %s", err)
		return result
	}

	if len(records) == 0 {
		result.Error = "dns resolve: no results returned"
		return result
	}

	result.Success = true

	// Validate against expected results if configured.
	if len(target.ProbeOpts.DNSExpectedResults) > 0 {
		if !matchExpected(records, target.ProbeOpts.DNSExpectedResults) {
			result.Success = false
			result.Error = fmt.Sprintf(
				"dns expected result mismatch: got %v, want %v",
				records, target.ProbeOpts.DNSExpectedResults,
			)
		}
	}

	return result
}

// buildResolver returns a net.Resolver configured to use the specified DNS
// server, or the system default resolver if server is empty.
func (p *DNSProber) buildResolver(server string) *net.Resolver {
	if server == "" {
		return net.DefaultResolver
	}

	// Ensure the server address includes a port.
	if _, _, err := net.SplitHostPort(server); err != nil {
		server = net.JoinHostPort(server, "53")
	}

	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", server)
		},
	}
}

// resolve performs the DNS lookup for the given query name and type.
// It returns the resolved records as a string slice.
func (p *DNSProber) resolve(
	ctx context.Context,
	resolver *net.Resolver,
	queryName string,
	queryType string,
) ([]string, error) {
	switch queryType {
	case "A":
		ips, err := resolver.LookupIP(ctx, "ip4", queryName)
		if err != nil {
			return nil, err
		}
		results := make([]string, len(ips))
		for i, ip := range ips {
			results[i] = ip.String()
		}
		return results, nil

	case "AAAA":
		ips, err := resolver.LookupIP(ctx, "ip6", queryName)
		if err != nil {
			return nil, err
		}
		results := make([]string, len(ips))
		for i, ip := range ips {
			results[i] = ip.String()
		}
		return results, nil

	case "CNAME":
		cname, err := resolver.LookupCNAME(ctx, queryName)
		if err != nil {
			return nil, err
		}
		// Strip trailing dot for consistent comparison.
		cname = strings.TrimSuffix(cname, ".")
		if cname == "" {
			return nil, nil
		}
		return []string{cname}, nil

	default:
		return nil, fmt.Errorf("unsupported dns_query_type: %s", queryType)
	}
}

// matchExpected checks whether the resolved records match the expected values.
// Comparison is order-independent: both slices are sorted and compared element
// by element. CNAME trailing dots are stripped before comparison.
func matchExpected(got, want []string) bool {
	// Normalize: lowercase and strip trailing dots.
	normalize := func(s []string) []string {
		out := make([]string, len(s))
		for i, v := range s {
			out[i] = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(v)), ".")
		}
		sort.Strings(out)
		return out
	}

	g := normalize(got)
	w := normalize(want)

	if len(g) != len(w) {
		return false
	}
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}
