// SPDX-License-Identifier: AGPL-3.0-or-later

// Package storetest builds store state for tests through the production write
// path: CreateJobSubmission for rows, LeaseTask and StartTaskAttempt for work
// in flight. It is written only against store.Store, so the same helpers run
// on every backend.
//
// States production cannot reach — an open attempt on a terminal task, a claim
// on a closed attempt — come from the backend's injectors, reached through
// [InjectorFor]. Never use an injector for a state a production write can
// produce: a fixture that bypasses the real path drifts from it unnoticed.
package storetest

import (
	"context"
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

// Start moves attempt's task to running through
// [store.TaskStore.StartTaskAttempt], failing the test unless it started.
func Start(t testing.TB, st store.Store, attempt store.TaskAttempt, sessionID string, now time.Time) {
	t.Helper()
	started, err := st.StartTaskAttempt(t.Context(), attempt.ID, attempt.TaskID, sessionID, now)
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
	Start(t, st, a, "", a.StartedAt)
	return a
}

// Injector writes rows exactly as given, with no state checks. Both concrete
// stores implement it; store.Store does not. It exists only to build states
// production cannot reach.
type Injector interface {
	InjectTaskAttempt(ctx context.Context, attempt store.TaskAttempt) (store.TaskAttempt, error)
	InjectClaim(ctx context.Context, claim store.UsageClaim) (store.UsageClaim, error)
}

// AsInjector reports whether st is a concrete store with injectors. A wrapper
// that embeds store.Store is not: take the injector from the store it wraps.
func AsInjector(st store.Store) (Injector, bool) {
	inj, ok := st.(Injector)
	return inj, ok
}

// InjectorFor returns st's injectors, failing the test when st has none.
func InjectorFor(t testing.TB, st store.Store) Injector {
	t.Helper()
	inj, ok := AsInjector(st)
	if !ok {
		t.Fatalf("store %T has no injectors; pass the concrete store, not a wrapper", st)
	}
	return inj
}
