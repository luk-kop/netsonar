package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"netsonar/internal/config"
)

// TestHTTPProber_ConcurrentProbesNoRace runs many goroutines invoking
// Probe() on the same prober with different per-target DNSResolver values.
// The prober now allocates the *http.Client and *http.Transport per call so
// concurrent invocations cannot share Transport state. This test catches
// regressions where the per-target resolver setting bleeds across probes
// (or worse, races against itself) by running under -race.
//
// Run with: go test -race ./internal/probe -run TestHTTPProber_ConcurrentProbesNoRace
func TestHTTPProber_ConcurrentProbesNoRace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	prober := NewHTTPProber(false, true, "")

	const goroutines = 32
	const probesPerGoroutine = 8
	resolvers := []*string{
		// Inherit / system resolver (effective: net.DefaultResolver).
		ptrString(""),
		// Pinned resolver. We only exercise the construction path; the
		// HTTP probe target is a literal IP (httptest binds 127.0.0.1)
		// so no actual DNS query goes to this address. The point is to
		// verify that the per-call Transport built from this value does
		// not race with concurrent calls using a different value.
		ptrString("8.8.8.8:53"),
		ptrString("1.1.1.1:53"),
	}

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			for i := 0; i < probesPerGoroutine; i++ {
				resolver := resolvers[(g+i)%len(resolvers)]
				target := config.TargetConfig{
					Name:        "race-target",
					Address:     srv.URL,
					ProbeType:   config.ProbeTypeHTTP,
					Timeout:     2 * time.Second,
					DNSResolver: resolver,
				}
				ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
				result := prober.Probe(ctx, target)
				cancel()
				if !result.Success {
					t.Errorf("probe %d/%d failed: %s", g, i, result.Error)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestHTTPBodyProber_ConcurrentProbesNoRace mirrors the HTTPProber race
// test for HTTPBodyProber so the same per-call client allocation contract
// is enforced for body-validation probes.
func TestHTTPBodyProber_ConcurrentProbesNoRace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello world"))
	}))
	defer srv.Close()

	prober := NewHTTPBodyProber(false, true, "", "hello")

	const goroutines = 32
	const probesPerGoroutine = 8
	resolvers := []*string{
		ptrString(""),
		ptrString("8.8.8.8:53"),
		ptrString("1.1.1.1:53"),
	}

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			for i := 0; i < probesPerGoroutine; i++ {
				resolver := resolvers[(g+i)%len(resolvers)]
				target := config.TargetConfig{
					Name:        "race-body-target",
					Address:     srv.URL,
					ProbeType:   config.ProbeTypeHTTPBody,
					Timeout:     2 * time.Second,
					DNSResolver: resolver,
					ProbeOpts: config.ProbeOptions{
						BodyMatchString: "hello",
					},
				}
				ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
				result := prober.Probe(ctx, target)
				cancel()
				if !result.Success {
					t.Errorf("probe %d/%d failed: %s", g, i, result.Error)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestHTTPProber_PerCallResolverWiredIntoTransport asserts that the Dial
// callback observed during a probe receives the per-target DNS resolver
// rather than the default one. We stand up a mock UDP DNS listener on
// loopback and verify it observes a DNS query when the target hostname
// is resolved through the per-target resolver.
//
// The probe target is a hostname (so a DNS lookup is forced). The
// underlying lookup eventually fails because our mock returns nothing,
// but that is fine — we are asserting on the *path* taken (resolver was
// consulted), not on probe success.
func TestHTTPProber_PerCallResolverWiredIntoTransport(t *testing.T) {
	// Bind a UDP socket on loopback. Any DNS query routed here is
	// recorded in `observedQueries`. We do not respond, so the lookup
	// fails after the resolver's internal retry budget — exactly what
	// we want for the assertion.
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer func() { _ = pc.Close() }()

	var queryCount atomic.Int32
	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if n > 0 {
				queryCount.Add(1)
			}
		}
	}()

	resolverAddr := pc.LocalAddr().String()

	prober := NewHTTPProber(false, true, "")
	target := config.TargetConfig{
		Name:        "resolver-pin",
		Address:     "http://example.invalid/",
		ProbeType:   config.ProbeTypeHTTP,
		Timeout:     1500 * time.Millisecond,
		DNSResolver: ptrString(resolverAddr),
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	result := prober.Probe(ctx, target)
	if result.Success {
		t.Fatal("expected probe to fail because mock resolver does not respond")
	}
	if queryCount.Load() == 0 {
		t.Fatalf("mock resolver received zero DNS queries; per-target dns_resolver was not honored. probe error: %s", result.Error)
	}
}

// ptrString is a local helper mirroring config_test.go's `ptr`. Internal
// to the probe package so the resolver test does not need to depend on
// the config-package test helper.
func ptrString(s string) *string { return &s }
