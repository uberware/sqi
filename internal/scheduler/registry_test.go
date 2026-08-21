// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for the worker registry path in scheduler.go: handleWorkerMessage
// routing plus handleWorkerRegister, handleWorkerHeartbeat, and
// handleWorkerDeregister. These are white-box tests in package scheduler.
//
// fakeJSMsg (from logingest_test.go) supplies a jetstream.Msg with a settable
// Subject so handleWorkerMessage routes by subject. A real metrics registry is
// used because every handler refreshes the WorkersTotal gauge.

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/ws"
)

// workerMsgJSON marshals any value to JSON bytes for a fakeJSMsg payload.
func workerMsgJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// ── handleWorkerRegister ──────────────────────────────────────────────────────

func TestHandleWorkerRegister_Valid(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "")

	msg := &fakeJSMsg{
		subject: bus.WorkerRegisterSubject("w-1"),
		data: workerMsgJSON(t, protocol.RegisterMsg{
			Version: protocol.ProtocolVersion, Type: protocol.TypeRegister,
			WorkerID: "w-1", FarmID: "farm-1", Name: "worker-2", Hostname: "node-1", OS: "linux",
			WorkerVersion: "v0.1.0",
		}),
	}
	s.handleWorkerMessage(msg)

	if !msg.acked {
		t.Error("valid register should be acked")
	}
	w, err := st.GetWorker(t.Context(), "w-1")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if w.Status != store.WorkerStatusOnline {
		t.Errorf("worker status = %q, want online", w.Status)
	}
	if w.Name != "worker-2" {
		t.Errorf("worker name = %q, want worker-2", w.Name)
	}
	if w.Version != "v0.1.0" {
		t.Errorf("worker version = %q, want v0.1.0", w.Version)
	}
	if w.LastHeartbeatAt == nil {
		t.Error("expected LastHeartbeatAt set on registration")
	}
}

func TestHandleWorkerRegister_MalformedJSON_Acked(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "")

	msg := &fakeJSMsg{subject: bus.WorkerRegisterSubject("w-1"), data: []byte("{bad")}
	s.handleWorkerMessage(msg)

	if !msg.acked {
		t.Error("malformed register should be acked (discarded)")
	}
}

// TestHandleWorkerRegister_EmptyPayloadWorkerID_Acked drives a payload with no
// worker_id at all against a real subject. There is no separate "missing
// worker_id" code path any more: bus.ParseWorkerSubject guarantees the
// subject's worker token is never empty, so an empty m.WorkerID always fails
// the subject/payload mismatch check first and is discarded through that
// branch.
func TestHandleWorkerRegister_EmptyPayloadWorkerID_Acked(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "")

	msg := &fakeJSMsg{
		subject: bus.WorkerRegisterSubject("w-1"),
		data:    workerMsgJSON(t, protocol.RegisterMsg{Version: protocol.ProtocolVersion, WorkerID: "", FarmID: "farm-1"}),
	}
	s.handleWorkerMessage(msg)

	if !msg.acked {
		t.Error("register with an empty payload worker_id should be acked (mismatch)")
	}
	if _, err := st.GetWorker(t.Context(), "w-1"); err == nil {
		t.Error("no worker should have been registered from a mismatched payload")
	}
}

// registerErrSt makes RegisterWorker fail to exercise the nak path.
type registerErrSt struct {
	store.Store
}

func (*registerErrSt) RegisterWorker(_ context.Context, _ store.Worker) (store.Worker, error) {
	return store.Worker{}, context.DeadlineExceeded
}

func TestHandleWorkerRegister_StoreError_Nacked(t *testing.T) {
	st := &registerErrSt{Store: fake.New()}
	s := newMetricsScheduler(st, &recordBus{}, "")

	msg := &fakeJSMsg{
		subject: bus.WorkerRegisterSubject("w-1"),
		data:    workerMsgJSON(t, protocol.RegisterMsg{Version: protocol.ProtocolVersion, WorkerID: "w-1", FarmID: "farm-1"}),
	}
	s.handleWorkerMessage(msg)

	if !msg.nacked {
		t.Error("store error on register should nack for redelivery")
	}
	if msg.acked {
		t.Error("message should not be acked when register fails")
	}
}

// touchRecordingStore wraps a real store and records every
// TouchWorkerCredential call, so a test can prove registration does or does
// not reach it without depending on timing.
type touchRecordingStore struct {
	store.Store

	touched []string
}

func (s *touchRecordingStore) TouchWorkerCredential(ctx context.Context, workerID string, at time.Time) error {
	s.touched = append(s.touched, workerID)
	return s.Store.TouchWorkerCredential(ctx, workerID, at)
}

// TestHandleWorkerRegister_TouchesActiveCredential_WhenAuthEnabled proves
// that registering a worker with an active broker credential sets
// LastSeenAt, and only when broker authentication is enabled.
func TestHandleWorkerRegister_TouchesActiveCredential_WhenAuthEnabled(t *testing.T) {
	fk := fake.New()
	if _, err := fk.CreateWorkerCredential(t.Context(), store.WorkerCredential{
		ID: uuid.NewString(), WorkerID: "w-1", PublicKey: "pub1", EnrolledAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed CreateWorkerCredential: %v", err)
	}
	st := &touchRecordingStore{Store: fk}

	cfg := DefaultConfig()
	cfg.NATSAuthEnabled = true
	s := New(cfg, st, &recordBus{}, metrics.New(), slog.New(slog.DiscardHandler), ws.NoopNotifier{}, nil)
	s.ctx = context.Background()

	msg := &fakeJSMsg{
		subject: bus.WorkerRegisterSubject("w-1"),
		data: workerMsgJSON(t, protocol.RegisterMsg{
			Version: protocol.ProtocolVersion, Type: protocol.TypeRegister,
			WorkerID: "w-1", FarmID: "farm-1", Hostname: "node-1", OS: "linux",
		}),
	}
	s.handleWorkerMessage(msg)

	if !msg.acked {
		t.Error("valid register should be acked")
	}
	if len(st.touched) != 1 || st.touched[0] != "w-1" {
		t.Errorf("touched = %v, want exactly one call for w-1", st.touched)
	}
	cred, err := fk.GetActiveWorkerCredentialByWorkerID(t.Context(), "w-1")
	if err != nil {
		t.Fatalf("GetActiveWorkerCredentialByWorkerID: %v", err)
	}
	if cred.LastSeenAt == nil {
		t.Error("expected LastSeenAt to be set after registration")
	}
}

// TestHandleWorkerRegister_NoTouchCall_WhenAuthDisabled asserts the
// auth-off default path does no extra store work: no credential rows exist
// on an auth-off farm, and the touch call must not even be attempted.
func TestHandleWorkerRegister_NoTouchCall_WhenAuthDisabled(t *testing.T) {
	st := &touchRecordingStore{Store: fake.New()}
	s := newMetricsScheduler(st, &recordBus{}, "") // DefaultConfig: NATSAuthEnabled false

	msg := &fakeJSMsg{
		subject: bus.WorkerRegisterSubject("w-1"),
		data: workerMsgJSON(t, protocol.RegisterMsg{
			Version: protocol.ProtocolVersion, Type: protocol.TypeRegister,
			WorkerID: "w-1", FarmID: "farm-1", Hostname: "node-1", OS: "linux",
		}),
	}
	s.handleWorkerMessage(msg)

	if !msg.acked {
		t.Error("valid register should be acked")
	}
	if len(st.touched) != 0 {
		t.Errorf("touched = %v, want no calls with broker auth disabled", st.touched)
	}
}

// TestHandleWorkerRegister_NoActiveCredential_StillAcked proves a missing
// credential (store.ErrNotFound) never fails registration: the message is
// still acked and the worker is still registered.
func TestHandleWorkerRegister_NoActiveCredential_StillAcked(t *testing.T) {
	st := fake.New()
	cfg := DefaultConfig()
	cfg.NATSAuthEnabled = true
	s := New(cfg, st, &recordBus{}, metrics.New(), slog.New(slog.DiscardHandler), ws.NoopNotifier{}, nil)
	s.ctx = context.Background()

	msg := &fakeJSMsg{
		subject: bus.WorkerRegisterSubject("w-1"),
		data: workerMsgJSON(t, protocol.RegisterMsg{
			Version: protocol.ProtocolVersion, Type: protocol.TypeRegister,
			WorkerID: "w-1", FarmID: "farm-1", Hostname: "node-1", OS: "linux",
		}),
	}
	s.handleWorkerMessage(msg)

	if !msg.acked {
		t.Error("register should be acked even with no active credential to touch")
	}
	if _, err := st.GetWorker(t.Context(), "w-1"); err != nil {
		t.Errorf("worker should still be registered: %v", err)
	}
}

// ── handleWorkerHeartbeat ─────────────────────────────────────────────────────

func TestHandleWorkerHeartbeat_Valid(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "")

	now := time.Now().UTC()
	if _, err := st.RegisterWorker(t.Context(), store.Worker{
		ID: "w-1", FarmID: "farm-1", Status: store.WorkerStatusOnline, LastHeartbeatAt: &now,
	}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}

	hbAt := now.Add(5 * time.Second)
	msg := &fakeJSMsg{
		subject: bus.WorkerHeartbeatSubject("w-1"),
		data:    workerMsgJSON(t, protocol.HeartbeatMsg{Version: protocol.ProtocolVersion, WorkerID: "w-1", At: hbAt}),
	}
	s.handleWorkerMessage(msg)

	if !msg.acked {
		t.Error("valid heartbeat should be acked")
	}
	w, err := st.GetWorker(t.Context(), "w-1")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if w.LastHeartbeatAt == nil || !w.LastHeartbeatAt.Equal(hbAt) {
		t.Errorf("LastHeartbeatAt = %v, want %v", w.LastHeartbeatAt, hbAt)
	}
}

func TestHandleWorkerHeartbeat_ZeroAt_UsesServerTime(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "")

	before := time.Now().UTC()
	old := before.Add(-time.Hour)
	if _, err := st.RegisterWorker(t.Context(), store.Worker{
		ID: "w-1", FarmID: "farm-1", Status: store.WorkerStatusOnline, LastHeartbeatAt: &old,
	}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}

	msg := &fakeJSMsg{
		subject: bus.WorkerHeartbeatSubject("w-1"),
		data:    workerMsgJSON(t, protocol.HeartbeatMsg{Version: protocol.ProtocolVersion, WorkerID: "w-1"}), // zero At
	}
	s.handleWorkerMessage(msg)

	w, err := st.GetWorker(t.Context(), "w-1")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if w.LastHeartbeatAt == nil || w.LastHeartbeatAt.Before(before) {
		t.Errorf("LastHeartbeatAt = %v, want >= server time %v", w.LastHeartbeatAt, before)
	}
}

func TestHandleWorkerHeartbeat_UnknownWorker_Nacked(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "")

	msg := &fakeJSMsg{
		subject: bus.WorkerHeartbeatSubject("ghost"),
		data:    workerMsgJSON(t, protocol.HeartbeatMsg{Version: protocol.ProtocolVersion, WorkerID: "ghost", At: time.Now()}),
	}
	s.handleWorkerMessage(msg)

	if !msg.nacked {
		t.Error("heartbeat for unknown worker should nack for retry")
	}
}

// TestHandleWorkerHeartbeat_MalformedAndEmptyPayloadID_Acked covers a
// malformed body and a body with an empty worker_id. The latter has no
// separate "missing worker_id" code path any more: bus.ParseWorkerSubject
// guarantees the subject's worker token is never empty, so an empty
// m.WorkerID always fails the subject/payload mismatch check first.
func TestHandleWorkerHeartbeat_MalformedAndEmptyPayloadID_Acked(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"malformed", []byte("{bad")},
		{"empty payload id (mismatch)", nil}, // filled below
	}
	tests[1].data = workerMsgJSON(t, protocol.HeartbeatMsg{Version: protocol.ProtocolVersion, WorkerID: ""})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := fake.New()
			s := newMetricsScheduler(st, &recordBus{}, "")
			msg := &fakeJSMsg{subject: bus.WorkerHeartbeatSubject("w-1"), data: tt.data}
			s.handleWorkerMessage(msg)
			if !msg.acked {
				t.Errorf("%s heartbeat should be acked (discarded)", tt.name)
			}
		})
	}
}

// ── handleWorkerDeregister ────────────────────────────────────────────────────

func TestHandleWorkerDeregister_Valid(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "")

	now := time.Now().UTC()
	if _, err := st.RegisterWorker(t.Context(), store.Worker{
		ID: "w-1", FarmID: "farm-1", Status: store.WorkerStatusOnline, LastHeartbeatAt: &now,
	}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}

	msg := &fakeJSMsg{
		subject: bus.WorkerDeregisterSubject("w-1"),
		data:    workerMsgJSON(t, map[string]string{"worker_id": "w-1", "reason": "shutdown"}),
	}
	s.handleWorkerMessage(msg)

	if !msg.acked {
		t.Error("valid deregister should be acked")
	}
	w, err := st.GetWorker(t.Context(), "w-1")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if w.Status != store.WorkerStatusOffline {
		t.Errorf("worker status = %q, want offline", w.Status)
	}
}

// TestHandleWorkerDeregister_ReclaimsInFlightTasks verifies that a graceful
// deregister returns the worker's assigned/running tasks to the ready queue and
// closes their attempts. Without this, the heartbeat sweep (which only inspects
// online workers) would never recover them, stranding the tasks in 'assigned'.
func TestHandleWorkerDeregister_ReclaimsInFlightTasks(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "")
	ctx := t.Context()
	now := time.Now().UTC()

	const workerID = "w-bye"
	if _, err := st.RegisterWorker(ctx, store.Worker{
		ID: workerID, FarmID: "farm-1", Hostname: "node-bye",
		Status: store.WorkerStatusOnline, LastHeartbeatAt: &now,
	}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	job, err := st.CreateJob(ctx, store.Job{
		ID: uuid.NewString(), FarmID: "farm-1", QueueID: "queue-1", Name: "j",
		Status: store.JobStatusRunning, TemplateFormat: store.TemplateFormatJSON,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	step, err := st.CreateStep(ctx, store.Step{
		ID: uuid.NewString(), JobID: job.ID, Name: "s",
		Status: store.StepStatusRunning, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}
	task, err := st.CreateTask(ctx, store.Task{
		ID: uuid.NewString(), JobID: job.ID, StepID: step.ID, Name: "t",
		Status: store.TaskStatusAssigned, AssignedWorkerID: workerID,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	attempt, err := st.CreateTaskAttempt(ctx, store.TaskAttempt{
		ID: uuid.NewString(), TaskID: task.ID, WorkerID: workerID, AttemptNumber: 1,
		Status: store.AttemptStatusRunning, StartedAt: now, CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateTaskAttempt: %v", err)
	}

	msg := &fakeJSMsg{
		subject: bus.WorkerDeregisterSubject(workerID),
		data:    workerMsgJSON(t, map[string]string{"worker_id": workerID, "reason": "shutdown"}),
	}
	s.handleWorkerMessage(msg)

	tk, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if tk.Status != store.TaskStatusReady {
		t.Errorf("task status = %q, want ready (reclaimed on deregister)", tk.Status)
	}
	if tk.AssignedWorkerID != "" {
		t.Errorf("task still assigned to %q, want cleared", tk.AssignedWorkerID)
	}
	att, err := st.GetTaskAttempt(ctx, attempt.ID)
	if err != nil {
		t.Fatalf("GetTaskAttempt: %v", err)
	}
	if att.Status != store.AttemptStatusFailed {
		t.Errorf("attempt status = %q, want failed", att.Status)
	}
}

func TestHandleWorkerDeregister_UnknownWorker_Acked(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "")

	msg := &fakeJSMsg{
		subject: bus.WorkerDeregisterSubject("ghost"),
		data:    workerMsgJSON(t, map[string]string{"worker_id": "ghost"}),
	}
	s.handleWorkerMessage(msg)

	if !msg.acked {
		t.Error("deregister for unknown worker should be acked (benign race)")
	}
}

// TestHandleWorkerDeregister_MalformedAndEmptyPayloadID_Acked covers a
// malformed body and a body with an empty worker_id. The latter has no
// separate "missing worker_id" code path any more: bus.ParseWorkerSubject
// guarantees the subject's worker token is never empty, so an empty
// m.WorkerID always fails the subject/payload mismatch check first.
func TestHandleWorkerDeregister_MalformedAndEmptyPayloadID_Acked(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"malformed", []byte("{bad")},
		{"empty payload id (mismatch)", workerMsgJSON(t, map[string]string{"worker_id": ""})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := fake.New()
			s := newMetricsScheduler(st, &recordBus{}, "")
			msg := &fakeJSMsg{subject: bus.WorkerDeregisterSubject("w-1"), data: tt.data}
			s.handleWorkerMessage(msg)
			if !msg.acked {
				t.Errorf("%s deregister should be acked", tt.name)
			}
		})
	}
}

// ── ensureComputeLocation / auto-registration ─────────────────────────────────

func TestRegistration_AutoRegistersComputeLocation(t *testing.T) {
	ctx := context.Background()
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "")

	// Case 1: end-to-end — register a worker with a new location via
	// handleWorkerMessage; assert the entity is created in the store.
	msg := &fakeJSMsg{
		subject: bus.WorkerRegisterSubject("w-loc-1"),
		data: workerMsgJSON(t, protocol.RegisterMsg{
			Version: protocol.ProtocolVersion, Type: protocol.TypeRegister,
			WorkerID: "w-loc-1", FarmID: "farm-1", Hostname: "n1", OS: "linux",
			ComputeLocation: "render-hall",
		}),
	}
	s.handleWorkerMessage(msg)
	if !msg.acked {
		t.Fatal("valid register should be acked")
	}
	got, err := st.GetComputeLocationByName(ctx, "render-hall")
	if err != nil {
		t.Fatalf("expected location created: %v", err)
	}
	if got.Description != "" {
		t.Fatalf("auto-registered description = %q, want empty", got.Description)
	}

	// Case 2: existing location, same name — idempotent; description preserved.
	_, err = st.UpdateComputeLocation(ctx, store.ComputeLocation{
		ID: got.ID, Name: got.Name, Description: "curated",
	})
	if err != nil {
		t.Fatalf("seed description: %v", err)
	}
	s.ensureComputeLocation(ctx, "render-hall") // same case — must not create a duplicate
	locs, err := st.ListComputeLocations(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("location count = %d, want 1", len(locs))
	}
	if locs[0].Description != "curated" {
		t.Fatalf("description clobbered: %q", locs[0].Description)
	}

	// Case 3: empty location → no entity created.
	s.ensureComputeLocation(ctx, "")
	locs, err = st.ListComputeLocations(ctx)
	if err != nil {
		t.Fatalf("list after empty: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("empty location created an entity: %d", len(locs))
	}
}

// TestRegistration_EnsureComputeLocation_StoreError verifies that a store
// error during ensureComputeLocation does not prevent registration from
// succeeding (best-effort).
func TestRegistration_EnsureComputeLocation_StoreError(t *testing.T) {
	st := &computeLocationErrSt{Store: fake.New()}
	s := newMetricsScheduler(st, &recordBus{}, "")

	msg := &fakeJSMsg{
		subject: bus.WorkerRegisterSubject("w-loc-err"),
		data: workerMsgJSON(t, protocol.RegisterMsg{
			Version: protocol.ProtocolVersion, Type: protocol.TypeRegister,
			WorkerID: "w-loc-err", FarmID: "farm-1", Hostname: "n1", OS: "linux",
			ComputeLocation: "render-hall",
		}),
	}
	s.handleWorkerMessage(msg)

	if !msg.acked {
		t.Error("registration must be acked even when ensureComputeLocation fails")
	}
	if msg.nacked {
		t.Error("registration must not be nacked when only ensureComputeLocation fails")
	}
}

// computeLocationErrSt wraps the fake store and makes GetComputeLocationByName
// return ErrNotFound (so ensureComputeLocation proceeds to Create) and
// CreateComputeLocation return an error, exercising the create-failure
// best-effort path.
type computeLocationErrSt struct {
	store.Store
}

func (*computeLocationErrSt) GetComputeLocationByName(_ context.Context, _ string) (store.ComputeLocation, error) {
	return store.ComputeLocation{}, store.ErrNotFound // proceed to Create
}

func (*computeLocationErrSt) CreateComputeLocation(_ context.Context, _ store.ComputeLocation) (store.ComputeLocation, error) {
	return store.ComputeLocation{}, context.DeadlineExceeded
}

// computeLocationLookupErrSt wraps the fake store and makes
// GetComputeLocationByName fail with a non-ErrNotFound error, exercising the
// lookup-failure best-effort path (ensureComputeLocation returns early without
// calling Create).
type computeLocationLookupErrSt struct {
	store.Store
}

func (*computeLocationLookupErrSt) GetComputeLocationByName(_ context.Context, _ string) (store.ComputeLocation, error) {
	return store.ComputeLocation{}, context.DeadlineExceeded
}

// TestRegistration_EnsureComputeLocation_LookupError verifies that a lookup
// failure in ensureComputeLocation does not prevent the worker registration
// from succeeding (best-effort).
func TestRegistration_EnsureComputeLocation_LookupError(t *testing.T) {
	st := &computeLocationLookupErrSt{Store: fake.New()}
	s := newMetricsScheduler(st, &recordBus{}, "")

	msg := &fakeJSMsg{
		subject: bus.WorkerRegisterSubject("w-loc-err2"),
		data: workerMsgJSON(t, protocol.RegisterMsg{
			Version: protocol.ProtocolVersion, Type: protocol.TypeRegister,
			WorkerID: "w-loc-err2", FarmID: "farm-1", Hostname: "n1", OS: "linux",
			ComputeLocation: "render-hall",
		}),
	}
	s.handleWorkerMessage(msg)

	if !msg.acked {
		t.Error("registration must be acked even when compute location lookup fails")
	}
	if msg.nacked {
		t.Error("registration must not be nacked when only ensureComputeLocation fails")
	}
}

// ── handleWorkerMessage routing ───────────────────────────────────────────────

func TestHandleWorkerMessage_UnknownSubject_Acked(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "")

	msg := &fakeJSMsg{subject: "worker.bogus", data: []byte("{}")}
	s.handleWorkerMessage(msg)

	if !msg.acked {
		t.Error("unknown subject should be acked")
	}
}

func TestHandleWorkerMessage_RegisterID(t *testing.T) {
	// Sanity check that the subject constants used in routing are distinct and
	// the register path actually creates a worker via the router (not a direct
	// handler call).
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "")
	id := uuid.NewString()

	msg := &fakeJSMsg{
		subject: bus.WorkerRegisterSubject(id),
		data:    workerMsgJSON(t, protocol.RegisterMsg{Version: protocol.ProtocolVersion, WorkerID: id, FarmID: "farm-1"}),
	}
	s.handleWorkerMessage(msg)

	if _, err := st.GetWorker(t.Context(), id); err != nil {
		t.Errorf("worker %q not registered via router: %v", id, err)
	}
}
