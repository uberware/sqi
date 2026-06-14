// SPDX-License-Identifier: AGPL-3.0-or-later

package pull

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/worker/protocol"
)

// ── Mocks ─────────────────────────────────────────────────────────────────────

// mockMsg implements [ackableMsg] for testing handleMessage without needing a
// live NATS server or the full [jetstream.Msg] interface.
type mockMsg struct {
	data     []byte
	acked    bool
	nacked   bool
	nakDelay time.Duration
	// ackErr is returned by Ack if non-nil.
	ackErr error
	// nakErr is returned by NakWithDelay if non-nil.
	nakErr error
}

func (m *mockMsg) Data() []byte { return m.data }

func (m *mockMsg) Ack() error {
	m.acked = true
	return m.ackErr
}

func (m *mockMsg) NakWithDelay(d time.Duration) error {
	m.nacked = true
	m.nakDelay = d
	return m.nakErr
}

// mockStateSource is a [StateSource] that returns a fixed active count.
type mockStateSource struct{ count int }

func (m *mockStateSource) ActiveTaskCount() int { return m.count }

// mockDispatcher records Dispatch calls and returns a configurable error.
type mockDispatcher struct {
	dispatched []*protocol.AssignMsg
	err        error
}

func (d *mockDispatcher) Dispatch(_ context.Context, msg *protocol.AssignMsg) error {
	d.dispatched = append(d.dispatched, msg)
	return d.err
}

// testLogger returns a discard slog.Logger so test output is not polluted.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// mustMarshal marshals v to JSON, panicking on error. For use in test helpers
// where marshaling a known-good struct should never fail.
func mustMarshal(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic("mustMarshal: " + err.Error())
	}
	return data
}

// validAssignMsg returns a marshaled [protocol.AssignMsg] for the given task
// ID and compute location.
func validAssignMsg(taskID, computeLoc string) []byte {
	return mustMarshal(protocol.AssignMsg{
		Version:         protocol.ProtocolVersion,
		Type:            protocol.TypeAssign,
		TaskID:          taskID,
		JobID:           "job-1",
		AttemptID:       "attempt-1",
		TaskName:        "test-task",
		WorkerID:        "worker-1",
		AssignedAt:      time.Now(),
		ComputeLocation: computeLoc,
	})
}

// newTestPuller creates a Puller wired with the given state and dispatcher,
// but no JetStream context (nil is safe for handleMessage tests).
func newTestPuller(cfg Config, state StateSource, disp TaskDispatcher) *Puller {
	return New(nil, cfg, state, disp, testLogger())
}

// ── sanitizeConsumerToken ─────────────────────────────────────────────────────

func TestSanitizeConsumerToken(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"abc-123", "abc-123"},
		{"UUID-abc-DEF_0.9", "UUID-abc-DEF_0.9"},
		{"queue with spaces", "queue_with_spaces"},
		{"queue/slash", "queue_slash"},
		{"a@b!c", "a_b_c"},
		{"", ""},
		{"---", "---"},
		{"a.b.c", "a.b.c"},
	}
	for _, tc := range tests {
		got := sanitizeConsumerToken(tc.input)
		if got != tc.want {
			t.Errorf("sanitizeConsumerToken(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// ── New / defaults ────────────────────────────────────────────────────────────

func TestNew_DefaultsApplied(t *testing.T) {
	p := New(nil, Config{MaxConcurrentTasks: 0}, NoopStateSource{}, NoopDispatcher{}, testLogger())

	if p.cfg.IdleBackoff != 2*time.Second {
		t.Errorf("IdleBackoff = %v, want 2s", p.cfg.IdleBackoff)
	}
	if p.cfg.NackDelay != 5*time.Second {
		t.Errorf("NackDelay = %v, want 5s", p.cfg.NackDelay)
	}
	// MaxConcurrentTasks of 0 is clamped to 1.
	if p.cfg.MaxConcurrentTasks != 1 {
		t.Errorf("MaxConcurrentTasks = %d, want 1 (clamped from 0)", p.cfg.MaxConcurrentTasks)
	}
}

func TestNew_ExplicitValuesPreserved(t *testing.T) {
	p := New(nil, Config{
		MaxConcurrentTasks: 8,
		IdleBackoff:        10 * time.Second,
		NackDelay:          15 * time.Second,
	}, NoopStateSource{}, NoopDispatcher{}, testLogger())

	if p.cfg.MaxConcurrentTasks != 8 {
		t.Errorf("MaxConcurrentTasks = %d, want 8", p.cfg.MaxConcurrentTasks)
	}
	if p.cfg.IdleBackoff != 10*time.Second {
		t.Errorf("IdleBackoff = %v, want 10s", p.cfg.IdleBackoff)
	}
	if p.cfg.NackDelay != 15*time.Second {
		t.Errorf("NackDelay = %v, want 15s", p.cfg.NackDelay)
	}
}

// ── NoopDispatcher / NoopStateSource ─────────────────────────────────────────

func TestNoopDispatcher_AlwaysAccepts(t *testing.T) {
	d := NoopDispatcher{}
	if err := d.Dispatch(context.Background(), &protocol.AssignMsg{}); err != nil {
		t.Errorf("NoopDispatcher.Dispatch returned error: %v", err)
	}
}

func TestNoopStateSource_AlwaysZero(t *testing.T) {
	s := NoopStateSource{}
	if n := s.ActiveTaskCount(); n != 0 {
		t.Errorf("NoopStateSource.ActiveTaskCount() = %d, want 0", n)
	}
}

// ── handleMessage: ack path ───────────────────────────────────────────────────

func TestHandleMessage_ValidPayload_Acked(t *testing.T) {
	disp := &mockDispatcher{}
	p := newTestPuller(Config{
		MaxConcurrentTasks: 4,
		ComputeLocation:    "onprem",
		NackDelay:          5 * time.Second,
	}, &mockStateSource{}, disp)

	msg := &mockMsg{data: validAssignMsg("task-1", "onprem")}
	p.handleMessage(context.Background(), msg, "q1")

	if !msg.acked {
		t.Error("expected message to be acked on success")
	}
	if msg.nacked {
		t.Error("expected message NOT to be nacked on success")
	}
	if len(disp.dispatched) != 1 {
		t.Fatalf("expected 1 dispatch call, got %d", len(disp.dispatched))
	}
	if disp.dispatched[0].TaskID != "task-1" {
		t.Errorf("dispatched TaskID = %q, want %q", disp.dispatched[0].TaskID, "task-1")
	}
}

// ── handleMessage: nack paths ─────────────────────────────────────────────────

func TestHandleMessage_MalformedJSON_Nacked(t *testing.T) {
	disp := &mockDispatcher{}
	p := newTestPuller(Config{
		MaxConcurrentTasks: 4,
		NackDelay:          3 * time.Second,
	}, &mockStateSource{}, disp)

	msg := &mockMsg{data: []byte("not json {")}
	p.handleMessage(context.Background(), msg, "q1")

	if msg.acked {
		t.Error("expected message NOT to be acked on malformed JSON")
	}
	if !msg.nacked {
		t.Error("expected message to be nacked on malformed JSON")
	}
	if len(disp.dispatched) != 0 {
		t.Error("expected no dispatch call on malformed JSON")
	}
}

func TestHandleMessage_WrongProtocolVersion_Nacked(t *testing.T) {
	data := mustMarshal(protocol.AssignMsg{
		Version:  "99",
		Type:     protocol.TypeAssign,
		TaskID:   "task-bad-version",
		JobID:    "job-1",
		WorkerID: "w1",
	})
	disp := &mockDispatcher{}
	p := newTestPuller(Config{
		MaxConcurrentTasks: 4,
		NackDelay:          3 * time.Second,
	}, &mockStateSource{}, disp)

	msg := &mockMsg{data: data}
	p.handleMessage(context.Background(), msg, "q1")

	if !msg.nacked {
		t.Error("expected message to be nacked on protocol version mismatch")
	}
	if msg.acked {
		t.Error("expected message NOT to be acked on protocol version mismatch")
	}
	if len(disp.dispatched) != 0 {
		t.Error("expected no dispatch call on version mismatch")
	}
}

// Compute location mismatch.
func TestHandleMessage_ComputeLocationMismatch_Nacked(t *testing.T) {
	disp := &mockDispatcher{}
	p := newTestPuller(Config{
		MaxConcurrentTasks: 4,
		ComputeLocation:    "onprem_linux",
		NackDelay:          3 * time.Second,
	}, &mockStateSource{}, disp)

	msg := &mockMsg{data: validAssignMsg("task-loc", "cloud_aws_us_east")}
	p.handleMessage(context.Background(), msg, "q1")

	if !msg.nacked {
		t.Error("expected message to be nacked on compute location mismatch")
	}
	if msg.acked {
		t.Error("expected message NOT to be acked on compute location mismatch")
	}
	if len(disp.dispatched) != 0 {
		t.Error("expected no dispatch call on compute location mismatch")
	}
}

// Empty assignment location disables the check.
func TestHandleMessage_EmptyAssignmentLocation_Accepted(t *testing.T) {
	disp := &mockDispatcher{}
	p := newTestPuller(Config{
		MaxConcurrentTasks: 4,
		ComputeLocation:    "onprem_linux",
		NackDelay:          3 * time.Second,
	}, &mockStateSource{}, disp)

	// Assignment has no ComputeLocation — check must be skipped.
	msg := &mockMsg{data: validAssignMsg("task-no-loc", "")}
	p.handleMessage(context.Background(), msg, "q1")

	if !msg.acked {
		t.Error("expected message to be acked when assignment has no compute location")
	}
}

// Empty worker location disables the check.
func TestHandleMessage_EmptyWorkerLocation_Accepted(t *testing.T) {
	disp := &mockDispatcher{}
	p := newTestPuller(Config{
		MaxConcurrentTasks: 4,
		ComputeLocation:    "", // no location configured on this worker
		NackDelay:          3 * time.Second,
	}, &mockStateSource{}, disp)

	msg := &mockMsg{data: validAssignMsg("task-any-loc", "cloud_aws")}
	p.handleMessage(context.Background(), msg, "q1")

	if !msg.acked {
		t.Error("expected message to be acked when worker has no compute location")
	}
}

// Dispatcher rejection → nack with the configured delay.
func TestHandleMessage_DispatcherError_Nacked(t *testing.T) {
	wantDelay := 7 * time.Second
	disp := &mockDispatcher{err: errors.New("pre-execution validation failed")}
	p := newTestPuller(Config{
		MaxConcurrentTasks: 4,
		NackDelay:          wantDelay,
	}, &mockStateSource{}, disp)

	msg := &mockMsg{data: validAssignMsg("task-rejected", "")}
	p.handleMessage(context.Background(), msg, "q1")

	if !msg.nacked {
		t.Error("expected message to be nacked on dispatcher error")
	}
	if msg.acked {
		t.Error("expected message NOT to be acked on dispatcher error")
	}
	if msg.nakDelay != wantDelay {
		t.Errorf("nack delay = %v, want %v", msg.nakDelay, wantDelay)
	}
	if len(disp.dispatched) != 1 {
		t.Errorf("expected 1 dispatch call, got %d", len(disp.dispatched))
	}
}

// Ack failure is non-fatal — the dispatcher still ran.
func TestHandleMessage_AckError_DispatchStillRan(t *testing.T) {
	disp := &mockDispatcher{}
	p := newTestPuller(Config{
		MaxConcurrentTasks: 4,
		NackDelay:          5 * time.Second,
	}, &mockStateSource{}, disp)

	msg := &mockMsg{
		data:   validAssignMsg("task-ack-fail", ""),
		ackErr: errors.New("transient ack failure"),
	}
	p.handleMessage(context.Background(), msg, "q1")

	// Dispatch should have been called even though ack subsequently failed.
	if len(disp.dispatched) != 1 {
		t.Errorf("expected 1 dispatch call, got %d", len(disp.dispatched))
	}
	// Ack was attempted (ackErr surfaced from the attempt, not a missed call).
	if !msg.acked {
		t.Error("expected Ack to have been called despite returning an error")
	}
}

// ── Nack error path ───────────────────────────────────────────────────────────

func TestHandleMessage_NackError_LoggedButNonFatal(t *testing.T) {
	// When NakWithDelay itself returns an error, handleMessage must not panic
	// or propagate the error — it is logged as a warning and the caller
	// continues (AckWait will trigger redelivery regardless).
	disp := &mockDispatcher{err: errors.New("rejected")}
	p := newTestPuller(Config{
		MaxConcurrentTasks: 4,
		NackDelay:          3 * time.Second,
	}, &mockStateSource{}, disp)

	msg := &mockMsg{
		data:   validAssignMsg("task-nak-err", ""),
		nakErr: errors.New("nats: publish error"),
	}
	// Should not panic.
	p.handleMessage(context.Background(), msg, "q1")

	if !msg.nacked {
		t.Error("expected NakWithDelay to have been called")
	}
}

// ── NackDelay propagation ─────────────────────────────────────────────────────

func TestHandleMessage_NackDelay_PropagatedToNakWithDelay(t *testing.T) {
	wantDelay := 12 * time.Second
	// Trigger nack via malformed JSON (simplest path).
	p := newTestPuller(Config{
		MaxConcurrentTasks: 4,
		NackDelay:          wantDelay,
	}, &mockStateSource{}, &mockDispatcher{})

	msg := &mockMsg{data: []byte("{")} // valid JSON but not a valid AssignMsg
	p.handleMessage(context.Background(), msg, "q1")

	if msg.nakDelay != wantDelay {
		t.Errorf("nack delay = %v, want %v", msg.nakDelay, wantDelay)
	}
}
