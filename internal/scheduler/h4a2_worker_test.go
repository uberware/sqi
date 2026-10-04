// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/worker/protocol"
)

func registerMsg(t *testing.T, workerID, instance string) *fakeJSMsg {
	t.Helper()
	return &fakeJSMsg{subject: bus.WorkerRegisterSubject(workerID), data: workerMsgJSON(t, protocol.RegisterMsg{
		Version: protocol.ProtocolVersion, Type: protocol.TypeRegister,
		WorkerID: workerID, FarmID: "farm-1", Hostname: "node-stale", InstanceID: instance,
	})}
}

// TestH4a2_RestartedWorkerHasItsTasksReclaimed pins item 9 viii (first half)
// end to end: a worker that restarts and re-registers within WorkerTimeout no
// longer leaves its previous process's task running forever. The reclaim is
// reported through reclaimOfflineWorkerTasks, so a parked lease waiter is woken
// for the task that came back to ready, and every registration's worker event
// carries the status of the row the store returned (online here).
func TestH4a2_RestartedWorkerHasItsTasksReclaimed(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			workerID, taskID, _ := seedStaleWorkerWithTask(t, st, 0)
			s := newMetricsScheduler(st, &recordBus{}, "")
			s.ctx = t.Context()
			rec := &workerRecordingNotifier{}
			s.notifier = rec

			s.handleWorkerMessage(registerMsg(t, workerID, "p1"))
			if got := mustTaskOf(t, st, taskID).Status; got != store.TaskStatusRunning {
				t.Fatalf("after the first instance register: task = %q, want running", got)
			}
			s.handleWorkerMessage(registerMsg(t, workerID, "p1")) // reconnect
			if got := mustTaskOf(t, st, taskID).Status; got != store.TaskStatusRunning {
				t.Fatalf("after a reconnect: task = %q, want running", got)
			}

			woke := parkWaiter(t, s, "queue-1")
			restart := registerMsg(t, workerID, "p2")
			s.handleWorkerMessage(restart) // restart
			if !restart.acked {
				t.Fatal("the restart registration was not acked")
			}
			if got := mustTaskOf(t, st, taskID).Status; got != store.TaskStatusReady {
				t.Fatalf("after a restart: task = %q, want ready", got)
			}
			select {
			case got := <-woke:
				if !got {
					t.Fatal("parked lease waiter not woken by the restart reclaim")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("parked lease waiter never returned after the restart reclaim")
			}

			if len(rec.workers) != 3 {
				t.Fatalf("worker events = %+v, want one per registration (3)", rec.workers)
			}
			for i, e := range rec.workers {
				if e.WorkerID != workerID || e.Status != string(store.WorkerStatusOnline) {
					t.Errorf("worker event %d = %+v, want %s online", i, e, workerID)
				}
			}
		})
	}
}

// registerStatusSt returns, from RegisterWorker, the stored row with its
// status replaced, standing in for a store that keeps a status the
// registration did not ask for (H4a2 §5.3 keeps disabled).
type registerStatusSt struct {
	store.Store

	status store.WorkerStatus
}

func (r *registerStatusSt) RegisterWorker(ctx context.Context, w store.Worker) (store.Worker, []store.Task, error) {
	out, reclaimed, err := r.Store.RegisterWorker(ctx, w)
	out.Status = r.status
	return out, reclaimed, err
}

// TestHandleWorkerRegister_EventCarriesTheStoredStatus pins that the register
// handler's worker event reports the status of the row RegisterWorker
// returned, not the "online" the registration asked for.
func TestHandleWorkerRegister_EventCarriesTheStoredStatus(t *testing.T) {
	st := &registerStatusSt{Store: newCheckedFake(t), status: store.WorkerStatusDisabled}
	s := newMetricsScheduler(st, &recordBus{}, "")
	rec := &workerRecordingNotifier{}
	s.notifier = rec

	s.handleWorkerMessage(registerMsg(t, "w-1", "p1"))

	if len(rec.workers) != 1 || rec.workers[0].Status != string(store.WorkerStatusDisabled) {
		t.Fatalf("worker events = %+v, want one carrying the stored status %q", rec.workers, store.WorkerStatusDisabled)
	}
}
