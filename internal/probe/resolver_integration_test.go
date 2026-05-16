// Package probe — integration tests verifying per-target DNS resolver
// override is honored across all prober implementations. The pattern is:
// stand up a mock UDP DNS listener on loopback, configure target.DNSResolver
// to point at it, and assert the listener received at least one query.
//
// We do not implement a full DNS responder — the mock simply observes
// traffic. This is sufficient because we are asserting on the *path* taken
// (which resolver was consulted), not on probe success.
package probe

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"netsonar/internal/config"
)

// mockDNSListener returns a UDP listener on loopback that records every
// incoming query. It does not respond, so any lookup routed through it
// fails after the resolver's internal retries — which is exactly what we
// want for a probe that should reach the DNS phase but not progress past
// it.
//
// The returned cleanup closes the socket; callers must call it before
// returning from the test.
type mockDNSListener struct {
	addr    string
	queries *atomic.Int32
	cleanup func()
}

func newMockDNSListener(t *testing.T) *mockDNSListener {
	t.Helper()
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}

	var queryCount atomic.Int32
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err := pc.ReadFrom(buf)
			if err != nil {
				close(done)
				return
			}
			if n > 0 {
				queryCount.Add(1)
			}
		}
	}()

	return &mockDNSListener{
		addr:    pc.LocalAddr().String(),
		queries: &queryCount,
		cleanup: func() {
			_ = pc.Close()
			<-done
		},
	}
}

// TestTCPProber_HonorsTargetDNSResolver verifies that the per-target
// resolver is wired into the TCP probe's DNS phase. The mock receives the
// query for the hostname target.
func TestTCPProber_HonorsTargetDNSResolver(t *testing.T) {
	mock := newMockDNSListener(t)
	defer mock.cleanup()

	prober := &TCPProber{}
	target := config.TargetConfig{
		Name:        "tcp-mock-resolver",
		Address:     "host.invalid:443",
		ProbeType:   config.ProbeTypeTCP,
		Timeout:     1 * time.Second,
		DNSResolver: ptrString(mock.addr),
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	result := prober.Probe(ctx, target)
	if result.Success {
		t.Fatal("expected failure: mock resolver does not respond")
	}
	if mock.queries.Load() == 0 {
		t.Fatalf("mock resolver received zero queries; target.DNSResolver not honored. probe error: %s", result.Error)
	}
}

// TestICMPProber_HonorsTargetDNSResolver verifies that the resolver
// override is wired into the ICMP probe's pre-resolution.
func TestICMPProber_HonorsTargetDNSResolver(t *testing.T) {
	mock := newMockDNSListener(t)
	defer mock.cleanup()

	prober := &ICMPProber{}
	target := config.TargetConfig{
		Name:        "icmp-mock-resolver",
		Address:     "host.invalid",
		ProbeType:   config.ProbeTypeICMP,
		Timeout:     1 * time.Second,
		DNSResolver: ptrString(mock.addr),
		ProbeOpts: config.ProbeOptions{
			PingCount: 1,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	_ = prober.Probe(ctx, target)
	if mock.queries.Load() == 0 {
		t.Fatalf("mock resolver received zero queries; target.DNSResolver not honored")
	}
}

// TestMTUProber_HonorsTargetDNSResolver mirrors the ICMP test for MTU
// probes, which share the resolveIPv4 helper.
func TestMTUProber_HonorsTargetDNSResolver(t *testing.T) {
	mock := newMockDNSListener(t)
	defer mock.cleanup()

	prober := &MTUProber{}
	target := config.TargetConfig{
		Name:        "mtu-mock-resolver",
		Address:     "host.invalid",
		ProbeType:   config.ProbeTypeMTU,
		Timeout:     2 * time.Second,
		DNSResolver: ptrString(mock.addr),
		ProbeOpts: config.ProbeOptions{
			ICMPPayloadSizes:     []int{1472},
			ExpectedMinMTU:       1500,
			MTURetries:           1,
			MTUPerAttemptTimeout: 500 * time.Millisecond,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	_ = prober.Probe(ctx, target)
	if mock.queries.Load() == 0 {
		t.Fatalf("mock resolver received zero queries; target.DNSResolver not honored")
	}
}

// TestTLSCertProber_HonorsTargetDNSResolver verifies that the resolver
// override is wired into the TLS cert probe's TCP dial step. We use a
// hostname target so the dial requires DNS resolution.
func TestTLSCertProber_HonorsTargetDNSResolver(t *testing.T) {
	mock := newMockDNSListener(t)
	defer mock.cleanup()

	prober := &TLSCertProber{}
	target := config.TargetConfig{
		Name:        "tlscert-mock-resolver",
		Address:     "host.invalid:443",
		ProbeType:   config.ProbeTypeTLSCert,
		Timeout:     1 * time.Second,
		DNSResolver: ptrString(mock.addr),
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	result := prober.Probe(ctx, target)
	if result.Success {
		t.Fatal("expected failure: mock resolver does not respond")
	}
	if mock.queries.Load() == 0 {
		t.Fatalf("mock resolver received zero queries; target.DNSResolver not honored. probe error: %s", result.Error)
	}
}

// TestHTTPBodyProber_HonorsTargetDNSResolver mirrors the HTTP race test's
// resolver assertion for body-validation probes.
func TestHTTPBodyProber_HonorsTargetDNSResolver(t *testing.T) {
	mock := newMockDNSListener(t)
	defer mock.cleanup()

	prober := NewHTTPBodyProber(false, true, "", "")
	target := config.TargetConfig{
		Name:        "httpbody-mock-resolver",
		Address:     "http://host.invalid/",
		ProbeType:   config.ProbeTypeHTTPBody,
		Timeout:     1500 * time.Millisecond,
		DNSResolver: ptrString(mock.addr),
		ProbeOpts: config.ProbeOptions{
			BodyMatchString: "irrelevant",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	result := prober.Probe(ctx, target)
	if result.Success {
		t.Fatal("expected failure: mock resolver does not respond")
	}
	if mock.queries.Load() == 0 {
		t.Fatalf("mock resolver received zero queries; target.DNSResolver not honored. probe error: %s", result.Error)
	}
}

// TestHTTPProber_ProxyPath_HonorsTargetDNSResolver verifies that the
// per-target resolver is honored on the proxy code path. The proxy URL uses
// an unresolvable hostname (proxy.invalid) so the prober must consult DNS
// for the *proxy* address before any tunnel can be established; with
// target.DNSResolver pointed at the mock, the mock must observe at least
// one query.
//
// This guards the proxy-dial wiring (http_proxy_path.go, proxy_tunnel.go)
// which goes through a separate code path from the direct HTTPProber probe
// and is not otherwise exercised by the race or direct-path tests.
func TestHTTPProber_ProxyPath_HonorsTargetDNSResolver(t *testing.T) {
	mock := newMockDNSListener(t)
	defer mock.cleanup()

	prober := NewHTTPProber(false, true, "http://proxy.invalid:8080")
	target := config.TargetConfig{
		Name:        "http-via-proxy-mock-resolver",
		Address:     "http://api.example.invalid/",
		ProbeType:   config.ProbeTypeHTTP,
		Timeout:     1500 * time.Millisecond,
		DNSResolver: ptrString(mock.addr),
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	result := prober.Probe(ctx, target)
	if result.Success {
		t.Fatal("expected failure: mock resolver does not respond, proxy unreachable")
	}
	if mock.queries.Load() == 0 {
		t.Fatalf("mock resolver received zero queries; target.DNSResolver not honored on proxy path. probe error: %s", result.Error)
	}
}
