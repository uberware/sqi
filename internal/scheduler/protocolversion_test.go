// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for the receiver-side wire-protocol version gate on the three worker →
// server message types the server ACTS ON: worker.register, worker.heartbeat
// and task.status.<job>.
//
// WHY A GATE AT ALL. encoding/json silently drops every field the receiving
// struct does not declare, so a message that merely decodes is not evidence
// that sender and receiver agree on its shape — a worker built against a
// different protocol revision hands the server a half-understood message and
// the server acts on it as if nothing were missing. This mirrors the check the
// worker already applies to the assignments it receives
// (internal/worker/lease.decodeAssignment), which is where the reasoning is
// written out in full.
//
// These tests drive the real handlers through real JSON bytes. A version gate
// that is bypassed by the way a test constructs its input tests nothing.
//
// The positive controls live next to the behavior they protect rather than
// here: TestHandleWorkerRegister_Valid, TestHandleWorkerHeartbeat_Valid and
// the whole of taskstatus_test.go all carry the current version, so a gate
// inverted to reject the matching version fails them loudly.

import (
	"errors"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// mismatchedVersions are the three shapes a real farm produces: a worker older
// than this server, a worker newer than it, and a payload carrying no envelope
// at all (a hand-rolled publisher, or a field renamed out from under the
// sender). The empty case matters most — it is the one a reader assumes is
// handled and the one an `if version != "" && version != current` gate lets
// through.
var mismatchedVersions = []string{"1", "3", ""}

// ── worker.register ───────────────────────────────────────────────────────────

// TestHandleWorkerRegister_VersionMismatch_Discarded pins the highest-value
// half of the gate: refusing the registration is what actually fences the
// worker out, because handleLeaseRequest looks the worker up in the store and
// replies empty when it is absent. A worker whose registration is refused is
// therefore never offered work at all, rather than being offered work it will
// reject one assignment at a time while the tasks churn through reclaim.
func TestHandleWorkerRegister_VersionMismatch_Discarded(t *testing.T) {
	for _, v := range mismatchedVersions {
		t.Run("version="+v, func(t *testing.T) {
			st := fake.New()
			s := newMetricsScheduler(st, &recordBus{}, "")

			msg := &fakeJSMsg{
				subject: bus.SubjectWorkerRegister,
				data: workerMsgJSON(t, protocol.RegisterMsg{
					Version:  v,
					Type:     protocol.TypeRegister,
					WorkerID: "w-1",
					FarmID:   "farm-1",
					Hostname: "node-1",
					OS:       "linux",
					CPUCount: 8,
				}),
			}
			s.handleWorkerMessage(msg)

			if _, err := st.GetWorker(t.Context(), "w-1"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("GetWorker after a version-%q registration = %v, want ErrNotFound. "+
					"A worker whose registration this server cannot read must not be recorded "+
					"online: the lease path selects work for whatever the store says is there.",
					v, err)
			}
			if !msg.acked {
				t.Error("a version-mismatched registration must be acked (discarded): " +
					"redelivery cannot change the version the message was published with")
			}
			if msg.nacked {
				t.Error("a version-mismatched registration must not be nacked into a redelivery loop")
			}
		})
	}
}

// ── worker.heartbeat ──────────────────────────────────────────────────────────

// TestHandleWorkerHeartbeat_VersionMismatch_Discarded seeds a worker that is
// already registered and online, so the store write the gate has to prevent is
// one that would otherwise succeed. Leaving LastHeartbeatAt untouched is what
// lets the heartbeat sweep retire a worker the server can no longer talk to
// and reclaim its in-flight tasks — the gate does not need its own
// mark-offline path.
func TestHandleWorkerHeartbeat_VersionMismatch_Discarded(t *testing.T) {
	for _, v := range mismatchedVersions {
		t.Run("version="+v, func(t *testing.T) {
			st := fake.New()
			s := newMetricsScheduler(st, &recordBus{}, "")

			seeded := time.Now().UTC().Add(-time.Hour)
			if _, err := st.RegisterWorker(t.Context(), store.Worker{
				ID: "w-1", FarmID: "farm-1",
				Status: store.WorkerStatusOnline, LastHeartbeatAt: &seeded,
			}); err != nil {
				t.Fatalf("RegisterWorker: %v", err)
			}

			msg := &fakeJSMsg{
				subject: bus.SubjectWorkerHeartbeat,
				data: workerMsgJSON(t, protocol.HeartbeatMsg{
					Version:  v,
					Type:     protocol.TypeHeartbeat,
					WorkerID: "w-1",
					At:       seeded.Add(time.Hour),
				}),
			}
			s.handleWorkerMessage(msg)

			w, err := st.GetWorker(t.Context(), "w-1")
			if err != nil {
				t.Fatalf("GetWorker: %v", err)
			}
			if w.LastHeartbeatAt == nil || !w.LastHeartbeatAt.Equal(seeded) {
				t.Errorf("LastHeartbeatAt = %v after a version-%q heartbeat, want the seeded %v "+
					"unchanged: a liveness signal the server cannot read must not keep a worker alive",
					w.LastHeartbeatAt, v, seeded)
			}
			if !msg.acked {
				t.Error("a version-mismatched heartbeat must be acked (discarded)")
			}
			if msg.nacked {
				t.Error("a version-mismatched heartbeat must not be nacked: it will never become readable")
			}
		})
	}
}

// ── task.status.<job> ─────────────────────────────────────────────────────────

// TestHandleTaskStatusMessage_VersionMismatch_Discarded is the one with a
// cost: a terminal status discarded here means the server never learns the
// task finished and the reaper eventually re-runs it. That is the accepted
// trade — this message carries the OUTCOME (exit code, session, timestamps),
// so it is the message where acting on a half-understood payload does the most
// damage. The task is left exactly as it was found.
func TestHandleTaskStatusMessage_VersionMismatch_Discarded(t *testing.T) {
	for _, v := range mismatchedVersions {
		t.Run("version="+v, func(t *testing.T) {
			st := fake.New()
			s := newStatusTestScheduler(st)
			job, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusAssigned)

			msg := &fakeJSMsg{
				subject: "task.status." + job.ID,
				data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
					Version:   v,
					Type:      protocol.TypeTaskStatus,
					TaskID:    task.ID,
					AttemptID: attempt.ID,
					JobID:     job.ID,
					Status:    string(store.TaskStatusRunning),
					At:        time.Now().UTC(),
				}),
			}
			s.handleTaskStatusMessage(msg)

			got, err := st.GetTask(t.Context(), task.ID)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if got.Status != store.TaskStatusAssigned {
				t.Errorf("task status = %q after a version-%q status message, want %q unchanged",
					got.Status, v, store.TaskStatusAssigned)
			}
			if !msg.acked {
				t.Error("a version-mismatched task status must be acked (discarded)")
			}
			if msg.nacked {
				t.Error("a version-mismatched task status must not be nacked into a redelivery loop")
			}
		})
	}
}
