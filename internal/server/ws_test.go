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

	got, err := resolve("job-1")
	if err != nil || got != "alice" {
		t.Errorf("resolve(job-1) = (%q, %v), want (%q, nil)", got, err, "alice")
	}
	// A missing job must fail closed AND report the error, so the hub knows
	// not to memoize the empty owner.
	got, err = resolve("does-not-exist")
	if got != "" {
		t.Errorf("resolve(does-not-exist) = %q, want %q (must fail closed)", got, "")
	}
	if err == nil {
		t.Error("resolve(does-not-exist) returned a nil error; an unresolvable job must not be cacheable")
	}
}

// TestNewWSHub_ScopedClientReceivesOwnJobEventsOnly exercises the actual
// wiring (*Server).start uses — newWSHub(s.logger, st, true) — end to end
// through the real Hub API rather than constructing a hub with a hand-rolled
// resolver. A scoped client must receive NotifyTask pushes for its own job
// (resolved via the real store, TaskEvent carries no Owner field so the
// resolver is the only source of truth) and must not receive pushes for
// another owner's job. If newWSHub silently dropped owner scoping, the owner
// cache could never resolve anything and this would fail on the first
// assertion. authEnabled=true here: this test is pinning the
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

// TestNewWSHub_AuthDisabled_ResolverNeverReadsStore pins the auth-off path:
// newWSHub must wire no owner resolver, so ownerCache.get short-circuits
// before ever touching the store, no matter how many job/task events fire and
// even with zero registered clients.
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

// TestNewWSHub_AuthEnabled_OwnerlessJobResolvesOnce pins the auth-on
// counterpart. A job with no owner — every job submitted before auth was
// enabled — used to re-query the store on *every* task event, because the
// owner cache refused to memoize an empty owner (it could not distinguish "no
// owner" from "lookup failed"). The resolver now returns an error for the
// latter only, so the empty owner is cached after the first read.
func TestNewWSHub_AuthEnabled_OwnerlessJobResolvesOnce(t *testing.T) {
	st := &countingStore{Store: fake.New()}
	seedTestJob(t, st, "job-1", "") // owner-less: a pre-auth job

	hub := newWSHub(testLogger(), st, true)

	for range 25 {
		hub.NotifyTask(ws.TaskEvent{JobID: "job-1", TaskID: "t1", Status: "running"})
	}

	if got := st.getJobCalls.Load(); got != 1 {
		t.Fatalf("GetJob calls = %d, want 1 (an owner-less job must be resolved "+
			"once and memoized, not re-read on every task event)", got)
	}
}

// TestNewWSHub_AuthEnabled_FailedLookupIsNotCached is the other half of that
// trade-off: a lookup that fails must NOT be memoized, or one transient store
// error would hide a job from scoped clients for the process's lifetime.
func TestNewWSHub_AuthEnabled_FailedLookupIsNotCached(t *testing.T) {
	st := &countingStore{Store: fake.New()}
	// No job seeded, so every GetJob fails with ErrNotFound.

	hub := newWSHub(testLogger(), st, true)

	for range 5 {
		hub.NotifyTask(ws.TaskEvent{JobID: "missing", TaskID: "t1", Status: "running"})
	}

	if got := st.getJobCalls.Load(); got != 5 {
		t.Fatalf("GetJob calls = %d, want 5 (a failed resolution must stay "+
			"un-memoized so it can recover)", got)
	}
}
