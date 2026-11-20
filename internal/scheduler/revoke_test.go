// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for leasing nothing to a worker whose broker credential was revoked
// through the server ([Scheduler.NoteWorkerRevoked]).

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// submitReadyTask submits a job of one ready one-core task to queue q1 of farm
// f1 (the lease fixture's) and returns the task's ID.
func submitReadyTask(t *testing.T, st store.Store) string {
	t.Helper()
	one := 1
	sub := store.JobSubmission{Job: store.Job{
		ID: uuid.NewString(), FarmID: "f1", QueueID: "q1", Name: "later",
		Status: store.JobStatusRunning, TemplateFormat: store.TemplateFormatJSON,
		RawTemplate: minimalRenderJSON,
	}}
	sub.Steps = []store.Step{{ID: uuid.NewString(), JobID: sub.Job.ID, Name: "render", Status: store.StepStatusReady}}
	sub.Tasks = []store.Task{{
		ID: uuid.NewString(), JobID: sub.Job.ID, StepID: sub.Steps[0].ID, Name: "t",
		Status: store.TaskStatusReady, Parameters: map[string]string{}, RequiredCores: &one,
	}}
	storetest.Submit(t, st, sub)
	return sub.Tasks[0].ID
}

// parkLeaseRequest starts a lease request for workerID on q1 and waits until it
// has parked waiting for work; the returned channel yields its reply.
func parkLeaseRequest(t *testing.T, s *Scheduler, workerID string) <-chan []byte {
	t.Helper()
	req, err := json.Marshal(leaseRequest{WorkerID: workerID})
	if err != nil {
		t.Fatalf("marshal lease request: %v", err)
	}
	done := make(chan []byte, 1)
	go func() { done <- s.handleLeaseRequest(workerID, "q1", req) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.waiters.mu.Lock()
		parked := len(s.waiters.waiters["q1"])
		s.waiters.mu.Unlock()
		if parked > 0 {
			return done
		}
		if time.Now().After(deadline) {
			t.Fatal("the lease request never parked")
		}
		time.Sleep(time.Millisecond)
	}
}

// A lease request the worker parked before its credential was revoked is not
// served when work arrives afterwards: the revoke disconnected the worker, so
// the reply would reach no one and the task would wait for the heartbeat sweep.
func TestParkedLeaseOfARevokedWorkerGetsNoWork(t *testing.T) {
	st := newCheckedFake(t)
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	s.ctx = t.Context()
	s.cfg.NATSAuthEnabled = true
	s.leaseHoldTimeout = 30 * time.Second
	one := 1
	w, _ := seedLeaseFixture(t, st, []*int{&one})
	if got, _ := leaseAs(t, s, w.ID, ""); len(got) != 1 {
		t.Fatalf("first lease = %v, want the one task", got)
	}
	done := parkLeaseRequest(t, s, w.ID)

	s.NoteWorkerRevoked(w.ID)
	later := submitReadyTask(t, st)
	s.waiters.notifyAll()

	select {
	case raw := <-done:
		var rep leaseReply
		if err := json.Unmarshal(raw, &rep); err != nil {
			t.Fatalf("unmarshal lease reply: %v", err)
		}
		if len(rep.Assignments) != 0 {
			t.Fatalf("woken lease of a revoked worker = %s, want no assignments", raw)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the parked lease request never returned")
	}
	if task := mustTaskOf(t, st, later); task.Status != store.TaskStatusReady {
		t.Fatalf("task = %q, want ready (not leased to the revoked worker)", task.Status)
	}
}

// A revoked worker still connected (an external broker with no system
// credential keeps its connection until its user JWT expires) is leased no new
// work, and its request is held rather than answered at once.
func TestRevokedWorkerGetsNoNewWork(t *testing.T) {
	st := newCheckedFake(t)
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	s.ctx = t.Context()
	s.cfg.NATSAuthEnabled = true
	s.leaseHoldTimeout = 200 * time.Millisecond
	s.leaseRefusalDelay = time.Millisecond // a refusal on this delay would show as a fast answer
	one := 1
	w, ids := seedLeaseFixture(t, st, []*int{&one})

	s.NoteWorkerRevoked(w.ID)
	got, took := leaseAs(t, s, w.ID, "")
	if len(got) != 0 {
		t.Fatalf("assignments = %v, want none for a revoked worker", got)
	}
	if took < s.leaseHoldTimeout {
		t.Fatalf("refusal answered in %v, want it held for the lease hold %v", took, s.leaseHoldTimeout)
	}
	if task := mustTaskOf(t, st, ids[0]); task.Status != store.TaskStatusReady {
		t.Fatalf("task = %q, want ready (not leased)", task.Status)
	}
}

// registerF1 drives a registration of worker w1 in farm f1 (the lease
// fixture's) through the handler and requires it acknowledged.
func registerF1(t *testing.T, s *Scheduler) {
	t.Helper()
	msg := &fakeJSMsg{
		subject: bus.WorkerRegisterSubject("w1"),
		data: workerMsgJSON(t, protocol.RegisterMsg{
			Version: protocol.ProtocolVersion, Type: protocol.TypeRegister,
			WorkerID: "w1", FarmID: "f1", Hostname: "h1", OS: "linux", Arch: "amd64", CPUCount: 4,
		}),
	}
	s.handleWorkerMessage(msg)
	if !msg.acked {
		t.Fatal("registration of w1 was not acknowledged")
	}
}

// A registration made with an active credential (the worker enrolled again
// after the revoke, and only an active credential can connect) ends the
// revocation, so the worker is leased work again.
func TestRegistrationWithAnActiveCredentialEndsARevocation(t *testing.T) {
	st := newCheckedFake(t)
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	s.ctx = t.Context()
	s.cfg.NATSAuthEnabled = true
	one := 1
	w, ids := seedLeaseFixture(t, st, []*int{&one})
	s.NoteWorkerRevoked(w.ID)
	if _, err := st.CreateWorkerCredential(t.Context(), store.WorkerCredential{
		ID: uuid.NewString(), WorkerID: w.ID, PublicKey: "UKEY" + uuid.NewString(), EnrolledAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateWorkerCredential: %v", err)
	}

	registerF1(t, s)
	if got, _ := leaseAs(t, s, w.ID, ""); len(got) != 1 || got[0] != ids[0] {
		t.Fatalf("lease after re-enrolment = %v, want the ready task %s", got, ids[0])
	}
}

// A registration with no active credential, such as one stored before the
// revoke and delivered after it, does not end the revocation.
func TestRegistrationWithoutAnActiveCredentialKeepsARevocation(t *testing.T) {
	st := newCheckedFake(t)
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	s.ctx = t.Context()
	s.cfg.NATSAuthEnabled = true
	s.leaseHoldTimeout = 100 * time.Millisecond
	one := 1
	w, ids := seedLeaseFixture(t, st, []*int{&one})
	s.NoteWorkerRevoked(w.ID)

	registerF1(t, s)
	if got, _ := leaseAs(t, s, w.ID, ""); len(got) != 0 {
		t.Fatalf("lease after a registration with no active credential = %v, want none", got)
	}
	if task := mustTaskOf(t, st, ids[0]); task.Status != store.TaskStatusReady {
		t.Fatalf("task = %q, want ready (not leased)", task.Status)
	}
}

// With broker authentication off there is no connection to revoke and no
// credential a registration could confirm, so a revoke of a leftover
// credential row records nothing and the worker is leased work as before.
func TestNoteWorkerRevoked_NoEffectWithBrokerAuthOff(t *testing.T) {
	st := newCheckedFake(t)
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	s.ctx = t.Context()
	one := 1
	w, ids := seedLeaseFixture(t, st, []*int{&one})

	s.NoteWorkerRevoked(w.ID)
	if got, _ := leaseAs(t, s, w.ID, ""); len(got) != 1 || got[0] != ids[0] {
		t.Fatalf("lease with broker auth off = %v, want the ready task %s", got, ids[0])
	}
}

// A re-enrolled worker's lease request that arrives before its registration is
// held as a revoked worker's; the registration that ends the revocation wakes
// it, so the worker gets work at once rather than after the lease hold.
func TestRegistrationWakesARevokedWorkersHeldRequest(t *testing.T) {
	st := newCheckedFake(t)
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	s.ctx = t.Context()
	s.cfg.NATSAuthEnabled = true
	s.leaseHoldTimeout = 30 * time.Second
	one := 1
	w, ids := seedLeaseFixture(t, st, []*int{&one})
	s.NoteWorkerRevoked(w.ID)

	req, err := json.Marshal(leaseRequest{WorkerID: w.ID})
	if err != nil {
		t.Fatalf("marshal lease request: %v", err)
	}
	done := make(chan []byte, 1)
	go func() { done <- s.handleLeaseRequest(w.ID, "q1", req) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.disabledWaiters.mu.Lock()
		held := len(s.disabledWaiters.waiters[w.ID])
		s.disabledWaiters.mu.Unlock()
		if held > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the revoked worker's lease request was never held")
		}
		time.Sleep(time.Millisecond)
	}

	if _, err := st.CreateWorkerCredential(t.Context(), store.WorkerCredential{
		ID: uuid.NewString(), WorkerID: w.ID, PublicKey: "UKEY" + uuid.NewString(), EnrolledAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateWorkerCredential: %v", err)
	}
	registerF1(t, s)

	select {
	case raw := <-done:
		var rep leaseReply
		if err := json.Unmarshal(raw, &rep); err != nil {
			t.Fatalf("unmarshal lease reply: %v", err)
		}
		if len(rep.Assignments) != 1 {
			t.Fatalf("held lease after re-enrolment = %s, want the ready task %s", raw, ids[0])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the held lease request was not woken by the registration")
	}
}
