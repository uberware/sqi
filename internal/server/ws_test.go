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
	"sync/atomic"
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
// wiring (*Server).start uses — newWSHub(s.logger, st, true) — end to end
// through the real Hub API rather than constructing a hub with a hand-rolled
// resolver. A scoped client must receive NotifyTask pushes for its own job
// (resolved via the real store, TaskEvent carries no Owner field so the
// resolver is the only source of truth) and must not receive pushes for
// another owner's job. If newWSHub silently reverted to ws.NewHub(logger,
// nil), the owner cache could never resolve anything and this would fail on
// the first assertion. authEnabled=true here: this test is pinning the
// auth-enabled path specifically — see
// TestNewWSHub_AuthDisabled_ResolverNeverReadsStore for the auth-off path.
func TestNewWSHub_ScopedClientReceivesOwnJobEventsOnly(t *testing.T) {
	st := fake.New()
	seedTestJob(t, st, "job-alice", "alice")
	seedTestJob(t, st, "job-bob", "bob")

	hub := newWSHub(testLogger(), st, true)

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

// countingStore wraps a store.Store and counts calls to GetJob, so tests can
// assert a code path performs zero store reads without depending on timing.
type countingStore struct {
	store.Store

	getJobCalls atomic.Int64
}

func (c *countingStore) GetJob(ctx context.Context, id string) (store.Job, error) {
	c.getJobCalls.Add(1)
	return c.Store.GetJob(ctx, id)
}

// TestNewWSHub_AuthDisabled_ResolverNeverReadsStore pins Fix wave 2 Finding
// 1: with auth disabled, newWSHub must wire a nil owner resolver so that
// resolveOwner's ownerCache.get short-circuits (lookup == nil) before ever
// touching the store, no matter how many job/task events fire and even with
// zero registered clients. Before the fix, every one of these events ran a
// live store.GetJob, because ownerCache.get deliberately never memoizes a ""
// result — exactly what an owner-less (pre-auth) job resolves to — so an
// auth-off job re-queried the store on every single event, forever, on the
// scheduler goroutine.
func TestNewWSHub_AuthDisabled_ResolverNeverReadsStore(t *testing.T) {
	st := &countingStore{Store: fake.New()}
	seedTestJob(t, st, "job-1", "") // owner-less: the auth-off norm

	hub := newWSHub(testLogger(), st, false)

	// Zero registered clients, matching the reviewer's measurement setup.
	for range 10 {
		hub.NotifyTask(ws.TaskEvent{JobID: "job-1", TaskID: "t1", Status: "running"})
		hub.NotifyJob(ws.JobEvent{JobID: "job-1", Status: "running"})
	}

	if got := st.getJobCalls.Load(); got != 0 {
		t.Fatalf("GetJob calls = %d, want 0 (auth disabled must wire a nil resolver, "+
			"no store reads)", got)
	}
}
