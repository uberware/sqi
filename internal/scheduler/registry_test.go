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
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
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
		subject: bus.SubjectWorkerRegister,
		data: workerMsgJSON(t, RegisterMsg{
			WorkerID: "w-1", FarmID: "farm-1", Name: "worker-2", Hostname: "node-1", OS: "linux",
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
	if w.LastHeartbeatAt == nil {
		t.Error("expected LastHeartbeatAt set on registration")
	}
}

func TestHandleWorkerRegister_MalformedJSON_Acked(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "")

	msg := &fakeJSMsg{subject: bus.SubjectWorkerRegister, data: []byte("{bad")}
	s.handleWorkerMessage(msg)

	if !msg.acked {
		t.Error("malformed register should be acked (discarded)")
	}
}

func TestHandleWorkerRegister_MissingWorkerID_Acked(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "")

	msg := &fakeJSMsg{
		subject: bus.SubjectWorkerRegister,
		data:    workerMsgJSON(t, RegisterMsg{WorkerID: "", FarmID: "farm-1"}),
	}
	s.handleWorkerMessage(msg)

	if !msg.acked {
		t.Error("register missing worker_id should be acked")
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
		subject: bus.SubjectWorkerRegister,
		data:    workerMsgJSON(t, RegisterMsg{WorkerID: "w-1", FarmID: "farm-1"}),
	}
	s.handleWorkerMessage(msg)

	if !msg.nacked {
		t.Error("store error on register should nack for redelivery")
	}
	if msg.acked {
		t.Error("message should not be acked when register fails")
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
		subject: bus.SubjectWorkerHeartbeat,
		data:    workerMsgJSON(t, HeartbeatMsg{WorkerID: "w-1", At: hbAt}),
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
		subject: bus.SubjectWorkerHeartbeat,
		data:    workerMsgJSON(t, HeartbeatMsg{WorkerID: "w-1"}), // zero At
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
		subject: bus.SubjectWorkerHeartbeat,
		data:    workerMsgJSON(t, HeartbeatMsg{WorkerID: "ghost", At: time.Now()}),
	}
	s.handleWorkerMessage(msg)

	if !msg.nacked {
		t.Error("heartbeat for unknown worker should nack for retry")
	}
}

func TestHandleWorkerHeartbeat_MalformedAndMissingID_Acked(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"malformed", []byte("{bad")},
		{"missing id", nil}, // filled below
	}
	tests[1].data = workerMsgJSON(t, HeartbeatMsg{WorkerID: ""})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := fake.New()
			s := newMetricsScheduler(st, &recordBus{}, "")
			msg := &fakeJSMsg{subject: bus.SubjectWorkerHeartbeat, data: tt.data}
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
		subject: bus.SubjectWorkerDeregister,
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

func TestHandleWorkerDeregister_UnknownWorker_Acked(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "")

	msg := &fakeJSMsg{
		subject: bus.SubjectWorkerDeregister,
		data:    workerMsgJSON(t, map[string]string{"worker_id": "ghost"}),
	}
	s.handleWorkerMessage(msg)

	if !msg.acked {
		t.Error("deregister for unknown worker should be acked (benign race)")
	}
}

func TestHandleWorkerDeregister_MalformedAndMissingID_Acked(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"malformed", []byte("{bad")},
		{"missing id", workerMsgJSON(t, map[string]string{"worker_id": ""})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := fake.New()
			s := newMetricsScheduler(st, &recordBus{}, "")
			msg := &fakeJSMsg{subject: bus.SubjectWorkerDeregister, data: tt.data}
			s.handleWorkerMessage(msg)
			if !msg.acked {
				t.Errorf("%s deregister should be acked", tt.name)
			}
		})
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
		subject: bus.SubjectWorkerRegister,
		data:    workerMsgJSON(t, RegisterMsg{WorkerID: id, FarmID: "farm-1"}),
	}
	s.handleWorkerMessage(msg)

	if _, err := st.GetWorker(t.Context(), id); err != nil {
		t.Errorf("worker %q not registered via router: %v", id, err)
	}
}
