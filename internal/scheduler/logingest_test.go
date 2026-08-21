// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for logingest.go — item 8d of the test roadmap.
//
// handleLogChunk is an unexported method on *Scheduler, so tests live in
// package scheduler (white-box). A fakeJSMsg implements jetstream.Msg for the
// methods actually called by handleLogChunk (Data, Ack, Nak, Metadata, Subject).
// All other interface methods are forwarded to the embedded nil jetstream.Msg
// and will panic only if unexpectedly called.

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/ws"
)

// logTestWorkerID is the worker whose subject these tests publish log chunks
// on. handleLogChunk now checks it against the chunk's attempt, so any test
// exercising the persist path must open the attempt on this same worker.
const logTestWorkerID = "worker-1"

// ── fakeJSMsg: minimal jetstream.Msg for log ingest tests ────────────────────

// fakeJSMsg embeds jetstream.Msg (nil) and overrides only the methods called
// by handleLogChunk and the ackMsg/nakMsg helpers.
type fakeJSMsg struct {
	jetstream.Msg // nil embedded — unimplemented methods panic if called

	data    []byte
	subject string
	acked   bool
	nacked  bool
	metaErr error
	natsSeq uint64
}

func (m *fakeJSMsg) Data() []byte    { return m.data }
func (m *fakeJSMsg) Subject() string { return m.subject }

func (m *fakeJSMsg) Ack() error {
	m.acked = true
	return nil
}

func (m *fakeJSMsg) Nak() error {
	m.nacked = true
	return nil
}

func (m *fakeJSMsg) Metadata() (*jetstream.MsgMetadata, error) {
	if m.metaErr != nil {
		return nil, m.metaErr
	}
	return &jetstream.MsgMetadata{
		Sequence: jetstream.SequencePair{Stream: m.natsSeq},
	}, nil
}

// msgJSON serializes a LogChunkMsg to JSON for use as fakeJSMsg.data.
func msgJSON(t *testing.T, m protocol.LogChunkMsg) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal LogChunkMsg: %v", err)
	}
	return b
}

// newLogTestScheduler builds a Scheduler wired to st, safe for calling
// handleLogChunk without a live NATS broker.
func newLogTestScheduler(st store.Store) *Scheduler {
	return New(
		DefaultConfig(),
		st,
		nil, // bus — not used by handleLogChunk
		nil, // metrics — not used
		slog.New(slog.DiscardHandler),
		ws.NoopNotifier{},
		nil, // diagBuf — diagnostics disabled
	)
}

// ── handleLogChunk tests ──────────────────────────────────────────────────────

func TestHandleLogChunk_ValidStdout(t *testing.T) {
	st := fake.New()
	s := newLogTestScheduler(st)
	s.ctx = t.Context()

	attemptID := uuid.NewString()
	taskID := uuid.NewString()
	now := time.Now().UTC()

	if _, err := st.CreateTaskAttempt(t.Context(), store.TaskAttempt{
		ID: attemptID, TaskID: taskID, WorkerID: logTestWorkerID,
		AttemptNumber: 1, Status: store.AttemptStatusRunning, StartedAt: now,
	}); err != nil {
		t.Fatalf("CreateTaskAttempt: %v", err)
	}

	msg := &fakeJSMsg{
		subject: bus.TaskLogsSubject(logTestWorkerID, taskID),
		natsSeq: 42,
		data: msgJSON(t, protocol.LogChunkMsg{
			TaskID:    taskID,
			AttemptID: attemptID,
			SeqNum:    1,
			At:        now,
			Stream:    "stdout",
			Data:      "hello from worker",
		}),
	}

	s.handleLogChunk(msg)

	if !msg.acked {
		t.Error("expected message to be acked")
	}

	// Verify the log row was created.
	logs, err := st.ListTaskLogs(t.Context(), attemptID, 0, 100)
	if err != nil {
		t.Fatalf("ListTaskLogs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected 1 log row, got %d", len(logs))
	}
	if logs[0].Data != "hello from worker" {
		t.Errorf("Data = %q", logs[0].Data)
	}
	if logs[0].NATSSeq != 42 {
		t.Errorf("NATSSeq = %d, want 42", logs[0].NATSSeq)
	}
	if logs[0].Stream != store.LogStreamStdout {
		t.Errorf("Stream = %q, want stdout", logs[0].Stream)
	}
}

func TestHandleLogChunk_ValidStderr(t *testing.T) {
	st := fake.New()
	s := newLogTestScheduler(st)
	s.ctx = t.Context()

	attemptID := uuid.NewString()
	taskID := uuid.NewString()
	now := time.Now().UTC()
	if _, err := st.CreateTaskAttempt(t.Context(), store.TaskAttempt{
		ID: attemptID, TaskID: taskID, WorkerID: logTestWorkerID,
		AttemptNumber: 1, Status: store.AttemptStatusRunning, StartedAt: now,
	}); err != nil {
		t.Fatalf("CreateTaskAttempt: %v", err)
	}

	msg := &fakeJSMsg{
		subject: bus.TaskLogsSubject(logTestWorkerID, taskID),
		natsSeq: 1,
		data: msgJSON(t, protocol.LogChunkMsg{
			TaskID:    taskID,
			AttemptID: attemptID,
			SeqNum:    1,
			At:        now,
			Stream:    "stderr",
			Data:      "error output",
		}),
	}

	s.handleLogChunk(msg)

	logs, err := st.ListTaskLogs(t.Context(), attemptID, 0, 100)
	if err != nil {
		t.Fatalf("ListTaskLogs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected 1 log row, got %d", len(logs))
	}
	if logs[0].Stream != store.LogStreamStderr {
		t.Errorf("Stream = %q, want stderr", logs[0].Stream)
	}
}

func TestHandleLogChunk_ZeroAtUsesServerTime(t *testing.T) {
	st := fake.New()
	s := newLogTestScheduler(st)
	s.ctx = t.Context()

	before := time.Now().UTC()
	attemptID := uuid.NewString()
	taskID := uuid.NewString()
	if _, err := st.CreateTaskAttempt(t.Context(), store.TaskAttempt{
		ID: attemptID, TaskID: taskID, WorkerID: logTestWorkerID,
		AttemptNumber: 1, Status: store.AttemptStatusRunning, StartedAt: before,
	}); err != nil {
		t.Fatalf("CreateTaskAttempt: %v", err)
	}

	msg := &fakeJSMsg{
		subject: bus.TaskLogsSubject(logTestWorkerID, taskID),
		natsSeq: 1,
		data: msgJSON(t, protocol.LogChunkMsg{
			TaskID:    taskID,
			AttemptID: attemptID,
			SeqNum:    1,
			At:        time.Time{}, // zero → server clock
			Stream:    "stdout",
			Data:      "line",
		}),
	}

	s.handleLogChunk(msg)

	logs, err := st.ListTaskLogs(t.Context(), attemptID, 0, 100)
	if err != nil {
		t.Fatalf("ListTaskLogs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected 1 log row, got %d", len(logs))
	}
	if logs[0].At.Before(before) {
		t.Error("expected At to be populated with server time, not zero")
	}
}

func TestHandleLogChunk_MalformedJSON_Acked(t *testing.T) {
	st := fake.New()
	s := newLogTestScheduler(st)
	s.ctx = t.Context()

	msg := &fakeJSMsg{data: []byte("{not valid json")}
	s.handleLogChunk(msg)

	if !msg.acked {
		t.Error("malformed message should be acked (discarded)")
	}
}

func TestHandleLogChunk_MissingTaskID_Acked(t *testing.T) {
	st := fake.New()
	s := newLogTestScheduler(st)
	s.ctx = t.Context()

	msg := &fakeJSMsg{
		data: msgJSON(t, protocol.LogChunkMsg{
			TaskID:    "", // missing
			AttemptID: uuid.NewString(),
			SeqNum:    1,
			Stream:    "stdout",
			Data:      "line",
		}),
	}

	s.handleLogChunk(msg)

	if !msg.acked {
		t.Error("message with missing task_id should be acked (discarded)")
	}
}

func TestHandleLogChunk_MissingAttemptID_Acked(t *testing.T) {
	st := fake.New()
	s := newLogTestScheduler(st)
	s.ctx = t.Context()

	msg := &fakeJSMsg{
		data: msgJSON(t, protocol.LogChunkMsg{
			TaskID:    uuid.NewString(),
			AttemptID: "", // missing
			SeqNum:    1,
			Stream:    "stdout",
			Data:      "line",
		}),
	}

	s.handleLogChunk(msg)

	if !msg.acked {
		t.Error("message with missing attempt_id should be acked (discarded)")
	}
}

func TestHandleLogChunk_StoreFailure_Nacked(t *testing.T) {
	inner := fake.New()
	taskID := uuid.NewString()
	attemptID := uuid.NewString()
	if _, err := inner.CreateTaskAttempt(t.Context(), store.TaskAttempt{
		ID: attemptID, TaskID: taskID, WorkerID: logTestWorkerID,
		AttemptNumber: 1, Status: store.AttemptStatusRunning, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTaskAttempt: %v", err)
	}
	est := &logIngestErrSt{Store: inner}
	s := newLogTestScheduler(est)
	s.ctx = t.Context()

	msg := &fakeJSMsg{
		subject: bus.TaskLogsSubject(logTestWorkerID, taskID),
		data: msgJSON(t, protocol.LogChunkMsg{
			TaskID:    taskID,
			AttemptID: attemptID,
			SeqNum:    1,
			At:        time.Now().UTC(),
			Stream:    "stdout",
			Data:      "line",
		}),
	}

	s.handleLogChunk(msg)

	if !msg.nacked {
		t.Error("store failure should nack the message for redelivery")
	}
	if msg.acked {
		t.Error("message should not be acked when store fails")
	}
}

func TestHandleLogChunk_MetadataError_NATSSeqZero(t *testing.T) {
	// When metadata extraction fails, NATSSeq should be 0 but the log still persists.
	st := fake.New()
	s := newLogTestScheduler(st)
	s.ctx = t.Context()

	attemptID := uuid.NewString()
	taskID := uuid.NewString()
	if _, err := st.CreateTaskAttempt(t.Context(), store.TaskAttempt{
		ID: attemptID, TaskID: taskID, WorkerID: logTestWorkerID,
		AttemptNumber: 1, Status: store.AttemptStatusRunning, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTaskAttempt: %v", err)
	}

	msg := &fakeJSMsg{
		subject: bus.TaskLogsSubject(logTestWorkerID, taskID),
		metaErr: context.DeadlineExceeded, // metadata unavailable
		data: msgJSON(t, protocol.LogChunkMsg{
			TaskID:    taskID,
			AttemptID: attemptID,
			SeqNum:    7,
			At:        time.Now().UTC(),
			Stream:    "stdout",
			Data:      "output",
		}),
	}

	s.handleLogChunk(msg)

	// Use afterNATSSeq=-1 so that NATSSeq=0 entries (stored when metadata fails)
	// are included in the result (the filter is NATSSeq > afterNATSSeq).
	logs, err := st.ListTaskLogs(t.Context(), attemptID, -1, 100)
	if err != nil {
		t.Fatalf("ListTaskLogs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected 1 log row even when metadata fails, got %d", len(logs))
	}
	if logs[0].NATSSeq != 0 {
		t.Errorf("NATSSeq = %d, want 0 when metadata unavailable", logs[0].NATSSeq)
	}
}

// ── logIngestErrSt: store that fails CreateTaskLog ────────────────────────────

type logIngestErrSt struct {
	store.Store
}

func (*logIngestErrSt) CreateTaskLog(_ context.Context, _ store.TaskLog) (store.TaskLog, error) {
	return store.TaskLog{}, errInjectedLog
}

var errInjectedLog = context.DeadlineExceeded
