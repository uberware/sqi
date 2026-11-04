// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/postgres"
	"github.com/uberware/sqi/internal/store/sqlite"
	"github.com/uberware/sqi/internal/store/sqlstore"
	"github.com/uberware/sqi/internal/store/storetest"
)

// newSQLStores is newStores without the fake, for the write fence, which the
// fake does not implement yet.
func newSQLStores(t *testing.T) map[string]store.Store {
	t.Helper()
	sq, err := sqlite.Open(t.Context(), t.TempDir()+"/test.db", sqlite.DefaultOptions())
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = sq.Close() })
	out := map[string]store.Store{"sqlite": sq}
	if storetest.PostgresEnabled() {
		out["postgres"] = storetest.Postgres(t)
	}
	return out
}

// takeOver makes "a" the leader at epoch 1, lets its lease lapse and makes
// "b" the leader at epoch 2. It returns a leadership context for each: a's is
// stale, b's current.
func takeOver(t *testing.T, st store.Store) (stale, current context.Context) {
	t.Helper()
	ctx := t.Context()
	if epoch, ok, err := st.AcquireLeadership(ctx, "a", 50*time.Millisecond); err != nil || !ok || epoch != 1 {
		t.Fatalf("a: epoch=%d ok=%v err=%v, want epoch 1", epoch, ok, err)
	}
	time.Sleep(lapse)
	if epoch, ok, err := st.AcquireLeadership(ctx, "b", time.Hour); err != nil || !ok || epoch != 2 {
		t.Fatalf("b: epoch=%d ok=%v err=%v, want epoch 2", epoch, ok, err)
	}
	return store.WithLeaderEpoch(ctx, 1), store.WithLeaderEpoch(ctx, 2)
}

// fenceCase is one write the fence must stop: seed sets up the rows it
// writes, op runs it, and observe reports the state it changes.
type fenceCase struct {
	name    string
	seed    func(t *testing.T, st store.Store) fenceFixture
	op      func(ctx context.Context, st store.Store, f fenceFixture) error
	observe func(t *testing.T, st store.Store, f fenceFixture) string
}

// fenceFixture names the rows a fenceCase's seed made.
type fenceFixture struct {
	jobID, taskID, workerID, tokenHash string
}

// seedFenceJob submits a pending job with one ready task, in a farm and queue
// of its own.
func seedFenceJob(t *testing.T, st store.Store) fenceFixture {
	t.Helper()
	farm, queue := seedFarmQueue(t, st)
	storetest.NewJob("fence-job", farm.ID, queue.ID).
		Step("fence-step", store.StepStatusReady).
		Task("fence-task", "fence-step", store.TaskStatusReady).
		Submit(t, st)
	return fenceFixture{jobID: "fence-job", taskID: "fence-task"}
}

func observeJobStatus(t *testing.T, st store.Store, f fenceFixture) string {
	t.Helper()
	j, err := st.GetJob(t.Context(), f.jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	return fmt.Sprintf("status=%s reason=%q", j.Status, j.ParkReason)
}

// singleStatementWrites are the writes that ran as one statement outside any
// transaction before the fence: under a leadership context each now runs in
// one, checked at commit.
var singleStatementWrites = []fenceCase{
	{
		name: "PromoteJobRunning",
		seed: seedFenceJob,
		op: func(ctx context.Context, st store.Store, f fenceFixture) error {
			_, err := st.PromoteJobRunning(ctx, f.jobID, time.Now())
			return err
		},
		observe: observeJobStatus,
	},
	{
		name: "PauseJob",
		seed: seedFenceJob,
		op: func(ctx context.Context, st store.Store, f fenceFixture) error {
			return st.PauseJob(ctx, f.jobID, time.Now())
		},
		observe: observeJobStatus,
	},
	{
		name: "CancelJobStatus",
		seed: seedFenceJob,
		op: func(ctx context.Context, st store.Store, f fenceFixture) error {
			return st.CancelJobStatus(ctx, f.jobID)
		},
		observe: observeJobStatus,
	},
	{
		name: "ParkJob",
		seed: seedFenceJob,
		op: func(ctx context.Context, st store.Store, f fenceFixture) error {
			return st.ParkJob(ctx, f.jobID, "too many failures", time.Now())
		},
		observe: observeJobStatus,
	},
	{
		name: "ResumeJob",
		seed: func(t *testing.T, st store.Store) fenceFixture {
			t.Helper()
			f := seedFenceJob(t, st)
			if err := st.PauseJob(t.Context(), f.jobID, time.Now()); err != nil {
				t.Fatalf("PauseJob: %v", err)
			}
			return f
		},
		op: func(ctx context.Context, st store.Store, f fenceFixture) error {
			return st.ResumeJob(ctx, f.jobID, time.Now())
		},
		observe: observeJobStatus,
	},
	{
		name: "SetTaskUnschedulableReason",
		seed: seedFenceJob,
		op: func(ctx context.Context, st store.Store, f fenceFixture) error {
			_, err := st.SetTaskUnschedulableReason(ctx, f.taskID, "no worker has houdini")
			return err
		},
		observe: func(t *testing.T, st store.Store, f fenceFixture) string {
			t.Helper()
			task, err := st.GetTask(t.Context(), f.taskID)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			return task.UnschedulableReason
		},
	},
	{
		name: "SetBrokerIdentity",
		seed: func(t *testing.T, st store.Store) fenceFixture {
			t.Helper()
			if err := st.SetBrokerIdentity(t.Context(), "embedded:SQI", time.Now()); err != nil {
				t.Fatalf("SetBrokerIdentity: %v", err)
			}
			return fenceFixture{}
		},
		op: func(ctx context.Context, st store.Store, _ fenceFixture) error {
			return st.SetBrokerIdentity(ctx, "external:SQI@c1", time.Now())
		},
		observe: func(t *testing.T, st store.Store, _ fenceFixture) string {
			t.Helper()
			id, err := st.GetBrokerIdentity(t.Context())
			if err != nil {
				t.Fatalf("GetBrokerIdentity: %v", err)
			}
			return id
		},
	},
	{
		name: "DeleteExpiredSessions",
		seed: func(t *testing.T, st store.Store) fenceFixture {
			t.Helper()
			ctx := t.Context()
			u, err := st.CreateUser(ctx, mkUser("viewer"))
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			const hash = "fence-session-hash"
			if _, err := st.CreateSession(ctx, store.Session{
				ID: "fence-session", TokenHash: hash, UserID: u.ID,
				ExpiresAt: time.Now().Add(-time.Hour), CreatedAt: time.Now().Add(-2 * time.Hour),
			}); err != nil {
				t.Fatalf("CreateSession: %v", err)
			}
			return fenceFixture{tokenHash: hash}
		},
		op: func(ctx context.Context, st store.Store, _ fenceFixture) error {
			_, err := st.DeleteExpiredSessions(ctx, time.Now())
			return err
		},
		observe: func(t *testing.T, st store.Store, f fenceFixture) string {
			t.Helper()
			// Looked up as of before it expired, so only a delete hides it.
			_, err := st.GetSessionByTokenHash(t.Context(), f.tokenHash, time.Now().Add(-90*time.Minute))
			switch {
			case err == nil:
				return "present"
			case errors.Is(err, store.ErrNotFound):
				return "deleted"
			default:
				t.Fatalf("GetSessionByTokenHash: %v", err)
				return ""
			}
		},
	},
	{
		name: "UpdateWorkerHeartbeat",
		seed: func(t *testing.T, st store.Store) fenceFixture {
			t.Helper()
			farm := mustCreateFarm(t, st, "fence-farm")
			w := seedWorker(t, st, farm.ID, store.WorkerStatusOnline, time.Now().Add(-time.Hour).UTC())
			return fenceFixture{workerID: w.ID}
		},
		op: func(ctx context.Context, st store.Store, f fenceFixture) error {
			return st.UpdateWorkerHeartbeat(ctx, f.workerID, time.Now())
		},
		observe: func(t *testing.T, st store.Store, f fenceFixture) string {
			t.Helper()
			w, err := st.GetWorker(t.Context(), f.workerID)
			if err != nil {
				t.Fatalf("GetWorker: %v", err)
			}
			return fmt.Sprintf("heartbeat=%v status=%s", w.LastHeartbeatAt, w.Status)
		},
	},
}

// TestParity_Fence_SingleStatementWrites: each write a scheduler makes as one
// statement is refused under a deposed leader's epoch, with ErrFenced and the
// row as it was, and lands under the current leader's.
func TestParity_Fence_SingleStatementWrites(t *testing.T) {
	for _, tc := range singleStatementWrites {
		t.Run(tc.name, func(t *testing.T) {
			for name, st := range newSQLStores(t) {
				t.Run(name, func(t *testing.T) {
					f := tc.seed(t, st)
					stale, current := takeOver(t, st)
					before := tc.observe(t, st, f)

					if err := tc.op(stale, st, f); !errors.Is(err, store.ErrFenced) {
						t.Fatalf("under a stale epoch: err = %v, want ErrFenced", err)
					}
					if after := tc.observe(t, st, f); after != before {
						t.Fatalf("under a stale epoch the row changed: %q -> %q", before, after)
					}

					if err := tc.op(current, st, f); err != nil {
						t.Fatalf("under the current epoch: %v", err)
					}
					if after := tc.observe(t, st, f); after == before {
						t.Fatalf("under the current epoch nothing was written (still %q)", before)
					}
				})
			}
		})
	}
}

// TestParity_Fence_LeadershipMethodsAreNeverFenced: the lease and membership
// methods are how a server comes to lead, so a leadership context, even a
// stale one, never refuses them.
func TestParity_Fence_LeadershipMethodsAreNeverFenced(t *testing.T) {
	for name, st := range newSQLStores(t) {
		t.Run(name, func(t *testing.T) {
			stale, _ := takeOver(t, st)
			// b, the current leader, renews and releases under a's stale context.
			if ok, err := st.RenewLeadership(stale, "b", 2, time.Hour); err != nil || !ok {
				t.Fatalf("RenewLeadership under a stale epoch = (%v, %v), want (true, nil)", ok, err)
			}
			if err := st.ReleaseLeadership(stale, "b", 2); err != nil {
				t.Fatalf("ReleaseLeadership under a stale epoch: %v", err)
			}
			epoch, ok, err := st.AcquireLeadership(stale, "c", time.Hour)
			if err != nil || !ok || epoch != 3 {
				t.Fatalf("AcquireLeadership under a stale epoch = (%d, %v, %v), want (3, true, nil)", epoch, ok, err)
			}
			if l, err := st.GetLeadership(stale); err != nil || l.Holder != "c" || l.Epoch != 3 {
				t.Fatalf("GetLeadership under a stale epoch = %+v, %v, want c at epoch 3", l, err)
			}
			m := store.ServerMember{
				InstanceID: "i-1", Name: "n", Host: "h", Version: "v", GrantsFingerprint: "f", StartedAt: time.Now(),
			}
			if err := st.UpsertServerMember(stale, m); err != nil {
				t.Fatalf("UpsertServerMember under a stale epoch: %v", err)
			}
			if got, _, err := st.ListServerMembers(stale); err != nil || len(got) != 1 {
				t.Fatalf("ListServerMembers under a stale epoch = %v, %v, want one member", got, err)
			}
			if n, err := st.DeleteServerMembersUnseenFor(stale, 0); err != nil || n != 1 {
				t.Fatalf("DeleteServerMembersUnseenFor under a stale epoch = (%d, %v), want (1, nil)", n, err)
			}
		})
	}
}

// TestParity_Fence_PlainContextIsNeverFenced: a write with no leadership
// context (a REST handler's) is not checked against the epoch at all, so it
// lands whoever leads.
func TestParity_Fence_PlainContextIsNeverFenced(t *testing.T) {
	for name, st := range newSQLStores(t) {
		t.Run(name, func(t *testing.T) {
			f := seedFenceJob(t, st)
			takeOver(t, st)
			if err := st.PauseJob(t.Context(), f.jobID, time.Now()); err != nil {
				t.Fatalf("PauseJob with no leadership context: %v", err)
			}
			if got := observeJobStatus(t, st, f); !strings.HasPrefix(got, "status=paused") {
				t.Fatalf("job = %s, want paused", got)
			}
		})
	}
}

// TestParity_Fence_TransactionalWriteRollsBack: a write that is already a
// transaction is fenced too, and leaves nothing behind.
func TestParity_Fence_TransactionalWriteRollsBack(t *testing.T) {
	for name, st := range newSQLStores(t) {
		t.Run(name, func(t *testing.T) {
			f := seedFenceJob(t, st)
			stale, current := takeOver(t, st)
			if err := st.DeleteJob(stale, f.jobID); !errors.Is(err, store.ErrFenced) {
				t.Fatalf("DeleteJob under a stale epoch: err = %v, want ErrFenced", err)
			}
			if _, err := st.GetTask(t.Context(), f.taskID); err != nil {
				t.Fatalf("task after a fenced delete: %v, want it kept", err)
			}
			if err := st.DeleteJob(current, f.jobID); err != nil {
				t.Fatalf("DeleteJob under the current epoch: %v", err)
			}
			if _, err := st.GetJob(t.Context(), f.jobID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("job after the current leader's delete: err = %v, want ErrNotFound", err)
			}
		})
	}
}

// stallDialect parks the first statement whose text contains stmt until
// release is closed, after closing hit.
type stallDialect struct {
	sqlstore.Dialect

	stmt    string
	once    *sync.Once
	hit     chan struct{}
	release chan struct{}
}

func (d stallDialect) Rebind(q string) string {
	if strings.Contains(q, d.stmt) {
		d.once.Do(func() {
			close(d.hit)
			<-d.release
		})
	}
	return d.Dialect.Rebind(q)
}

// TestFence_LongTransactionStartedBeforeTakeoverRollsBack: a deposed leader's
// long transaction (the retention delete of a 50,000-task job) that was well
// under way when another server took over does not delay the takeover, and
// is rolled back at its check: the job and every task survive. PostgreSQL
// only; SQLite's single writer cannot run the takeover beside it.
func TestFence_LongTransactionStartedBeforeTakeoverRollsBack(t *testing.T) {
	if !storetest.PostgresEnabled() {
		t.Skip("needs PostgreSQL (SQI_TEST_POSTGRES_DSN)")
	}
	const nTasks = 50_000
	stall := stallDialect{
		stmt: "DELETE FROM steps", once: new(sync.Once),
		hit: make(chan struct{}), release: make(chan struct{}),
	}
	opts := postgres.DefaultOptions()
	opts.WrapDialect = func(d sqlstore.Dialect) sqlstore.Dialect { stall.Dialect = d; return stall }
	st := storetest.PostgresWith(t, opts)
	ctx := t.Context()

	farm, queue := seedFarmQueue(t, st)
	seed := storetest.NewJob("big-job", farm.ID, queue.ID).Step("big-step", store.StepStatusReady)
	for i := range nTasks {
		seed.Task(fmt.Sprintf("big-task-%05d", i), "big-step", store.TaskStatusReady)
	}
	seed.Submit(t, st)

	epoch, ok, err := st.AcquireLeadership(ctx, "a", 50*time.Millisecond)
	if err != nil || !ok || epoch != 1 {
		t.Fatalf("a: epoch=%d ok=%v err=%v", epoch, ok, err)
	}
	done := make(chan error, 1)
	go func() { done <- st.DeleteJob(store.WithLeaderEpoch(ctx, epoch), "big-job") }()
	var released sync.Once
	release := func() { released.Do(func() { close(stall.release) }) }
	t.Cleanup(release)

	select {
	case <-stall.hit: // the tasks are deleted, uncommitted; the steps are next
	case err := <-done:
		t.Fatalf("DeleteJob finished before reaching its steps: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("DeleteJob never reached its steps")
	}

	time.Sleep(lapse)
	start := time.Now()
	if epoch, ok, err := st.AcquireLeadership(ctx, "b", time.Hour); err != nil || !ok || epoch != 2 {
		t.Fatalf("b: epoch=%d ok=%v err=%v, want epoch 2", epoch, ok, err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the takeover took %v beside the deposed leader's open transaction, want no wait", took)
	}

	release()
	select {
	case err := <-done:
		if !errors.Is(err, store.ErrFenced) {
			t.Fatalf("DeleteJob = %v, want ErrFenced", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("DeleteJob did not finish after the takeover")
	}
	if _, err := st.GetJob(ctx, "big-job"); err != nil {
		t.Fatalf("job after the fenced delete: %v, want it kept", err)
	}
	counts, err := st.CountTasksByJob(ctx, "big-job")
	if err != nil {
		t.Fatalf("CountTasksByJob: %v", err)
	}
	total := 0
	for _, n := range counts {
		total += n
	}
	if total != nTasks {
		t.Fatalf("tasks after the fenced delete = %d, want %d", total, nTasks)
	}
}
