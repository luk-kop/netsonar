package probe

import (
	"crypto/tls"
	"net/http/httptrace"
	"sync"
	"time"
)

// httpTraceTimings collects per-phase timestamps from httptrace callbacks.
// Callbacks may fire concurrently from net/http transport goroutines while
// the probe goroutine reads the timestamps after client.Do returns; the
// mutex makes that exchange race-free.
type httpTraceTimings struct {
	mu sync.Mutex

	dnsStart, dnsEnd           time.Time
	connectStart, connectEnd   time.Time
	tlsStart, tlsEnd           time.Time
	wroteRequest, gotFirstByte time.Time
}

// httpTraceTimingsSnapshot is a value-type copy of httpTraceTimings used to
// pass timestamps out of the locked region. Carrying a sync.Mutex by value
// would be a vet error; this type intentionally has none.
type httpTraceTimingsSnapshot struct {
	dnsStart, dnsEnd           time.Time
	connectStart, connectEnd   time.Time
	tlsStart, tlsEnd           time.Time
	wroteRequest, gotFirstByte time.Time
}

func (t *httpTraceTimings) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart:             func(_ httptrace.DNSStartInfo) { t.set(&t.dnsStart) },
		DNSDone:              func(_ httptrace.DNSDoneInfo) { t.set(&t.dnsEnd) },
		ConnectStart:         func(_, _ string) { t.set(&t.connectStart) },
		ConnectDone:          func(_, _ string, _ error) { t.set(&t.connectEnd) },
		TLSHandshakeStart:    func() { t.set(&t.tlsStart) },
		TLSHandshakeDone:     func(_ tls.ConnectionState, _ error) { t.set(&t.tlsEnd) },
		WroteRequest:         func(_ httptrace.WroteRequestInfo) { t.set(&t.wroteRequest) },
		GotFirstResponseByte: func() { t.set(&t.gotFirstByte) },
	}
}

func (t *httpTraceTimings) set(field *time.Time) {
	t.mu.Lock()
	*field = time.Now()
	t.mu.Unlock()
}

func (t *httpTraceTimings) snapshot() httpTraceTimingsSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return httpTraceTimingsSnapshot{
		dnsStart: t.dnsStart, dnsEnd: t.dnsEnd,
		connectStart: t.connectStart, connectEnd: t.connectEnd,
		tlsStart: t.tlsStart, tlsEnd: t.tlsEnd,
		wroteRequest: t.wroteRequest, gotFirstByte: t.gotFirstByte,
	}
}

func buildPhasesFromSnapshot(snap httpTraceTimingsSnapshot, transferEnd time.Time) map[string]time.Duration {
	return buildPhases(
		snap.dnsStart, snap.dnsEnd,
		snap.connectStart, snap.connectEnd,
		snap.tlsStart, snap.tlsEnd,
		snap.wroteRequest, snap.gotFirstByte,
		transferEnd,
	)
}
