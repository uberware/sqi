// SPDX-License-Identifier: AGPL-3.0-or-later

package server

// Tests for the WebSocket-hub wiring the Task-8 review found had zero
// coverage: wsJobOwnerResolver itself, and newWSHub — the small wrapper
// around ws.NewHub that (*Server).start calls. Without a test exercising
// newWSHub, a regression that silently reverted its body back to
// ws.NewHub(logger, nil) (the pre-Task-8 placeholder) would pass every other
// test in this package, since nothing else constructs the hub with a real
// store-backed resolver.

import (
	"context"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/ws"
)

// TestWsJobOwnerResolver directly pins wsJobOwnerResolver's behavior: it
// resolves a known job's owner, and fails closed (returns "") when the job
// cannot be resolved.
func TestWsJobOwnerResolver(t *testing.T) {
	st := fake.New()
	if _, err := st.CreateJob(context.Background(), store.Job{
		ID:        "job-1",
		Name:      "job-1",
		Owner:     "alice",
		Status:    store.JobStatusPending,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	resolve := wsJobOwnerResolver(st)

	if got := resolve("job-1"); got != "alice" {
		t.Errorf("resolve(job-1) = %q, want %q", got, "alice")
	}
	if got := resolve("does-not-exist"); got != "" {
		t.Errorf("resolve(does-not-exist) = %q, want %q (must fail closed)", got, "")
	}
}

// TestNewWSHub_ScopedClientReceivesOwnJobEventsOnly exercises the actual
// wiring (*Server).start uses — newWSHub(s.logger, st) — end to end through
// the real Hub API rather than constructing a hub with a hand-rolled
// resolver. A scoped client must receive NotifyTask pushes for its own job
// (resolved via the real store, TaskEvent carries no Owner field so the
// resolver is the only source of truth) and must not receive pushes for
// another owner's job. If newWSHub silently reverted to ws.NewHub(logger,
// nil), the owner cache could never resolve anything and this would fail on
// the first assertion.
func TestNewWSHub_ScopedClientReceivesOwnJobEventsOnly(t *testing.T) {
	st := fake.New()
	seedTestJob(t, st, "job-alice", "alice")
	seedTestJob(t, st, "job-bob", "bob")

	hub := newWSHub(testLogger(), st)

	ch := hub.Register("c1", ws.Scope{Owner: "alice"})
	if err := hub.Subscribe("c1", ws.SubjectJobs, 0); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	hub.NotifyTask(ws.TaskEvent{JobID: "job-alice", TaskID: "t1", Status: "running"})
	hub.NotifyTask(ws.TaskEvent{JobID: "job-bob", TaskID: "t2", Status: "running"})

	select {
	case env := <-ch:
		if env.Subject != ws.SubjectJobs {
			t.Fatalf("subject = %q, want %q", env.Subject, ws.SubjectJobs)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scoped client did not receive NotifyTask push for its own job " +
			"— newWSHub's resolver wiring is broken")
	}

	// job-bob's push must never arrive: nothing else should be queued.
	select {
	case env := <-ch:
		t.Fatalf("scoped client unexpectedly received a second push (subject=%q) — cross-owner leak", env.Subject)
	case <-time.After(200 * time.Millisecond):
	}
}

// seedTestJob inserts a minimal job owned by owner.
func seedTestJob(t *testing.T, st store.Store, id, owner string) {
	t.Helper()
	if _, err := st.CreateJob(context.Background(), store.Job{
		ID:        id,
		Name:      id,
		Owner:     owner,
		Status:    store.JobStatusPending,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateJob(%s): %v", id, err)
	}
}
