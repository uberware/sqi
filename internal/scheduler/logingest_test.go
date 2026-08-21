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
	"errors"
	"fmt"
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

// ── attemptOwnerCache tests ───────────────────────────────────────────────────

// countingAttemptStore wraps a store.Store and counts GetTaskAttempt calls,
// so tests can prove the attempt-owner cache does (or does not) avoid a
// store read.
type countingAttemptStore struct {
	store.Store

	getTaskAttemptCalls int
}

func (s *countingAttemptStore) GetTaskAttempt(ctx context.Context, id string) (store.TaskAttempt, error) {
	s.getTaskAttemptCalls++
	return s.Store.GetTaskAttempt(ctx, id)
}

// TestHandleLogChunk_RepeatedChunk_CachedAfterFirstRead proves a second log
// chunk for the same attempt does not re-read the store: the first chunk
// misses the cache and reads through, the second hits.
func TestHandleLogChunk_RepeatedChunk_CachedAfterFirstRead(t *testing.T) {
	cst := &countingAttemptStore{Store: fake.New()}
	s := newLogTestScheduler(cst)
	s.ctx = t.Context()

	attemptID := uuid.NewString()
	taskID := uuid.NewString()
	now := time.Now().UTC()
	if _, err := cst.CreateTaskAttempt(t.Context(), store.TaskAttempt{
		ID: attemptID, TaskID: taskID, WorkerID: logTestWorkerID,
		AttemptNumber: 1, Status: store.AttemptStatusRunning, StartedAt: now,
	}); err != nil {
		t.Fatalf("CreateTaskAttempt: %v", err)
	}

	newChunk := func(seq int64) *fakeJSMsg {
		return &fakeJSMsg{
			subject: bus.TaskLogsSubject(logTestWorkerID, taskID),
			natsSeq: uint64(seq), // test data, small positive constant
			data: msgJSON(t, protocol.LogChunkMsg{
				TaskID:    taskID,
				AttemptID: attemptID,
				SeqNum:    seq,
				At:        now,
				Stream:    "stdout",
				Data:      "line",
			}),
		}
	}

	first := newChunk(1)
	s.handleLogChunk(first)
	if !first.acked {
		t.Fatal("expected first chunk to be acked")
	}
	if cst.getTaskAttemptCalls != 1 {
		t.Fatalf("getTaskAttemptCalls after first chunk = %d, want 1 (cache miss falls back to the store)", cst.getTaskAttemptCalls)
	}

	second := newChunk(2)
	s.handleLogChunk(second)
	if !second.acked {
		t.Fatal("expected second chunk to be acked")
	}
	if cst.getTaskAttemptCalls != 1 {
		t.Errorf("getTaskAttemptCalls after second chunk = %d, want still 1 (cache hit must not re-read the store)", cst.getTaskAttemptCalls)
	}

	logs, err := cst.ListTaskLogs(t.Context(), attemptID, 0, 100)
	if err != nil {
		t.Fatalf("ListTaskLogs: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("expected 2 log rows, got %d", len(logs))
	}
}

// TestHandleLogChunk_CacheMiss_VanishedAttempt_StillDiscarded proves that on
// a cache miss for an attempt the store has never heard of, handleLogChunk
// still falls back to the store (rather than assuming ownership) and
// discards the chunk exactly as it would with no cache at all.
func TestHandleLogChunk_CacheMiss_VanishedAttempt_StillDiscarded(t *testing.T) {
	cst := &countingAttemptStore{Store: fake.New()}
	s := newLogTestScheduler(cst)
	s.ctx = t.Context()

	msg := &fakeJSMsg{
		subject: bus.TaskLogsSubject(logTestWorkerID, uuid.NewString()),
		data: msgJSON(t, protocol.LogChunkMsg{
			TaskID:    uuid.NewString(),
			AttemptID: uuid.NewString(), // never created — vanished/unknown
			SeqNum:    1,
			At:        time.Now().UTC(),
			Stream:    "stdout",
			Data:      "line",
		}),
	}

	s.handleLogChunk(msg)

	if !msg.acked {
		t.Error("chunk for an unknown attempt should be acked (discarded)")
	}
	if cst.getTaskAttemptCalls != 1 {
		t.Errorf("getTaskAttemptCalls = %d, want 1 (a cache miss must still consult the store)", cst.getTaskAttemptCalls)
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

// ── attemptOwnerCache: stale hit on a deleted attempt ─────────────────────

// fkEnforcingLogStore wraps a store.Store and makes CreateTaskLog fail
// exactly as the SQLite backend does when a log row's AttemptID no longer
// names an existing task_attempts row: a FOREIGN KEY constraint failure,
// which mapErr turns into store.ErrConflict. The fake store enforces no such
// constraint (internal/store/fake/task_log.go appends unconditionally), so a
// test exercising this path needs this wrapper.
type fkEnforcingLogStore struct {
	store.Store
}

func (s *fkEnforcingLogStore) CreateTaskLog(ctx context.Context, log store.TaskLog) (store.TaskLog, error) {
	if _, err := s.GetTaskAttempt(ctx, log.AttemptID); errors.Is(err, store.ErrNotFound) {
		return store.TaskLog{}, fmt.Errorf("%w: FOREIGN KEY constraint failed", store.ErrConflict)
	} else if err != nil {
		return store.TaskLog{}, err
	}
	return s.Store.CreateTaskLog(ctx, log)
}

// TestHandleLogChunk_CacheHit_DeletedAttempt_SelfHeals is the regression test
// for the hazard on the cache-HIT path: a cache hit against an attempt whose
// row has been deleted out from under it — e.g. DELETE /api/v1/jobs/{id} on
// an ACTIVE job cancels its tasks and then runs DeleteJob, which removes the
// task_attempts row, while a worker is still publishing chunks for the
// window before its process notices — must not turn into a NAK loop.
//
// The first chunk populates the cache. Deleting the job removes the attempt
// row underneath it, but nothing evicts the cache entry on that path (it is
// a bulk write, not a terminal task-status message). The next chunk hits the
// stale-but-identity-matching cache entry, so both ownership checks pass and
// the write is attempted — and must fail. That failure has to evict the
// entry so the redelivery takes the store path and discards the chunk
// cleanly, rather than hitting the same stale entry and failing forever.
func TestHandleLogChunk_CacheHit_DeletedAttempt_SelfHeals(t *testing.T) {
	base := fake.New()
	st := &fkEnforcingLogStore{Store: base}
	s := newLogTestScheduler(st)
	s.ctx = t.Context()

	ctx := t.Context()
	now := time.Now().UTC()

	if _, err := base.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "farm-1"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if _, err := base.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "queue-1"}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	job, err := base.CreateJob(ctx, store.Job{
		ID: uuid.NewString(), FarmID: "farm-1", QueueID: "queue-1", Name: "job",
		Status: store.JobStatusRunning, TemplateFormat: store.TemplateFormatJSON,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	step, err := base.CreateStep(ctx, store.Step{
		ID: uuid.NewString(), JobID: job.ID, Name: "s1",
		Status: store.StepStatusRunning, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}
	task, err := base.CreateTask(ctx, store.Task{
		ID: uuid.NewString(), JobID: job.ID, StepID: step.ID, Name: "t1",
		Status: store.TaskStatusRunning, AssignedWorkerID: logTestWorkerID,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	attempt, err := base.CreateTaskAttempt(ctx, store.TaskAttempt{
		ID: uuid.NewString(), TaskID: task.ID, WorkerID: logTestWorkerID,
		AttemptNumber: 1, Status: store.AttemptStatusRunning, StartedAt: now, CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateTaskAttempt: %v", err)
	}

	newChunk := func(seq int64) *fakeJSMsg {
		return &fakeJSMsg{
			subject: bus.TaskLogsSubject(logTestWorkerID, task.ID),
			natsSeq: uint64(seq), // test data, small positive constant
			data: msgJSON(t, protocol.LogChunkMsg{
				TaskID:    task.ID,
				AttemptID: attempt.ID,
				SeqNum:    seq,
				At:        now,
				Stream:    "stdout",
				Data:      "line",
			}),
		}
	}

	// First chunk: cache miss, store read succeeds, cache is populated.
	first := newChunk(1)
	s.handleLogChunk(first)
	if !first.acked {
		t.Fatal("expected first chunk to be acked")
	}
	if _, ok := s.attemptCache.get(attempt.ID); !ok {
		t.Fatal("expected first chunk to populate the attempt-owner cache")
	}

	// The operator deletes the active job: tasks are canceled and DeleteJob
	// removes the attempt row along with everything else. Nothing on this
	// path evicts the cache entry, so it survives untouched.
	if err := base.DeleteJob(ctx, job.ID); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	if _, ok := s.attemptCache.get(attempt.ID); !ok {
		t.Fatal("test setup invariant broken: cache entry should still be present after DeleteJob")
	}

	// Second chunk: a cache HIT against the now-stale entry. Both identity
	// checks pass (they check the cached, historical values), so the write
	// is attempted and fails with the FK-style error.
	second := newChunk(2)
	s.handleLogChunk(second)
	if !second.nacked {
		t.Fatal("expected the write against a deleted attempt to nak for redelivery")
	}
	if second.acked {
		t.Error("second chunk should not be acked when the write fails")
	}
	if _, ok := s.attemptCache.get(attempt.ID); ok {
		t.Fatal("expected the failed write to evict the stale cache entry")
	}

	// Redelivery: the cache is now empty, so this takes the store path,
	// which correctly discards (acks) a chunk for a vanished attempt —
	// self-healing, exactly like the pre-cache behavior, instead of nacking
	// again and looping.
	redelivery := newChunk(2)
	s.handleLogChunk(redelivery)
	if !redelivery.acked {
		t.Error("expected the redelivery to be acked (discarded) once the cache entry is gone")
	}
	if redelivery.nacked {
		t.Error("redelivery should not nak again — that would loop forever")
	}
}
