// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Provenance tests: the subject a worker publishes on is the only identity
// NATS itself can vouch for, and these four tests prove the server actually
// enforces it rather than trusting the payload's own WorkerID field.
//
// Each test has worker A publish on its OWN subject (the only one NATS would
// let it publish on) while the payload claims — or the store already holds —
// worker B's identity, and asserts on STORE STATE that the forgery was
// discarded, not merely that it was logged.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// mustMarshal marshals v to JSON, failing the test on error.
func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// newFakeJetStreamMsg is an alias over the package's existing fakeJSMsg
// (logingest_test.go), which already implements jetstream.Msg with a
// settable Subject, Data, Ack and Nak. Given as a constructor matching the
// shape described for these tests so the subject — the whole point of a
// provenance test — is set at the call site.
func newFakeJetStreamMsg(_ *testing.T, subject string, data []byte) *fakeJSMsg {
	return &fakeJSMsg{subject: subject, data: data}
}

// seedRunnableTask creates a farm, queue, an online worker registered as
// workerID, a running job/step, and a task assigned to and running on that
// worker. Returns the job, step, and task.
func seedRunnableTask(t *testing.T, st *fake.Store, workerID string) (store.Job, store.Step, store.Task) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()

	farm, err := st.CreateFarm(ctx, store.Farm{ID: uuid.NewString(), Name: "f"})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err := st.CreateQueue(ctx, store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: "q"})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	if _, err := st.RegisterWorker(ctx, store.Worker{
		ID: workerID, FarmID: farm.ID, Hostname: workerID,
		Status: store.WorkerStatusOnline, CPUCount: 4, LastHeartbeatAt: &now,
		Tags: map[string]string{},
	}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	job, err := st.CreateJob(ctx, store.Job{
		ID:             uuid.NewString(),
		FarmID:         farm.ID,
		QueueID:        queue.ID,
		Name:           "job",
		Status:         store.JobStatusRunning,
		TemplateFormat: store.TemplateFormatJSON,
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	step, err := st.CreateStep(ctx, store.Step{
		ID: uuid.NewString(), JobID: job.ID, Name: "step",
		Status: store.StepStatusRunning, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}
	task, err := st.CreateTask(ctx, store.Task{
		ID:               uuid.NewString(),
		JobID:            job.ID,
		StepID:           step.ID,
		Name:             "task",
		Status:           store.TaskStatusRunning,
		AssignedWorkerID: workerID,
		CreatedAt:        now,
		UpdatedAt:        now,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return job, step, task
}

// ── task.status ────────────────────────────────────────────────────────────

// TestProvenance_StatusFromWrongWorker proves that a status message whose
// subject names worker A cannot complete a task held by worker B.
//
// Before this check, the handler read WorkerID only to log it. The only thing
// standing between an attacker and a forged completion was that attempt_id is
// an unguessable UUID — capability-by-obscurity, not authorization.
func TestProvenance_StatusFromWrongWorker(t *testing.T) {
	st := fake.New()
	ctx := t.Context()

	// Two workers, a task held by B, and a live attempt for it.
	const workerA, workerB = "worker-a", "worker-b"
	job, step, task := seedRunnableTask(t, st, workerB) // helper above
	attempt, err := st.CreateTaskAttempt(ctx, store.TaskAttempt{
		ID:        uuid.NewString(),
		TaskID:    task.ID,
		WorkerID:  workerB,
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("CreateTaskAttempt: %v", err)
	}

	s := newTestScheduler(st, &stubBus{}) // existing helper in this package
	s.ctx = ctx

	// Worker A forges a completion on its OWN subject — which is the only
	// subject NATS would let it publish on — for a task it does not hold.
	forged := protocol.TaskStatusMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeTaskStatus,
		TaskID:    task.ID,
		AttemptID: attempt.ID,
		JobID:     job.ID,
		Status:    "succeeded",
		WorkerID:  workerB, // the payload lies; the subject does not
		At:        time.Now().UTC(),
	}
	msg := newFakeJetStreamMsg(
		t,
		bus.TaskStatusSubject(workerA, job.ID),
		mustMarshal(t, forged),
	)

	s.handleTaskStatusMessage(msg)

	// The task must be untouched.
	got, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status == store.TaskStatusSucceeded {
		t.Fatal("forged status was applied: worker A completed worker B's task")
	}
	if got.Status != task.Status {
		t.Errorf("task status changed to %q, want unchanged %q", got.Status, task.Status)
	}
	// And the message must be acked, not redelivered forever.
	if !msg.acked {
		t.Error("forged message was not acked; it will redeliver in a loop")
	}
	_ = step
}

// ── task.logs ──────────────────────────────────────────────────────────────

// TestProvenance_LogsFromWrongWorker proves that a log chunk whose subject
// names worker A is not persisted against a task held by worker B.
//
// LogChunkMsg carries no worker-identity field at all — the subject is the
// ONLY identity available for this channel — so this is the case where
// dropping the subject check would leave zero provenance signal whatsoever.
func TestProvenance_LogsFromWrongWorker(t *testing.T) {
	st := fake.New()
	ctx := t.Context()

	const workerA, workerB = "worker-a", "worker-b"
	_, _, task := seedRunnableTask(t, st, workerB)
	attempt, err := st.CreateTaskAttempt(ctx, store.TaskAttempt{
		ID:        uuid.NewString(),
		TaskID:    task.ID,
		WorkerID:  workerB,
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("CreateTaskAttempt: %v", err)
	}

	s := newTestScheduler(st, &stubBus{})
	s.ctx = ctx

	forged := protocol.LogChunkMsg{
		TaskID:    task.ID,
		AttemptID: attempt.ID,
		SeqNum:    1,
		At:        time.Now().UTC(),
		Stream:    "stdout",
		Data:      "injected by worker A",
	}
	msg := newFakeJetStreamMsg(
		t,
		bus.TaskLogsSubject(workerA, task.ID),
		mustMarshal(t, forged),
	)

	s.handleLogChunk(msg)

	// afterNATSSeq=-1 (not 0): the fake message carries no NATS sequence
	// metadata, so a persisted row would land at NATSSeq=0, and the store
	// filters strictly greater than afterNATSSeq — 0 would silently exclude
	// it and pass for the wrong reason regardless of whether the row exists.
	logs, err := st.ListTaskLogs(ctx, attempt.ID, -1, 100)
	if err != nil {
		t.Fatalf("ListTaskLogs: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("forged log chunk was persisted: %d rows, want 0", len(logs))
	}
	if !msg.acked {
		t.Error("forged message was not acked; it will redeliver in a loop")
	}
}

// ── worker.deregister ─────────────────────────────────────────────────────

// TestProvenance_DeregisterOfAnotherWorker proves that worker A cannot mark
// worker B offline by publishing on its own subject with B's ID in the
// payload — a denial-of-service against the farm otherwise available to any
// worker that can reach the broker.
func TestProvenance_DeregisterOfAnotherWorker(t *testing.T) {
	st := fake.New()
	ctx := t.Context()

	const workerA, workerB = "worker-a", "worker-b"
	now := time.Now().UTC()
	if _, err := st.RegisterWorker(ctx, store.Worker{
		ID: workerA, FarmID: "farm-1", Status: store.WorkerStatusOnline, LastHeartbeatAt: &now,
	}); err != nil {
		t.Fatalf("RegisterWorker(A): %v", err)
	}
	if _, err := st.RegisterWorker(ctx, store.Worker{
		ID: workerB, FarmID: "farm-1", Status: store.WorkerStatusOnline, LastHeartbeatAt: &now,
	}); err != nil {
		t.Fatalf("RegisterWorker(B): %v", err)
	}

	s := newMetricsScheduler(st, &recordBus{}, "")

	forged := struct {
		WorkerID string `json:"worker_id"`
		Reason   string `json:"reason,omitempty"`
	}{WorkerID: workerB, Reason: "forged shutdown"}

	msg := newFakeJetStreamMsg(
		t,
		bus.WorkerDeregisterSubject(workerA),
		mustMarshal(t, forged),
	)

	s.handleWorkerMessage(msg)

	w, err := st.GetWorker(ctx, workerB)
	if err != nil {
		t.Fatalf("GetWorker(B): %v", err)
	}
	if w.Status != store.WorkerStatusOnline {
		t.Fatalf("worker B was marked %q by worker A's forged deregister, want online", w.Status)
	}
	if !msg.acked {
		t.Error("forged message was not acked; it will redeliver in a loop")
	}
}

// ── work.lease ─────────────────────────────────────────────────────────────

// TestProvenance_LeaseAsAnotherWorker proves that a lease request published
// on worker A's subject cannot have its assignments credited to worker B: the
// reply must be empty and no task may end up assigned to B.
func TestProvenance_LeaseAsAnotherWorker(t *testing.T) {
	st := fake.New()
	one := 1
	// seedLeaseFixture (lease_test.go) registers a real, eligible worker "w1"
	// with a ready task that WOULD be leased to it if the provenance check
	// did not intervene first — workerB below is that real worker, the one
	// being impersonated; workerA is the actual (unregistered) publisher, and
	// its absence from the store must not matter, since the mismatch check
	// has to fire before any worker lookup happens.
	_, taskIDs := seedLeaseFixture(t, st, []*int{&one})

	s := newMetricsScheduler(st, &recordBus{}, "f1")

	const workerA, workerB = "worker-a", "w1"
	req, err := json.Marshal(leaseRequest{WorkerID: workerB})
	if err != nil {
		t.Fatalf("marshal lease request: %v", err)
	}

	reply := s.handleLeaseRequest(workerA, "q1", req)

	var got leaseReply
	if err := json.Unmarshal(reply, &got); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if len(got.Assignments) != 0 {
		t.Fatalf("forged lease request returned %d assignments, want 0", len(got.Assignments))
	}

	for _, id := range taskIDs {
		task, err := st.GetTask(t.Context(), id)
		if err != nil {
			t.Fatalf("GetTask(%s): %v", id, err)
		}
		if task.AssignedWorkerID == workerB {
			t.Fatalf("task %s was assigned to impersonated worker %q", id, workerB)
		}
	}
}
