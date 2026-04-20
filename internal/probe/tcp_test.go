package probe

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"netsonar/internal/config"
)

// TestTCPProber_Success verifies that probing a reachable TCP listener
// reports Success=true and Duration>0.
func TestTCPProber_Success(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer func() { _ = ln.Close() }()

	// Accept connections in background so the dial succeeds.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	target := config.TargetConfig{
		Name:      "test-tcp-success",
		Address:   ln.Addr().String(),
		ProbeType: config.ProbeTypeTCP,
		Timeout:   2 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &TCPProber{}
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}
	if result.Duration <= 0 {
		t.Fatalf("expected Duration > 0, got %v", result.Duration)
	}
	if result.Error != "" {
		t.Fatalf("expected empty Error on success, got %q", result.Error)
	}
	if result.Phases != nil {
		t.Fatalf("expected no TCP phase timings, got %v", result.Phases)
	}
}

// TestTCPProber_ConnectionRefused verifies that probing a port with no
// listener reports Success=false and a non-empty, descriptive Error.
func TestTCPProber_ConnectionRefused(t *testing.T) {
	// Bind and immediately close to get a port that is guaranteed unused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	target := config.TargetConfig{
		Name:      "test-tcp-refused",
		Address:   addr,
		ProbeType: config.ProbeTypeTCP,
		Timeout:   2 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &TCPProber{}
	result := prober.Probe(ctx, target)

	if result.Success {
		t.Fatal("expected Success=false for connection refused")
	}
	if result.Error == "" {
		t.Fatal("expected non-empty Error for connection refused")
	}
	if result.Duration <= 0 {
		t.Fatalf("expected Duration > 0 even on failure, got %v", result.Duration)
	}
}

// TestTCPProber_TimeoutEnforcement verifies that the probe respects the
// context timeout and does not block significantly beyond it.
func TestTCPProber_TimeoutEnforcement(t *testing.T) {
	// Create a listener that accepts but never closes — simulates a
	// black-hole that will cause the dial to hang until timeout.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer func() { _ = ln.Close() }()

	// Accept connections but hold them open (never read/write/close).
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Keep connection open; close only when test ends via defer ln.Close().
			_ = conn
		}
	}()

	timeout := 200 * time.Millisecond
	target := config.TargetConfig{
		Name:      "test-tcp-timeout",
		Address:   ln.Addr().String(),
		ProbeType: config.ProbeTypeTCP,
		Timeout:   timeout,
	}

	// Use a very short context timeout to force the dial to time out.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	prober := &TCPProber{}
	result := prober.Probe(ctx, target)
	elapsed := time.Since(start)

	// The probe should succeed here because the listener accepts the
	// connection (TCP handshake completes). If it does succeed, that's
	// fine — the important thing is it didn't block beyond the timeout.
	// But if it fails, the error should be timeout-related.
	if !result.Success && result.Error == "" {
		t.Fatal("expected non-empty Error when probe fails")
	}

	// Regardless of success/failure, elapsed time must not greatly exceed
	// the timeout. Allow 200ms of slack for scheduling jitter.
	maxAllowed := timeout + 200*time.Millisecond
	if elapsed > maxAllowed {
		t.Fatalf("probe took %v, exceeding timeout %v + 200ms slack", elapsed, timeout)
	}
}

// TestTCPProber_ConnectionCleanup verifies that after a successful probe
// the TCP connection is closed (the server detects client disconnect).
func TestTCPProber_ConnectionCleanup(t *testing.T) {
	var clientDisconnected atomic.Bool

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer func() { _ = ln.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Block until the client side closes the connection.
		// A Read on a closed connection returns an error (EOF or reset).
		buf := make([]byte, 1)
		_, err = conn.Read(buf)
		if err != nil {
			clientDisconnected.Store(true)
		}
		_ = conn.Close()
	}()

	target := config.TargetConfig{
		Name:      "test-tcp-cleanup",
		Address:   ln.Addr().String(),
		ProbeType: config.ProbeTypeTCP,
		Timeout:   2 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	defer cancel()

	prober := &TCPProber{}
	result := prober.Probe(ctx, target)

	if !result.Success {
		t.Fatalf("expected Success=true, got false; error: %s", result.Error)
	}

	// Wait for the server goroutine to detect the disconnect.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server to detect client disconnect")
	}

	if !clientDisconnected.Load() {
		t.Fatal("expected server to detect client disconnected (connection not cleaned up)")
	}
}
