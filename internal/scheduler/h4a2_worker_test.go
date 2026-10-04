// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"encoding/json"
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

// leaseAs sends one lease request for workerID on q1 carrying instance, and
// returns the assigned task IDs and how long the handler took to answer.
func leaseAs(t *testing.T, s *Scheduler, workerID, instance string) ([]string, time.Duration) {
	t.Helper()
	req, err := json.Marshal(leaseRequest{WorkerID: workerID, InstanceID: instance})
	if err != nil {
		t.Fatalf("marshal lease request: %v", err)
	}
	start := time.Now()
	raw := s.handleLeaseRequest(workerID, "q1", req)
	took := time.Since(start)
	var rep leaseReply
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("unmarshal lease reply: %v", err)
	}
	ids := make([]string, 0, len(rep.Assignments))
	for _, a := range rep.Assignments {
		var m protocol.AssignMsg
		if err := json.Unmarshal(a, &m); err != nil {
			t.Fatalf("unmarshal assignment: %v", err)
		}
		ids = append(ids, m.TaskID)
	}
	return ids, took
}

func mustRegisterInstance(t *testing.T, st store.Store, w store.Worker, instance string) []store.Task {
	t.Helper()
	w.InstanceID = instance
	now := time.Now().UTC()
	w.LastHeartbeatAt = &now
	_, reclaimed, err := st.RegisterWorker(t.Context(), w)
	if err != nil {
		t.Fatalf("RegisterWorker(%q): %v", instance, err)
	}
	return reclaimed
}

// TestH4a2_LeaseHeldBackUntilARestartedProcessRegisters pins the lease side of
// the restart reclaim (H4a2 §4.5): a restarted worker process can ask for work
// before the server has consumed its new registration, and a task leased to it
// then would be handed back to ready by that registration's reclaim (which
// matches by worker ID) while the process runs it. A request whose instance ID
// differs from the stored one is answered with no work, at once and without
// touching a task; one whose ID matches, or that carries none, is served; and
// once the new registration lands the new process is served, and what it leases
// is not reclaimed by a later re-register of the same process.
func TestH4a2_LeaseHeldBackUntilARestartedProcessRegisters(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			s := newMetricsScheduler(st, &recordBus{}, "f1")
			s.ctx = t.Context()
			s.cfg.AssignBatchSize = 1             // one task per served request
			s.leaseHoldTimeout = 30 * time.Second // a park would show as a slow answer
			s.leaseRefusalDelay = 50 * time.Millisecond
			one := 1
			w, ids := seedLeaseFixture(t, st, []*int{&one, &one, &one})
			if r := mustRegisterInstance(t, st, w, "p1"); len(r) != 0 {
				t.Fatalf("first instance register reclaimed %d", len(r))
			}

			// A process whose registration has not landed: no work, held for
			// the refusal delay (so its lease loop cannot spin) but not parked.
			got, took := leaseAs(t, s, w.ID, "p2")
			if len(got) != 0 {
				t.Fatalf("lease as p2 before its registration = %v, want no assignments", got)
			}
			if took < s.leaseRefusalDelay {
				t.Fatalf("lease as p2 answered in %v, want at least the refusal delay %v", took, s.leaseRefusalDelay)
			}
			if took > 5*time.Second {
				t.Fatalf("lease as p2 took %v: the refusal parked instead of answering after the delay", took)
			}
			for _, id := range ids {
				if task := mustTaskOf(t, st, id); task.Status != store.TaskStatusReady || task.AssignedWorkerID != "" {
					t.Fatalf("task %s = %q on %q after the refused lease, want ready and unassigned", id, task.Status, task.AssignedWorkerID)
				}
				attempts, err := st.ListTaskAttempts(t.Context(), id)
				if err != nil {
					t.Fatalf("ListTaskAttempts: %v", err)
				}
				if len(attempts) != 0 {
					t.Fatalf("task %s has %d attempts after the refused lease, want 0", id, len(attempts))
				}
			}

			// The registered process, and a worker that sends no ID: served.
			if got, _ := leaseAs(t, s, w.ID, "p1"); len(got) != 1 {
				t.Fatalf("lease as the registered p1 = %v, want one assignment", got)
			}
			if got, _ := leaseAs(t, s, w.ID, ""); len(got) != 1 {
				t.Fatalf("lease with no instance ID = %v, want one assignment", got)
			}

			// p2's registration lands: the two tasks leased before it belong to
			// the previous process and are reclaimed; then p2 is served.
			if r := mustRegisterInstance(t, st, w, "p2"); len(r) != 2 {
				t.Fatalf("p2 register reclaimed %d, want the 2 tasks leased before it", len(r))
			}
			fresh, _ := leaseAs(t, s, w.ID, "p2")
			if len(fresh) != 1 {
				t.Fatalf("lease as p2 after its registration = %v, want one assignment", fresh)
			}
			// A reconnect re-register of p2 reclaims nothing: the task p2 leased
			// stays with it.
			if r := mustRegisterInstance(t, st, w, "p2"); len(r) != 0 {
				t.Fatalf("p2 re-register reclaimed %+v, want nothing", r)
			}
			if task := mustTaskOf(t, st, fresh[0]); task.Status != store.TaskStatusAssigned || task.AssignedWorkerID != w.ID {
				t.Fatalf("p2's task = %q on %q, want still assigned to %s", task.Status, task.AssignedWorkerID, w.ID)
			}
		})
	}
}

// TestH4a2_ParkedLeaseFromASupersededProcessGetsNoWork pins the same rule on
// the parked path: a request that parked while its process was the registered
// one, and is woken after another process of the worker has registered, gets
// no work, so a reclaimed task is not handed to the process that is gone.
func TestH4a2_ParkedLeaseFromASupersededProcessGetsNoWork(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			s := newMetricsScheduler(st, &recordBus{}, "f1")
			s.ctx = t.Context()
			s.leaseHoldTimeout = 30 * time.Second
			one := 1
			w, ids := seedLeaseFixture(t, st, []*int{&one})
			mustRegisterInstance(t, st, w, "p1")
			if got, _ := leaseAs(t, s, w.ID, "p1"); len(got) != 1 {
				t.Fatalf("lease as p1 = %v, want the one task", got)
			}

			// p1 asks again with nothing ready, and parks. The goroutine only
			// calls the handler; the reply is decoded on the test goroutine.
			req, err := json.Marshal(leaseRequest{WorkerID: w.ID, InstanceID: "p1"})
			if err != nil {
				t.Fatalf("marshal lease request: %v", err)
			}
			done := make(chan []byte, 1)
			go func() { done <- s.handleLeaseRequest(w.ID, "q1", req) }()
			deadline := time.Now().Add(5 * time.Second)
			for {
				s.waiters.mu.Lock()
				parked := len(s.waiters.waiters["q1"])
				s.waiters.mu.Unlock()
				if parked > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("p1's second lease request never parked")
				}
				time.Sleep(time.Millisecond)
			}

			// p2 registers: p1's task is reclaimed to ready, and the parked
			// request is woken with work available.
			if r := mustRegisterInstance(t, st, w, "p2"); len(r) != 1 {
				t.Fatalf("p2 register reclaimed %d, want p1's task", len(r))
			}
			s.waiters.notifyAll()
			select {
			case raw := <-done:
				var rep leaseReply
				if err := json.Unmarshal(raw, &rep); err != nil {
					t.Fatalf("unmarshal lease reply: %v", err)
				}
				if len(rep.Assignments) != 0 {
					t.Fatalf("woken lease from the superseded p1 = %s, want no assignments", raw)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the parked lease request never returned")
			}
			if task := mustTaskOf(t, st, ids[0]); task.Status != store.TaskStatusReady {
				t.Fatalf("reclaimed task = %q, want still ready", task.Status)
			}
		})
	}
}

// TestH4a2_HeldLeaseRefusalEndsWithTheScheduler pins that the refusal's hold
// (leaseRefusalDelay) gives way to the scheduler's context: a request held
// when the scheduler shuts down is answered, empty, at once.
func TestH4a2_HeldLeaseRefusalEndsWithTheScheduler(t *testing.T) {
	st := newCheckedFake(t)
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.ctx = ctx
	s.leaseRefusalDelay = time.Minute
	one := 1
	w, ids := seedLeaseFixture(t, st, []*int{&one})
	mustRegisterInstance(t, st, w, "p1")

	req, err := json.Marshal(leaseRequest{WorkerID: w.ID, InstanceID: "p2"})
	if err != nil {
		t.Fatalf("marshal lease request: %v", err)
	}
	done := make(chan []byte, 1)
	go func() { done <- s.handleLeaseRequest(w.ID, "q1", req) }()
	time.Sleep(20 * time.Millisecond) // let the request reach its hold
	cancel()

	select {
	case raw := <-done:
		var rep leaseReply
		if err := json.Unmarshal(raw, &rep); err != nil {
			t.Fatalf("unmarshal lease reply: %v", err)
		}
		if len(rep.Assignments) != 0 {
			t.Fatalf("held refusal answered %s, want no assignments", raw)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a held refusal did not return when the scheduler's context ended")
	}
	if task := mustTaskOf(t, st, ids[0]); task.Status != store.TaskStatusReady {
		t.Fatalf("task = %q, want still ready", task.Status)
	}
}

// TestH4a2_DisabledWorkerGetsNoWork pins N1: docs/api.md says disable "stops
// new assignments", but the lease path never checked worker status. The refusal
// is held for leaseRefusalDelay, like the unregistered-instance one: a worker
// re-requests the moment a reply arrives and can stay disabled for days, so an
// instant empty reply would spin its lease loop against the broker and the
// store for that long.
func TestH4a2_DisabledWorkerGetsNoWork(t *testing.T) {
	st := newCheckedFake(t)
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	s.leaseHoldTimeout = 30 * time.Second // a park would show as a slow answer
	s.leaseRefusalDelay = 50 * time.Millisecond
	one := 1
	w, ids := seedLeaseFixture(t, st, []*int{&one})
	if err := st.UpdateWorkerStatus(t.Context(), w.ID, store.WorkerStatusDisabled); err != nil {
		t.Fatalf("UpdateWorkerStatus: %v", err)
	}
	got, took := leaseAs(t, s, w.ID, "")
	if len(got) != 0 {
		t.Fatalf("assignments = %v, want none for a disabled worker", got)
	}
	if took < s.leaseRefusalDelay {
		t.Fatalf("refusal answered in %v, want at least the refusal delay %v", took, s.leaseRefusalDelay)
	}
	if took > 5*time.Second {
		t.Fatalf("refusal took %v: it parked instead of answering after the delay", took)
	}
	if task := mustTaskOf(t, st, ids[0]); task.Status != store.TaskStatusReady || task.AssignedWorkerID != "" {
		t.Fatalf("task = %q on %q, want ready and unassigned (not leased)", task.Status, task.AssignedWorkerID)
	}
	attempts, err := st.ListTaskAttempts(t.Context(), ids[0])
	if err != nil {
		t.Fatalf("ListTaskAttempts: %v", err)
	}
	if len(attempts) != 0 {
		t.Fatalf("task has %d attempts after the refused lease, want 0", len(attempts))
	}
}

// TestH4a2_ParkedLeaseOfAWorkerDisabledMeanwhileGetsNoWork pins the same rule
// on the parked path: a request that parked while its worker was online, and is
// woken after an operator disabled the worker, gets no work even though a task
// is ready and the worker has the cores for it.
func TestH4a2_ParkedLeaseOfAWorkerDisabledMeanwhileGetsNoWork(t *testing.T) {
	st := newCheckedFake(t)
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	s.ctx = t.Context()
	s.leaseHoldTimeout = 30 * time.Second
	one := 1
	w, ids := seedLeaseFixture(t, st, []*int{&one})

	// The worker takes the one ready task, so its next request finds nothing
	// ready and parks. It is online and has three cores free, so the only thing
	// that can keep it from the task added below is its status.
	if got, _ := leaseAs(t, s, w.ID, ""); len(got) != 1 {
		t.Fatalf("first lease = %v, want the one task", got)
	}
	req, err := json.Marshal(leaseRequest{WorkerID: w.ID})
	if err != nil {
		t.Fatalf("marshal lease request: %v", err)
	}
	done := make(chan []byte, 1)
	go func() { done <- s.handleLeaseRequest(w.ID, "q1", req) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.waiters.mu.Lock()
		parked := len(s.waiters.waiters["q1"])
		s.waiters.mu.Unlock()
		if parked > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the second lease request never parked")
		}
		time.Sleep(time.Millisecond)
	}

	// While it is parked the worker is disabled and a new task becomes ready.
	if err := st.UpdateWorkerStatus(t.Context(), w.ID, store.WorkerStatusDisabled); err != nil {
		t.Fatalf("UpdateWorkerStatus: %v", err)
	}
	held := mustTaskOf(t, st, ids[0])
	now := time.Now().UTC()
	fresh, err := st.CreateTask(t.Context(), store.Task{
		ID: "t-after-disable", JobID: held.JobID, StepID: held.StepID,
		Name: "t", Status: store.TaskStatusReady, Parameters: map[string]string{},
		RequiredCores: &one, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	s.waiters.notifyAll()

	select {
	case raw := <-done:
		var rep leaseReply
		if err := json.Unmarshal(raw, &rep); err != nil {
			t.Fatalf("unmarshal lease reply: %v", err)
		}
		if len(rep.Assignments) != 0 {
			t.Fatalf("woken lease of a disabled worker = %s, want no assignments", raw)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the parked lease request never returned")
	}
	if task := mustTaskOf(t, st, fresh.ID); task.Status != store.TaskStatusReady {
		t.Fatalf("task = %q, want ready (not leased to the disabled worker)", task.Status)
	}
}

// TestH4a2_DeregisterOfADisabledWorkerSaysDisabled is Review Focus 4: a
// graceful deregister of a disabled worker still reclaims its task, the store
// keeps the worker disabled (H4a2 §5.3), and the worker event says so rather
// than announcing an offline the row does not hold.
func TestH4a2_DeregisterOfADisabledWorkerSaysDisabled(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			workerID, taskID, _ := seedStaleWorkerWithTask(t, st, 0)
			if err := st.UpdateWorkerStatus(t.Context(), workerID, store.WorkerStatusDisabled); err != nil {
				t.Fatalf("UpdateWorkerStatus: %v", err)
			}
			rec := &workerRecordingNotifier{}
			s := newMetricsScheduler(st, &recordBus{}, "")
			s.notifier = rec
			s.ctx = t.Context()
			msg := &fakeJSMsg{subject: bus.WorkerDeregisterSubject(workerID), data: workerMsgJSON(t, protocol.DeregisterMsg{
				Version: protocol.ProtocolVersion, Type: protocol.TypeDeregister, WorkerID: workerID,
			})}
			s.handleWorkerMessage(msg)
			if !msg.acked {
				t.Fatal("the deregister was not acked")
			}
			if got := mustTaskOf(t, st, taskID).Status; got != store.TaskStatusReady {
				t.Fatalf("task = %q, want ready (reclaimed)", got)
			}
			w, err := st.GetWorker(t.Context(), workerID)
			if err != nil {
				t.Fatalf("GetWorker: %v", err)
			}
			if w.Status != store.WorkerStatusDisabled {
				t.Fatalf("worker = %q, want disabled (kept)", w.Status)
			}
			if len(rec.workers) != 1 || rec.workers[0].WorkerID != workerID || rec.workers[0].Status != string(store.WorkerStatusDisabled) {
				t.Fatalf("worker events = %+v, want one saying %s is disabled", rec.workers, workerID)
			}
		})
	}
}

// TestH4a2_DeregisterOfAnOnlineWorkerSaysOffline is the control for the test
// above: the deregister event reads the stored status, which for any worker
// that is not disabled is offline.
func TestH4a2_DeregisterOfAnOnlineWorkerSaysOffline(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			workerID, _, _ := seedStaleWorkerWithTask(t, st, 0)
			rec := &workerRecordingNotifier{}
			s := newMetricsScheduler(st, &recordBus{}, "")
			s.notifier = rec
			s.ctx = t.Context()
			s.handleWorkerMessage(&fakeJSMsg{subject: bus.WorkerDeregisterSubject(workerID), data: workerMsgJSON(t, protocol.DeregisterMsg{
				Version: protocol.ProtocolVersion, Type: protocol.TypeDeregister, WorkerID: workerID,
			})})
			if len(rec.workers) != 1 || rec.workers[0].Status != string(store.WorkerStatusOffline) {
				t.Fatalf("worker events = %+v, want one saying offline", rec.workers)
			}
		})
	}
}

// TestH4a2_RegisterOfADisabledWorkerSaysDisabled pins the registration half of
// H4a2 §5.3 end to end, on the real stores rather than a stand-in: a disabled
// worker that re-registers (any NATS reconnect) stays disabled, and the worker
// event carries that, not the online the registration asked for.
func TestH4a2_RegisterOfADisabledWorkerSaysDisabled(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			workerID, _, _ := seedStaleWorkerWithTask(t, st, 0)
			if err := st.UpdateWorkerStatus(t.Context(), workerID, store.WorkerStatusDisabled); err != nil {
				t.Fatalf("UpdateWorkerStatus: %v", err)
			}
			rec := &workerRecordingNotifier{}
			s := newMetricsScheduler(st, &recordBus{}, "")
			s.notifier = rec
			s.ctx = t.Context()
			msg := registerMsg(t, workerID, "")
			s.handleWorkerMessage(msg)
			if !msg.acked {
				t.Fatal("the registration was not acked")
			}
			w, err := st.GetWorker(t.Context(), workerID)
			if err != nil {
				t.Fatalf("GetWorker: %v", err)
			}
			if w.Status != store.WorkerStatusDisabled {
				t.Fatalf("worker = %q, want disabled (kept)", w.Status)
			}
			if len(rec.workers) != 1 || rec.workers[0].Status != string(store.WorkerStatusDisabled) {
				t.Fatalf("worker events = %+v, want one saying disabled", rec.workers)
			}
		})
	}
}

// TestH4a2_SweepReclaimsADeadDisabledWorkerWithoutAnOfflineEvent pins item 9
// iv: a disabled worker that dies holding a task has the task reclaimed by the
// heartbeat sweep, stays disabled, and is announced by no worker event.
func TestH4a2_SweepReclaimsADeadDisabledWorkerWithoutAnOfflineEvent(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			workerID, taskID, _ := seedStaleWorkerWithTask(t, st, time.Hour)
			if err := st.UpdateWorkerStatus(t.Context(), workerID, store.WorkerStatusDisabled); err != nil {
				t.Fatalf("UpdateWorkerStatus: %v", err)
			}
			rec := &workerRecordingNotifier{}
			s := newMetricsScheduler(st, &recordBus{}, "")
			s.notifier = rec
			s.sweepStaleWorkers(t.Context())
			if got := mustTaskOf(t, st, taskID).Status; got != store.TaskStatusReady {
				t.Fatalf("task = %q, want ready", got)
			}
			w, err := st.GetWorker(t.Context(), workerID)
			if err != nil {
				t.Fatalf("GetWorker: %v", err)
			}
			if w.Status != store.WorkerStatusDisabled {
				t.Fatalf("worker = %q, want disabled", w.Status)
			}
			if len(rec.workers) != 0 {
				t.Fatalf("worker events = %+v, want none", rec.workers)
			}
		})
	}
}
