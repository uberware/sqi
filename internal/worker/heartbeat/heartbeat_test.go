// SPDX-License-Identifier: AGPL-3.0-or-later

package heartbeat

// White-box tests for the heartbeat package. Being in the same package gives
// access to the unexported natsConn interface, runWatchdog, and publish methods
// so we can exercise them directly without needing a live NATS server.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/worker/protocol"
)

// ── Stubs ─────────────────────────────────────────────────────────────────────

// stubConn implements natsConn for tests. Each method is configurable via
// function fields so individual tests only configure what they need.
type stubConn struct {
	publishFn      func(subj string, data []byte) error
	flushTimeoutFn func(timeout time.Duration) error
	isConnectedFn  func() bool
}

func (s *stubConn) Publish(subj string, data []byte) error {
	if s.publishFn != nil {
		return s.publishFn(subj, data)
	}
	return nil
}

func (s *stubConn) FlushTimeout(timeout time.Duration) error {
	if s.flushTimeoutFn != nil {
		return s.flushTimeoutFn(timeout)
	}
	return nil
}

func (s *stubConn) IsConnected() bool {
	if s.isConnectedFn != nil {
		return s.isConnectedFn()
	}
	return true
}

// stubRegistrar implements Registrar for tests. It counts Register calls and
// returns a configurable LastRegisteredAt time.
type stubRegistrar struct {
	registerCalls    atomic.Int32
	registerErr      error
	lastRegisteredAt time.Time
}

func (r *stubRegistrar) Register(_ context.Context) error {
	r.registerCalls.Add(1)
	return r.registerErr
}

func (r *stubRegistrar) LastRegisteredAt() time.Time {
	return r.lastRegisteredAt
}

// discardLogger returns a no-op slog.Logger that drops all output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(noopWriter{}, &slog.HandlerOptions{Level: slog.LevelError + 99}))
}

type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }

// newTestPublisher creates a Publisher wired with the given conn and registrar.
// The watchdog interval is set to 5 ms so tests complete quickly without
// relying on the 500 ms production default.
func newTestPublisher(nc natsConn, reg Registrar) *Publisher {
	p := New(nc, "worker-test", 4, 10*time.Millisecond, NoopStateSource{}, reg, discardLogger())
	p.watchdogInterval = 5 * time.Millisecond
	return p
}

// ── NoopStateSource ───────────────────────────────────────────────────────────

func TestNoopStateSource(t *testing.T) {
	var s NoopStateSource

	if got := s.ActiveTaskCount(); got != 0 {
		t.Errorf("ActiveTaskCount() = %d, want 0", got)
	}
	if got := s.ActiveTaskIDs(); got != nil {
		t.Errorf("ActiveTaskIDs() = %v, want nil", got)
	}
	if got := s.LastAssignmentAt(); got != nil {
		t.Errorf("LastAssignmentAt() = %v, want nil", got)
	}
}

// ── publish: field correctness ────────────────────────────────────────────────

// TestPublish_CorrectFields verifies that publish() builds a HeartbeatMsg with
// the expected constant fields and marshals it to the right NATS subject.
func TestPublish_CorrectFields(t *testing.T) {
	const workerID = "worker-abc"
	const maxTasks = 7

	var (
		publishedSubj string
		publishedData []byte
	)

	nc := &stubConn{
		publishFn: func(subj string, data []byte) error {
			publishedSubj = subj
			publishedData = data
			return nil
		},
	}
	reg := &stubRegistrar{}

	p := New(nc, workerID, maxTasks, 15*time.Second, NoopStateSource{}, reg, discardLogger())
	p.publish(context.Background())

	if publishedSubj != "worker.heartbeat" {
		t.Errorf("published to subject %q, want %q", publishedSubj, "worker.heartbeat")
	}

	var msg protocol.HeartbeatMsg
	if err := json.Unmarshal(publishedData, &msg); err != nil {
		t.Fatalf("unmarshal HeartbeatMsg: %v", err)
	}

	if msg.WorkerID != workerID {
		t.Errorf("WorkerID = %q, want %q", msg.WorkerID, workerID)
	}
	if msg.Version != protocol.ProtocolVersion {
		t.Errorf("Version = %q, want %q", msg.Version, protocol.ProtocolVersion)
	}
	if msg.Type != protocol.TypeHeartbeat {
		t.Errorf("Type = %q, want %q", msg.Type, protocol.TypeHeartbeat)
	}
	if msg.MaxConcurrentTasks != maxTasks {
		t.Errorf("MaxConcurrentTasks = %d, want %d", msg.MaxConcurrentTasks, maxTasks)
	}
	if msg.At.IsZero() {
		t.Error("At is zero, expected a recent timestamp")
	}
	if msg.UptimeSeconds < 0 {
		t.Errorf("UptimeSeconds = %f, want >= 0", msg.UptimeSeconds)
	}
	// NoopStateSource → zero active tasks, nil IDs, nil last assignment.
	if msg.ActiveTaskCount != 0 {
		t.Errorf("ActiveTaskCount = %d, want 0", msg.ActiveTaskCount)
	}
	if msg.ActiveTaskIDs != nil {
		t.Errorf("ActiveTaskIDs = %v, want nil", msg.ActiveTaskIDs)
	}
	if msg.LastAssignmentAt != nil {
		t.Errorf("LastAssignmentAt = %v, want nil", msg.LastAssignmentAt)
	}
}

// TestPublish_StateSourceFieldsForwarded verifies that values from a non-noop
// StateSource are included in the published message.
func TestPublish_StateSourceFieldsForwarded(t *testing.T) {
	lastAssign := time.Now().Add(-5 * time.Second)
	state := &fakeStateSource{
		count:            3,
		ids:              []string{"task-1", "task-2", "task-3"},
		lastAssignmentAt: &lastAssign,
	}

	var publishedData []byte
	nc := &stubConn{
		publishFn: func(_ string, data []byte) error {
			publishedData = data
			return nil
		},
	}

	p := New(nc, "w", 4, 15*time.Second, state, &stubRegistrar{}, discardLogger())
	p.publish(context.Background())

	var msg protocol.HeartbeatMsg
	if err := json.Unmarshal(publishedData, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.ActiveTaskCount != 3 {
		t.Errorf("ActiveTaskCount = %d, want 3", msg.ActiveTaskCount)
	}
	if len(msg.ActiveTaskIDs) != 3 {
		t.Errorf("len(ActiveTaskIDs) = %d, want 3", len(msg.ActiveTaskIDs))
	}
	if msg.LastAssignmentAt == nil {
		t.Fatal("LastAssignmentAt is nil, want non-nil")
	}
	// time.Time round-trips through JSON with nanosecond precision (RFC 3339);
	// allow a small tolerance for any timezone normalisation.
	diff := msg.LastAssignmentAt.Sub(lastAssign)
	if diff < 0 {
		diff = -diff
	}
	if diff > time.Second {
		t.Errorf("LastAssignmentAt round-trip error = %v, want ≤1s", diff)
	}
}

// fakeStateSource is a StateSource whose values are set at construction time.
type fakeStateSource struct {
	count            int
	ids              []string
	lastAssignmentAt *time.Time
}

func (f *fakeStateSource) ActiveTaskCount() int         { return f.count }
func (f *fakeStateSource) ActiveTaskIDs() []string      { return f.ids }
func (f *fakeStateSource) LastAssignmentAt() *time.Time { return f.lastAssignmentAt }

// ── publish: error paths ──────────────────────────────────────────────────────

// TestPublish_PublishErrorSkipsFlush verifies that a Publish failure causes the
// function to return without calling FlushTimeout.
func TestPublish_PublishErrorSkipsFlush(t *testing.T) {
	flushCalled := false
	nc := &stubConn{
		publishFn:      func(_ string, _ []byte) error { return errors.New("publish error") },
		flushTimeoutFn: func(_ time.Duration) error { flushCalled = true; return nil },
	}
	p := newTestPublisher(nc, &stubRegistrar{})
	p.publish(context.Background())

	if flushCalled {
		t.Error("FlushTimeout was called after a Publish error; expected early return")
	}
}

// TestPublish_SlowFlushLogsWarning verifies that when FlushTimeout returns an
// error (timeout), publish returns without panicking and the error is handled.
// (The actual log output is discarded; we verify the code path does not panic
// or call anything unexpected after the flush error.)
func TestPublish_SlowFlushLogsWarning(t *testing.T) {
	nc := &stubConn{
		publishFn:      func(_ string, _ []byte) error { return nil },
		flushTimeoutFn: func(_ time.Duration) error { return errors.New("flush timeout") },
	}
	reg := &stubRegistrar{}
	p := newTestPublisher(nc, reg)

	// Must not panic.
	p.publish(context.Background())

	// The flush error is a NATS problem, not a registration problem.
	if reg.registerCalls.Load() != 0 {
		t.Error("Register was called unexpectedly after a flush error")
	}
}

// ── runWatchdog ───────────────────────────────────────────────────────────────

// TestWatchdog_SkipsReregisterWhenCallbackSucceeded verifies that the watchdog
// does not call Register when LastRegisteredAt is after the disconnect time.
func TestWatchdog_SkipsReregisterWhenCallbackSucceeded(t *testing.T) {
	// Connection sequence: up → down → up (three poll ticks minimum).
	states := []bool{true, false, true, true}
	idx := atomic.Int32{}
	nc := &stubConn{
		isConnectedFn: func() bool {
			i := int(idx.Add(1)) - 1
			if i >= len(states) {
				return true
			}
			return states[i]
		},
	}

	// Registrar reports it registered AFTER the initial state was captured.
	// We set lastRegisteredAt to a future time so it's always after the
	// disconnect the watchdog will observe.
	reg := &stubRegistrar{
		lastRegisteredAt: time.Now().Add(10 * time.Second),
	}

	p := newTestPublisher(nc, reg)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	p.runWatchdog(ctx)

	if reg.registerCalls.Load() != 0 {
		t.Errorf("Register called %d time(s); expected 0 — callback already succeeded",
			reg.registerCalls.Load())
	}
}

// TestWatchdog_RegistersWhenCallbackFailed verifies that the watchdog calls
// Register when LastRegisteredAt is before the disconnect time (callback failed
// or never ran).
func TestWatchdog_RegistersWhenCallbackFailed(t *testing.T) {
	states := []bool{true, false, true, true, true}
	idx := atomic.Int32{}
	nc := &stubConn{
		isConnectedFn: func() bool {
			i := int(idx.Add(1)) - 1
			if i >= len(states) {
				return true
			}
			return states[i]
		},
	}

	// Registrar reports zero LastRegisteredAt — callback never ran.
	reg := &stubRegistrar{lastRegisteredAt: time.Time{}}

	p := newTestPublisher(nc, reg)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	p.runWatchdog(ctx)

	if reg.registerCalls.Load() < 1 {
		t.Errorf("Register called %d time(s); expected ≥1 — callback did not run",
			reg.registerCalls.Load())
	}
}

// TestWatchdog_SkipsReregisterOnShutdown verifies that when the context is
// canceled (worker shutting down), the watchdog does not call Register even
// if a reconnect is detected.
func TestWatchdog_SkipsReregisterOnShutdown(t *testing.T) {
	states := []bool{true, false, true, true}
	idx := atomic.Int32{}
	nc := &stubConn{
		isConnectedFn: func() bool {
			i := int(idx.Add(1)) - 1
			if i >= len(states) {
				return true
			}
			return states[i]
		},
	}
	reg := &stubRegistrar{lastRegisteredAt: time.Time{}}

	p := newTestPublisher(nc, reg)

	// Cancel the context immediately so the watchdog sees it as "shutting down"
	// on the first reconnect detection.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done

	p.runWatchdog(ctx)

	if reg.registerCalls.Load() != 0 {
		t.Errorf("Register called %d time(s) during shutdown; expected 0",
			reg.registerCalls.Load())
	}
}

// TestWatchdog_ResetsDisconnectStateAfterReconnect verifies that a second
// disconnect-reconnect cycle after the first one was handled correctly also
// triggers Register when needed.
func TestWatchdog_ResetsDisconnectStateAfterReconnect(t *testing.T) {
	// Two full disconnect→reconnect cycles. LastRegisteredAt is zero throughout
	// so the watchdog should register once per cycle.
	states := []bool{
		true,  // poll 1: connected
		false, // poll 2: dropped (cycle 1)
		true,  // poll 3: reconnected
		false, // poll 4: dropped (cycle 2)
		true,  // poll 5: reconnected
		true,  // poll 6+: stable
	}
	idx := atomic.Int32{}
	nc := &stubConn{
		isConnectedFn: func() bool {
			i := int(idx.Add(1)) - 1
			if i >= len(states) {
				return true
			}
			return states[i]
		},
	}
	reg := &stubRegistrar{lastRegisteredAt: time.Time{}}

	p := newTestPublisher(nc, reg)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	p.runWatchdog(ctx)

	if got := reg.registerCalls.Load(); got < 2 {
		t.Errorf("Register called %d time(s); expected ≥2 (one per cycle)", got)
	}
}
