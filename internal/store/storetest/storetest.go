// SPDX-License-Identifier: AGPL-3.0-or-later

// Package storetest builds store state for tests through the production write
// path: CreateJobSubmission for rows, LeaseTask and StartTaskAttempt for work
// in flight, CompleteTaskAttempt and RecordTaskFailure for work that ended. It is written only against store.Store, so the same helpers run
// on every backend.
//
// States production cannot reach — an open attempt on a terminal task, a claim
// on a closed attempt — come from the backend's injectors, reached through
// [InjectAttempt], [InjectClaim] and [InjectorFor]. Never use an injector for a state a production write can
// produce: a fixture that bypasses the real path drifts from it unnoticed.
package storetest

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
)

// Submit creates sub through [store.JobStore.CreateJobSubmission], failing the
// test on error. Every status and field the submission carries is written as
// given; only created_at and updated_at are stamped by the store.
func Submit(t testing.TB, st store.Store, sub store.JobSubmission) store.JobSubmission {
	t.Helper()
	out, err := st.CreateJobSubmission(t.Context(), sub)
	if err != nil {
		t.Fatalf("CreateJobSubmission %s: %v", sub.Job.ID, err)
	}
	return out
}

// Lease leases req.TaskID through [store.TaskStore.LeaseTask] and returns the
// attempt it created, failing the test unless the lease landed. A zero
// AttemptID is filled with a fresh UUID and a zero Now with the current time.
func Lease(t testing.TB, st store.Store, req store.LeaseRequest) store.TaskAttempt {
	t.Helper()
	if req.AttemptID == "" {
		req.AttemptID = uuid.NewString()
	}
	if req.Now.IsZero() {
		req.Now = time.Now().UTC()
	}
	res, err := st.LeaseTask(t.Context(), req)
	if err != nil {
		t.Fatalf("LeaseTask %s: %v", req.TaskID, err)
	}
	if res.Outcome != store.LeaseLeased {
		t.Fatalf("LeaseTask %s: outcome %q (pool %q), want %q", req.TaskID, res.Outcome, res.FullPool, store.LeaseLeased)
	}
	return res.Attempt
}

// SubmitLeasing submits sub with each task that asks to be assigned or running
// submitted ready, then leases it in submission order through [Lease] (and, for
// running, starts it through [Start]). That is the only way production puts a
// task there: one submitted assigned or running has no attempt, a state no
// production write produces. lease returns the request each such task is
// leased with, given the task as submitted; SubmitLeasing sets its TaskID. The
// job's farm and queue must already exist, because the lease checks them, and
// a terminal job cannot hold leased work, so asking for one fails the test.
//
// It returns the submission with each leased task as stored after its lease,
// and the attempt each leased task holds, by task ID.
func SubmitLeasing(
	t testing.TB, st store.Store, sub store.JobSubmission, lease func(store.Task) store.LeaseRequest,
) (out store.JobSubmission, attempts map[string]store.TaskAttempt) {
	t.Helper()
	tasks := slices.Clone(sub.Tasks)
	var inflight []int
	for i, task := range tasks {
		if task.Status == store.TaskStatusAssigned || task.Status == store.TaskStatusRunning {
			inflight = append(inflight, i)
			tasks[i].Status = store.TaskStatusReady
		}
	}
	if len(inflight) > 0 && sub.Job.Status.IsTerminal() {
		t.Fatalf("SubmitLeasing: a %s job cannot hold assigned or running tasks through production writes", sub.Job.Status)
	}
	asked := sub.Tasks
	sub.Tasks = tasks
	out = Submit(t, st, sub)

	attempts = make(map[string]store.TaskAttempt, len(inflight))
	for _, i := range inflight {
		task := asked[i]
		req := lease(task)
		req.TaskID = task.ID
		if task.Status == store.TaskStatusRunning {
			attempts[task.ID] = Running(t, st, req)
		} else {
			attempts[task.ID] = Lease(t, st, req)
		}
		got, err := st.GetTask(t.Context(), task.ID)
		if err != nil {
			t.Fatalf("GetTask %s: %v", task.ID, err)
		}
		out.Tasks[i] = got
	}
	return out, attempts
}

// LeaseTo is a [SubmitLeasing] lease function that leases every task to
// workerID.
func LeaseTo(workerID string) func(store.Task) store.LeaseRequest {
	return func(store.Task) store.LeaseRequest { return store.LeaseRequest{WorkerID: workerID} }
}

// Start moves attempt's task to running through
// [store.TaskStore.StartTaskAttempt] at the attempt's start time, failing the
// test unless it started.
func Start(t testing.TB, st store.Store, attempt store.TaskAttempt) {
	t.Helper()
	started, err := st.StartTaskAttempt(t.Context(), attempt.ID, attempt.TaskID, "", attempt.StartedAt)
	if err != nil {
		t.Fatalf("StartTaskAttempt %s: %v", attempt.ID, err)
	}
	if !started {
		t.Fatalf("StartTaskAttempt %s: not started", attempt.ID)
	}
}

// Running leases req.TaskID and starts the attempt, leaving the task running.
func Running(t testing.TB, st store.Store, req store.LeaseRequest) store.TaskAttempt {
	t.Helper()
	a := Lease(t, st, req)
	Start(t, st, a)
	return a
}

// Complete ends attempt the way a worker's report does, through
// [store.TaskStore.CompleteTaskAttempt]: its task reaches status (succeeded,
// failed or canceled) and the attempt the matching status, now. It fails the
// test unless the completion applied.
func Complete(t testing.TB, st store.Store, attempt store.TaskAttempt, status store.TaskStatus) {
	t.Helper()
	res, err := st.CompleteTaskAttempt(t.Context(), store.AttemptCompletion{
		AttemptID: attempt.ID, TaskID: attempt.TaskID, TaskStatus: status,
		AttemptStatus: store.AttemptStatus(status), EndedAt: time.Now().UTC(),
	})
	if err != nil || !res.Applied {
		t.Fatalf("CompleteTaskAttempt(%s -> %s) = (%+v, %v), want applied", attempt.TaskID, status, res, err)
	}
}

// FailAndRequeue leases req.TaskID, which must be ready, records the attempt as
// failed and requeues the task to run again at retryAfter: one failed attempt
// behind a ready task, the way a worker-reported failure leaves it. It returns
// the failed attempt.
func FailAndRequeue(t testing.TB, st store.Store, req store.LeaseRequest, retryAfter time.Time) store.TaskAttempt {
	t.Helper()
	a := Lease(t, st, req)
	now := time.Now().UTC()
	if _, _, first, err := st.RecordTaskFailure(t.Context(), a.ID, a.TaskID, nil, "", "boom", now); err != nil || !first {
		t.Fatalf("RecordTaskFailure %s = (first %v, %v), want the first close", a.ID, first, err)
	}
	if ok, err := st.RequeueTaskForRetry(t.Context(), a.TaskID, a.ID, retryAfter, now); err != nil || !ok {
		t.Fatalf("RequeueTaskForRetry %s = (%v, %v), want requeued", a.TaskID, ok, err)
	}
	return a
}

// Injector writes rows as given, with no state checks. Both concrete stores
// implement it; store.Store does not. It exists only to build states
// production cannot reach.
//
// On both backends a zero attempt CreatedAt or claim ClaimedAt is stamped now,
// and a claim is always written active: its ReleasedAt is ignored.
type Injector interface {
	InjectTaskAttempt(ctx context.Context, attempt store.TaskAttempt) (store.TaskAttempt, error)
	InjectClaim(ctx context.Context, claim store.UsageClaim) (store.UsageClaim, error)
}

// InjectorFor returns st's injectors, failing the test when st has none. A
// wrapper that embeds store.Store has none: pass the store it wraps.
func InjectorFor(t testing.TB, st store.Store) Injector {
	t.Helper()
	inj, ok := st.(Injector)
	if !ok {
		t.Fatalf("store %T has no injectors; pass the concrete store, not a wrapper", st)
	}
	return inj
}

// InjectAttempt injects attempt through st's injector, failing the test on
// error. A zero ID is filled with a fresh UUID and a zero StartedAt with now.
func InjectAttempt(t testing.TB, st store.Store, attempt store.TaskAttempt) store.TaskAttempt {
	t.Helper()
	if attempt.ID == "" {
		attempt.ID = uuid.NewString()
	}
	if attempt.StartedAt.IsZero() {
		attempt.StartedAt = time.Now().UTC()
	}
	out, err := InjectorFor(t, st).InjectTaskAttempt(t.Context(), attempt)
	if err != nil {
		t.Fatalf("InjectTaskAttempt %s: %v", attempt.ID, err)
	}
	return out
}

// InjectClaim injects an active claim through st's injector, failing the test
// on error. A zero ID is filled with a fresh UUID.
func InjectClaim(t testing.TB, st store.Store, claim store.UsageClaim) store.UsageClaim {
	t.Helper()
	if claim.ID == "" {
		claim.ID = uuid.NewString()
	}
	out, err := InjectorFor(t, st).InjectClaim(t.Context(), claim)
	if err != nil {
		t.Fatalf("InjectClaim %s: %v", claim.ID, err)
	}
	return out
}
