// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/postgres"
	"github.com/uberware/sqi/internal/store/sqlstore"
	"github.com/uberware/sqi/internal/store/storetest"
)

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

// singleStatementWrites are the writes that run as one statement outside any
// transaction without a leadership context; under one, each runs in a
// transaction and is checked at commit.
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
			now := time.Now().UTC()
			return st.UpdateWorkerHeartbeat(ctx, f.workerID, now, &now)
		},
		observe: func(t *testing.T, st store.Store, f fenceFixture) string {
			t.Helper()
			w, err := st.GetWorker(t.Context(), f.workerID)
			if err != nil {
				t.Fatalf("GetWorker: %v", err)
			}
			return fmt.Sprintf("heartbeat=%v broker=%v status=%s", w.LastHeartbeatAt, w.HeartbeatBrokerAt, w.Status)
		},
	},
}

// TestParity_Fence_SingleStatementWrites: each write a scheduler makes as one
// statement is refused under a deposed leader's epoch, with ErrFenced and the
// row as it was, and lands under the current leader's.
func TestParity_Fence_SingleStatementWrites(t *testing.T) {
	for _, tc := range singleStatementWrites {
		t.Run(tc.name, func(t *testing.T) {
			for name, st := range newStores(t) {
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
	for name, st := range newStores(t) {
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
			// A member counts as unseen only once the clock has moved past its
			// stamp; without a pause both can read the same microsecond.
			time.Sleep(10 * time.Millisecond)
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
	for name, st := range newStores(t) {
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

// TestParity_Fence_IgnoredWorkerWritesStillFence: the worker writes that can
// end without changing anything (a stale registration, a heartbeat the broker
// stamped before the recorded one, a heartbeat for an unknown worker) are fenced like any
// other, so a deposed leader sees ErrFenced from them on every backend and not
// the result the current leader would get.
func TestParity_Fence_IgnoredWorkerWritesStillFence(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			farm := fenceFarm(t, st)
			earlier, later := subSecond()
			beat := later
			reg := func(c context.Context, stamp time.Time) error {
				_, _, err := st.RegisterWorker(c, store.Worker{
					ID: "w-fence-order", FarmID: farm.ID, Hostname: "node", Status: store.WorkerStatusOnline,
					LastHeartbeatAt: &beat, InstanceID: "B", RegistrationBrokerAt: &stamp,
				})
				return err
			}
			if err := reg(ctx, later); err != nil {
				t.Fatalf("register: %v", err)
			}
			if err := st.UpdateWorkerHeartbeat(ctx, "w-fence-order", later, &later); err != nil {
				t.Fatalf("heartbeat: %v", err)
			}
			stale, current := takeOver(t, st)

			cases := []struct {
				name        string
				op          func(context.Context) error
				wantCurrent error
			}{
				{"stale registration", func(c context.Context) error { return reg(c, earlier) }, store.ErrStaleRegistration},
				{"earlier-stamped heartbeat", func(c context.Context) error {
					return st.UpdateWorkerHeartbeat(c, "w-fence-order", later, &earlier)
				}, nil},
				{"unknown worker heartbeat", func(c context.Context) error {
					return st.UpdateWorkerHeartbeat(c, "w-ghost", later, nil)
				}, store.ErrNotFound},
			}
			for _, tc := range cases {
				if err := tc.op(stale); !errors.Is(err, store.ErrFenced) {
					t.Errorf("%s under a stale epoch: err = %v, want ErrFenced", tc.name, err)
				}
				err := tc.op(current)
				if tc.wantCurrent == nil && err != nil || tc.wantCurrent != nil && !errors.Is(err, tc.wantCurrent) {
					t.Errorf("%s under the current epoch: err = %v, want %v", tc.name, err, tc.wantCurrent)
				}
			}
		})
	}
}

// TestParity_Fence_TransactionalWriteRollsBack: a write that is already a
// transaction is fenced too, and leaves nothing behind.
func TestParity_Fence_TransactionalWriteRollsBack(t *testing.T) {
	for name, st := range newStores(t) {
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

// fenceReadOnly lists every Store method that is not a fenced write: the reads,
// and the seven LeadershipStore methods, which are never fenced because they
// are how a server comes to lead. Close is not a database operation.
var fenceReadOnly = []string{
	"AcquireLeadership", "RenewLeadership", "ReleaseLeadership", "GetLeadership",
	"UpsertServerMember", "ListServerMembers", "DeleteServerMembersUnseenFor",
	"Close",
	"ActiveClaimCount", "CommittedCores", "FailureReasonSummary", "LatestTaskAttempt",
	"CountActiveTasksInFarm", "CountActiveTasksInQueue", "CountIdleWorkers", "CountReadyTasksByQueue",
	"CountTasksByJob", "CountUnschedulableTasksByJob", "CountUsers",
	"GetAPIKeyUserByTokenHash", "GetActiveWorkerCredentialByPublicKey", "GetBrokerIdentity",
	"GetComputeLocation", "GetComputeLocationByName", "GetFarm", "GetJob", "GetProductByName", "GetQueue",
	"GetSessionByTokenHash", "GetSessionUserByTokenHash", "GetStep", "GetStorageLocation",
	"GetStorageLocationByName", "GetTask", "GetTaskAttempt", "GetUsagePool", "GetUser",
	"GetUserByExternalID", "GetUserByUsername", "GetWorker", "GetWorkerJoinTokenByHash",
	"ListAPIKeysForUser", "ListActiveWorkerCredentials", "ListBlockedJobs", "ListComputeLocations",
	"ListDependents", "ListFarms", "ListJobDependencyIDs", "ListJobIDsWithPendingSteps", "ListJobs",
	"LatestAttemptProgress", "ListOpenAttempts", "ListProducts", "ListQueues", "ListReadyTasks", "ListStaleWorkers", "ListSteps",
	"ListStorageLocations", "ListStuckSteps", "ListTaskAttempts", "ListTaskLogs", "ListTasks",
	"ListUsagePoolUtilization", "ListUsagePools", "ListUsers", "ListWorkers",
	"RunningProgressByJob",
}

// fenceMutator builds, with a plain context, the rows one store write needs,
// and returns the write: a call that succeeds when made under the current
// leader's epoch and writes something, so a stale epoch has a write to refuse.
type fenceMutator func(t *testing.T, st store.Store) func(ctx context.Context) error

// fenceMutators has one entry for every Store method that writes and is
// fenced. Fixtures are named by UUID, so every entry runs against one store.
var fenceMutators = map[string]fenceMutator{
	"AppendAuditEntry": func(_ *testing.T, st store.Store) func(context.Context) error {
		e := store.AuditEntry{
			ID: uuid.NewString(), EntityType: "job", EntityID: uuid.NewString(), Action: "canceled",
			Details: map[string]any{}, CreatedAt: time.Now().UTC(),
		}
		return func(ctx context.Context) error { return st.AppendAuditEntry(ctx, e) }
	},
	"CancelBlockedJob": func(t *testing.T, st store.Store) func(context.Context) error {
		g := fenceBlockedJob(t, st)
		return func(ctx context.Context) error {
			_, _, err := st.CancelBlockedJob(ctx, g.Job.ID, "fenced", time.Now().UTC())
			return err
		}
	},
	"CancelJobExecution": func(t *testing.T, st store.Store) func(context.Context) error {
		g := fenceLiveJob(t, st, store.TaskStatusReady)
		return func(ctx context.Context) error {
			_, err := st.CancelJobExecution(ctx, g.Job.ID, "fenced", time.Now().UTC())
			return err
		}
	},
	"CancelJobStatus": func(t *testing.T, st store.Store) func(context.Context) error {
		g := fenceLiveJob(t, st, store.TaskStatusReady)
		return func(ctx context.Context) error { return st.CancelJobStatus(ctx, g.Job.ID) }
	},
	"CancelPendingStep": func(t *testing.T, st store.Store) func(context.Context) error {
		g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning}, failedUpstream, stepSpec{
			name: "a", status: store.StepStatusPending, dependsOn: []string{"up"},
			tasks: []store.TaskStatus{store.TaskStatusPending},
		})
		return func(ctx context.Context) error {
			_, _, err := st.CancelPendingStep(ctx, g.Steps["a"].ID, store.FailureReasonUpstreamFailed, time.Now().UTC())
			return err
		}
	},
	"CancelTaskExecution": func(t *testing.T, st store.Store) func(context.Context) error {
		g := fenceLiveJob(t, st, store.TaskStatusReady)
		return func(ctx context.Context) error {
			_, _, err := st.CancelTaskExecution(ctx, g.Tasks["a"][0].ID, "fenced", time.Now().UTC())
			return err
		}
	},
	"CompleteTaskAttempt": func(t *testing.T, st store.Store) func(context.Context) error {
		g := fenceLiveJob(t, st, store.TaskStatusRunning)
		task := g.Tasks["a"][0]
		c := completion(task, g.Attempts[task.ID], store.TaskStatusSucceeded, store.AttemptStatusSucceeded)
		return func(ctx context.Context) error {
			_, err := st.CompleteTaskAttempt(ctx, c)
			return err
		}
	},
	"CreateAPIKey": func(t *testing.T, st store.Store) func(context.Context) error {
		u := fenceUser(t, st, "viewer")
		k := mkAPIKey(u.ID, "hash-"+uuid.NewString())
		return func(ctx context.Context) error {
			_, err := st.CreateAPIKey(ctx, k)
			return err
		}
	},
	"CreateComputeLocation": func(_ *testing.T, st store.Store) func(context.Context) error {
		return func(ctx context.Context) error {
			_, err := st.CreateComputeLocation(ctx, mkComputeLocation(uuid.NewString()))
			return err
		}
	},
	"CreateFarm": func(_ *testing.T, st store.Store) func(context.Context) error {
		return func(ctx context.Context) error {
			_, err := st.CreateFarm(ctx, store.Farm{ID: uuid.NewString(), Name: uuid.NewString()})
			return err
		}
	},
	"CreateJobSubmission": func(t *testing.T, st store.Store) func(context.Context) error {
		farm, queue := fenceFarmQueue(t, st)
		sub := storetest.NewJob(uuid.NewString(), farm.ID, queue.ID).
			Step(uuid.NewString(), store.StepStatusReady).Sub
		sub.Tasks = []store.Task{{
			ID: uuid.NewString(), JobID: sub.Job.ID, StepID: sub.Steps[0].ID, Name: "t",
			Status: store.TaskStatusReady, Parameters: map[string]string{},
		}}
		return func(ctx context.Context) error {
			_, err := st.CreateJobSubmission(ctx, sub)
			return err
		}
	},
	"CreateProduct": func(_ *testing.T, st store.Store) func(context.Context) error {
		return func(ctx context.Context) error {
			_, err := st.CreateProduct(ctx, mkProduct(uuid.NewString()))
			return err
		}
	},
	"CreateQueue": func(t *testing.T, st store.Store) func(context.Context) error {
		farm := fenceFarm(t, st)
		return func(ctx context.Context) error {
			_, err := st.CreateQueue(ctx, store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: uuid.NewString()})
			return err
		}
	},
	"CreateSession": func(t *testing.T, st store.Store) func(context.Context) error {
		u := fenceUser(t, st, "viewer")
		return func(ctx context.Context) error {
			_, err := st.CreateSession(ctx, store.Session{
				ID: uuid.NewString(), TokenHash: uuid.NewString(), UserID: u.ID,
				ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(),
			})
			return err
		}
	},
	"CreateStorageLocation": func(_ *testing.T, st store.Store) func(context.Context) error {
		return func(ctx context.Context) error {
			_, err := st.CreateStorageLocation(ctx, mkStorageLocation(uuid.NewString()))
			return err
		}
	},
	"CreateTaskLog": func(t *testing.T, st store.Store) func(context.Context) error {
		g := fenceLiveJob(t, st, store.TaskStatusRunning)
		l := mkTaskLog(g.Attempts[g.Tasks["a"][0].ID], 1, "fenced\n")
		return func(ctx context.Context) error {
			_, err := st.CreateTaskLog(ctx, l)
			return err
		}
	},
	"CreateUsagePool": func(_ *testing.T, st store.Store) func(context.Context) error {
		return func(ctx context.Context) error {
			_, err := st.CreateUsagePool(ctx, store.UsagePool{ID: uuid.NewString(), Name: uuid.NewString(), MaxConcurrent: 1})
			return err
		}
	},
	"CreateUser": func(_ *testing.T, st store.Store) func(context.Context) error {
		return func(ctx context.Context) error {
			_, err := st.CreateUser(ctx, mkUser("viewer"))
			return err
		}
	},
	"CreateWorkerCredential": func(_ *testing.T, st store.Store) func(context.Context) error {
		return func(ctx context.Context) error {
			_, err := st.CreateWorkerCredential(ctx, mkCredential(uuid.NewString()))
			return err
		}
	},
	"CreateWorkerJoinToken": func(_ *testing.T, st store.Store) func(context.Context) error {
		return func(ctx context.Context) error {
			_, err := st.CreateWorkerJoinToken(ctx, mkJoinToken())
			return err
		}
	},
	"DeleteComputeLocation": func(t *testing.T, st store.Store) func(context.Context) error {
		loc := mustCreateComputeLocation(t, st, uuid.NewString())
		return func(ctx context.Context) error { return st.DeleteComputeLocation(ctx, loc.ID) }
	},
	"DeleteExpiredSessions": func(t *testing.T, st store.Store) func(context.Context) error {
		u := fenceUser(t, st, "viewer")
		if _, err := st.CreateSession(t.Context(), store.Session{
			ID: uuid.NewString(), TokenHash: uuid.NewString(), UserID: u.ID,
			ExpiresAt: time.Now().Add(-time.Hour), CreatedAt: time.Now().Add(-2 * time.Hour),
		}); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		return func(ctx context.Context) error {
			_, err := st.DeleteExpiredSessions(ctx, time.Now())
			return err
		}
	},
	"DeleteFarm": func(t *testing.T, st store.Store) func(context.Context) error {
		farm := fenceFarm(t, st)
		return func(ctx context.Context) error { return st.DeleteFarm(ctx, farm.ID) }
	},
	"DeleteJob": func(t *testing.T, st store.Store) func(context.Context) error {
		g := fenceLiveJob(t, st, store.TaskStatusReady)
		return func(ctx context.Context) error { return st.DeleteJob(ctx, g.Job.ID) }
	},
	"DeleteOfflineWorkersBefore": func(t *testing.T, st store.Store) func(context.Context) error {
		fenceWorker(t, st, store.WorkerStatusOffline, time.Now().Add(-time.Hour))
		return func(ctx context.Context) error {
			_, err := st.DeleteOfflineWorkersBefore(ctx, time.Now())
			return err
		}
	},
	"DeleteProduct": func(t *testing.T, st store.Store) func(context.Context) error {
		p := mustCreateProduct(t, st, uuid.NewString())
		return func(ctx context.Context) error { return st.DeleteProduct(ctx, p.Name) }
	},
	"DeleteQueue": func(t *testing.T, st store.Store) func(context.Context) error {
		_, q := fenceFarmQueue(t, st)
		return func(ctx context.Context) error { return st.DeleteQueue(ctx, q.ID) }
	},
	"DeleteSession": func(t *testing.T, st store.Store) func(context.Context) error {
		u := fenceUser(t, st, "viewer")
		s, err := st.CreateSession(t.Context(), store.Session{
			ID: uuid.NewString(), TokenHash: uuid.NewString(), UserID: u.ID,
			ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(),
		})
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		return func(ctx context.Context) error { return st.DeleteSession(ctx, s.ID) }
	},
	"DeleteStorageLocation": func(t *testing.T, st store.Store) func(context.Context) error {
		loc := mustCreateStorageLocation(t, st, uuid.NewString())
		return func(ctx context.Context) error { return st.DeleteStorageLocation(ctx, loc.ID) }
	},
	"DeleteTerminalJobsBefore": func(t *testing.T, st store.Store) func(context.Context) error {
		seedGraph(t, st, graphOpts{jobStatus: store.JobStatusCompleted},
			stepSpec{name: "a", status: store.StepStatusCompleted, tasks: []store.TaskStatus{store.TaskStatusSucceeded}})
		return func(ctx context.Context) error {
			_, err := st.DeleteTerminalJobsBefore(ctx, time.Now().Add(time.Hour), true)
			return err
		}
	},
	"DeleteUsagePool": func(t *testing.T, st store.Store) func(context.Context) error {
		p := seedPool(t, st, 1)
		return func(ctx context.Context) error { return st.DeleteUsagePool(ctx, p.ID) }
	},
	"DeleteUser": func(t *testing.T, st store.Store) func(context.Context) error {
		u := fenceUser(t, st, "viewer")
		return func(ctx context.Context) error { return st.DeleteUser(ctx, u.ID) }
	},
	"DeleteWorkerIfRemovable": func(t *testing.T, st store.Store) func(context.Context) error {
		w := fenceWorker(t, st, store.WorkerStatusOffline, time.Now().Add(-time.Hour))
		return func(ctx context.Context) error { return st.DeleteWorkerIfRemovable(ctx, w.ID) }
	},
	"DemoteStalledJobs": func(t *testing.T, st store.Store) func(context.Context) error {
		seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning},
			stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusReady}})
		return func(ctx context.Context) error {
			_, err := st.DemoteStalledJobs(ctx, time.Now().UTC())
			return err
		}
	},
	"FinalizeJob": func(t *testing.T, st store.Store) func(context.Context) error {
		g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning},
			stepSpec{name: "a", status: store.StepStatusCompleted, tasks: []store.TaskStatus{store.TaskStatusSucceeded}})
		return func(ctx context.Context) error {
			_, _, err := st.FinalizeJob(ctx, g.Job.ID, time.Now().UTC())
			return err
		}
	},
	"FinalizeStep": func(t *testing.T, st store.Store) func(context.Context) error {
		g := fenceLiveJob(t, st, store.TaskStatusSucceeded)
		return func(ctx context.Context) error {
			_, _, err := st.FinalizeStep(ctx, g.Steps["a"].ID, time.Now().UTC())
			return err
		}
	},
	"LeaseTask": func(t *testing.T, st store.Store) func(context.Context) error {
		g := fenceLiveJob(t, st, store.TaskStatusReady)
		req := leaseReq(g.Tasks["a"][0])
		return func(ctx context.Context) error {
			res, err := st.LeaseTask(ctx, req)
			if err == nil && res.Outcome != store.LeaseLeased {
				return fmt.Errorf("LeaseTask outcome = %q, want %q", res.Outcome, store.LeaseLeased)
			}
			return err
		}
	},
	"MarkWorkerJoinTokenUsed": func(t *testing.T, st store.Store) func(context.Context) error {
		tok := mustCreateJoinToken(t, st)
		return func(ctx context.Context) error { return st.MarkWorkerJoinTokenUsed(ctx, tok.ID, time.Now().UTC()) }
	},
	"OfflineStaleWorker": func(t *testing.T, st store.Store) func(context.Context) error {
		w := fenceWorker(t, st, store.WorkerStatusOnline, time.Now().Add(-time.Hour))
		return func(ctx context.Context) error {
			_, ok, err := st.OfflineStaleWorker(ctx, w.ID, time.Now().UTC(), time.Now().UTC())
			if err == nil && !ok {
				return errors.New("OfflineStaleWorker wrote nothing")
			}
			return err
		}
	},
	"OfflineWorker": func(t *testing.T, st store.Store) func(context.Context) error {
		w := fenceWorker(t, st, store.WorkerStatusOnline, time.Now())
		return func(ctx context.Context) error {
			_, ok, err := st.OfflineWorker(ctx, w.ID, "", time.Now().UTC())
			if err == nil && !ok {
				return errors.New("OfflineWorker wrote nothing")
			}
			return err
		}
	},
	"ParkJob": func(t *testing.T, st store.Store) func(context.Context) error {
		g := fenceLiveJob(t, st, store.TaskStatusReady)
		return func(ctx context.Context) error {
			return st.ParkJob(ctx, g.Job.ID, "too many failures", time.Now().UTC())
		}
	},
	"PauseJob": func(t *testing.T, st store.Store) func(context.Context) error {
		g := fenceLiveJob(t, st, store.TaskStatusReady)
		return func(ctx context.Context) error { return st.PauseJob(ctx, g.Job.ID, time.Now().UTC()) }
	},
	"PromoteJobRunning": func(t *testing.T, st store.Store) func(context.Context) error {
		g := fenceLiveJob(t, st, store.TaskStatusReady)
		return func(ctx context.Context) error {
			ok, err := st.PromoteJobRunning(ctx, g.Job.ID, time.Now().UTC())
			if err == nil && !ok {
				return errors.New("PromoteJobRunning wrote nothing")
			}
			return err
		}
	},
	"ReclaimStaleAssignedTasks": func(t *testing.T, st store.Store) func(context.Context) error {
		fenceLiveJob(t, st, store.TaskStatusAssigned)
		return func(ctx context.Context) error {
			_, err := st.ReclaimStaleAssignedTasks(ctx, time.Now().UTC().Add(time.Minute))
			return err
		}
	},
	"ReclaimTaskAttempt": func(t *testing.T, st store.Store) func(context.Context) error {
		g := fenceLiveJob(t, st, store.TaskStatusRunning)
		task := g.Tasks["a"][0]
		return func(ctx context.Context) error {
			ok, err := st.ReclaimTaskAttempt(ctx, g.Attempts[task.ID].ID, task.ID, time.Now().UTC())
			if err == nil && !ok {
				return errors.New("ReclaimTaskAttempt reclaimed nothing")
			}
			return err
		}
	},
	"RecordAttemptProgress": func(t *testing.T, st store.Store) func(context.Context) error {
		g := fenceLiveJob(t, st, store.TaskStatusRunning)
		task := g.Tasks["a"][0]
		a := g.Attempts[task.ID]
		one := 1
		return func(ctx context.Context) error {
			out, err := st.RecordAttemptProgress(ctx, store.AttemptProgress{AttemptID: a.ID, TaskID: task.ID, Progress: &one, At: time.Now().UTC()})
			if err == nil && out != store.ProgressRecorded {
				return fmt.Errorf("RecordAttemptProgress outcome = %v, want recorded", out)
			}
			return err
		}
	},
	"RecordTaskFailure": func(t *testing.T, st store.Store) func(context.Context) error {
		g := fenceLiveJob(t, st, store.TaskStatusRunning)
		task := g.Tasks["a"][0]
		return func(ctx context.Context) error {
			_, _, first, err := st.RecordTaskFailure(ctx, g.Attempts[task.ID].ID, task.ID, nil, "", "boom", time.Now().UTC())
			if err == nil && !first {
				return errors.New("RecordTaskFailure was not the first close")
			}
			return err
		}
	},
	"RedeemWorkerJoinToken": func(t *testing.T, st store.Store) func(context.Context) error {
		tok := mustCreateJoinToken(t, st)
		return func(ctx context.Context) error {
			_, err := st.RedeemWorkerJoinToken(ctx, tok.TokenHash, time.Now().UTC(), mkCredential(uuid.NewString()))
			return err
		}
	},
	"RegisterWorker": func(t *testing.T, st store.Store) func(context.Context) error {
		farm := fenceFarm(t, st)
		beat := time.Now().UTC()
		return func(ctx context.Context) error {
			_, _, err := st.RegisterWorker(ctx, store.Worker{
				ID: uuid.NewString(), FarmID: farm.ID, Hostname: "node", Status: store.WorkerStatusOnline, LastHeartbeatAt: &beat,
			})
			return err
		}
	},
	"ReleaseBlockedJob": func(t *testing.T, st store.Store) func(context.Context) error {
		g := fenceBlockedJob(t, st)
		return func(ctx context.Context) error {
			ok, err := st.ReleaseBlockedJob(ctx, g.Job.ID, time.Now().UTC())
			if err == nil && !ok {
				return errors.New("ReleaseBlockedJob released nothing")
			}
			return err
		}
	},
	"ReleaseStep": func(t *testing.T, st store.Store) func(context.Context) error {
		g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning},
			stepSpec{name: "a", status: store.StepStatusPending, tasks: []store.TaskStatus{store.TaskStatusPending}})
		return func(ctx context.Context) error {
			ok, _, err := st.ReleaseStep(ctx, g.Steps["a"].ID, time.Now().UTC())
			if err == nil && !ok {
				return errors.New("ReleaseStep released nothing")
			}
			return err
		}
	},
	"RequeueTaskForRetry": func(t *testing.T, st store.Store) func(context.Context) error {
		g := fenceLiveJob(t, st, store.TaskStatusReady)
		task := g.Tasks["a"][0]
		a := storetest.Lease(t, st, leaseReq(task))
		if _, _, first, err := st.RecordTaskFailure(t.Context(), a.ID, task.ID, nil, "", "boom", time.Now().UTC()); err != nil || !first {
			t.Fatalf("RecordTaskFailure = (first %v, %v)", first, err)
		}
		return func(ctx context.Context) error {
			ok, err := st.RequeueTaskForRetry(ctx, task.ID, a.ID, time.Now().UTC(), time.Now().UTC())
			if err == nil && !ok {
				return errors.New("RequeueTaskForRetry requeued nothing")
			}
			return err
		}
	},
	"ResumeJob": func(t *testing.T, st store.Store) func(context.Context) error {
		g := fenceLiveJob(t, st, store.TaskStatusReady)
		if err := st.PauseJob(t.Context(), g.Job.ID, time.Now().UTC()); err != nil {
			t.Fatalf("PauseJob: %v", err)
		}
		return func(ctx context.Context) error { return st.ResumeJob(ctx, g.Job.ID, time.Now().UTC()) }
	},
	"RetryTasks": func(t *testing.T, st store.Store) func(context.Context) error {
		g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning},
			stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusFailed, store.TaskStatusRunning}})
		ids := []string{g.Tasks["a"][0].ID}
		return func(ctx context.Context) error {
			revived, err := st.RetryTasks(ctx, g.Job.ID, ids, time.Now().UTC())
			if err == nil && len(revived) != 1 {
				return fmt.Errorf("RetryTasks revived %d tasks, want 1", len(revived))
			}
			return err
		}
	},
	"RevokeAPIKey": func(t *testing.T, st store.Store) func(context.Context) error {
		u := fenceUser(t, st, "viewer")
		k, err := st.CreateAPIKey(t.Context(), mkAPIKey(u.ID, "hash-"+uuid.NewString()))
		if err != nil {
			t.Fatalf("CreateAPIKey: %v", err)
		}
		return func(ctx context.Context) error { return st.RevokeAPIKey(ctx, k.ID, u.ID, time.Now().UTC()) }
	},
	"RevokeWorkerCredential": func(t *testing.T, st store.Store) func(context.Context) error {
		c := mustCreateCredential(t, st, mkCredential(uuid.NewString()))
		return func(ctx context.Context) error { return st.RevokeWorkerCredential(ctx, c.WorkerID, time.Now().UTC()) }
	},
	"SetBrokerIdentity": func(_ *testing.T, st store.Store) func(context.Context) error {
		return func(ctx context.Context) error { return st.SetBrokerIdentity(ctx, "embedded:SQI", time.Now().UTC()) }
	},
	"SetTaskUnschedulableReason": func(t *testing.T, st store.Store) func(context.Context) error {
		g := fenceLiveJob(t, st, store.TaskStatusReady)
		return func(ctx context.Context) error {
			ok, err := st.SetTaskUnschedulableReason(ctx, g.Tasks["a"][0].ID, "no worker has houdini")
			if err == nil && !ok {
				return errors.New("SetTaskUnschedulableReason changed nothing")
			}
			return err
		}
	},
	"SetUserDisplayName": func(t *testing.T, st store.Store) func(context.Context) error {
		u := fenceUser(t, st, "viewer")
		return func(ctx context.Context) error {
			_, err := st.SetUserDisplayName(ctx, u.ID, "Renamed")
			return err
		}
	},
	"SetUserPassword": func(t *testing.T, st store.Store) func(context.Context) error {
		u := fenceUser(t, st, "viewer")
		return func(ctx context.Context) error { return st.SetUserPassword(ctx, u.ID, "$argon2id$new") }
	},
	"SetUserPasswordAndEvictSessions": func(t *testing.T, st store.Store) func(context.Context) error {
		u := fenceUser(t, st, "viewer")
		return func(ctx context.Context) error { return st.SetUserPasswordAndEvictSessions(ctx, u.ID, "$argon2id$new") }
	},
	"SetWorkerDisabled": func(t *testing.T, st store.Store) func(context.Context) error {
		w := fenceWorker(t, st, store.WorkerStatusOnline, time.Now())
		return func(ctx context.Context) error {
			_, err := st.SetWorkerDisabled(ctx, w.ID, true)
			return err
		}
	},
	"StartTaskAttempt": func(t *testing.T, st store.Store) func(context.Context) error {
		g := fenceLiveJob(t, st, store.TaskStatusAssigned)
		task := g.Tasks["a"][0]
		a := g.Attempts[task.ID]
		return func(ctx context.Context) error {
			started, err := st.StartTaskAttempt(ctx, a.ID, task.ID, "", a.StartedAt)
			if err == nil && !started {
				return errors.New("StartTaskAttempt started nothing")
			}
			return err
		}
	},
	"TouchAPIKeyLastUsed": func(t *testing.T, st store.Store) func(context.Context) error {
		u := fenceUser(t, st, "viewer")
		k, err := st.CreateAPIKey(t.Context(), mkAPIKey(u.ID, "hash-"+uuid.NewString()))
		if err != nil {
			t.Fatalf("CreateAPIKey: %v", err)
		}
		return func(ctx context.Context) error { return st.TouchAPIKeyLastUsed(ctx, k.ID, time.Now().UTC()) }
	},
	"TouchWorkerCredential": func(t *testing.T, st store.Store) func(context.Context) error {
		c := mustCreateCredential(t, st, mkCredential(uuid.NewString()))
		return func(ctx context.Context) error { return st.TouchWorkerCredential(ctx, c.WorkerID, time.Now().UTC()) }
	},
	"UpdateComputeLocation": func(t *testing.T, st store.Store) func(context.Context) error {
		loc := mustCreateComputeLocation(t, st, uuid.NewString())
		loc.Description = "fenced"
		return func(ctx context.Context) error {
			_, err := st.UpdateComputeLocation(ctx, loc)
			return err
		}
	},
	"UpdateFarm": func(t *testing.T, st store.Store) func(context.Context) error {
		farm := fenceFarm(t, st)
		farm.MaxConcurrentTasks = 7
		return func(ctx context.Context) error {
			_, err := st.UpdateFarm(ctx, farm)
			return err
		}
	},
	"UpdateJob": func(t *testing.T, st store.Store) func(context.Context) error {
		g := fenceLiveJob(t, st, store.TaskStatusReady)
		job := g.Job
		job.Priority = 77
		return func(ctx context.Context) error {
			_, err := st.UpdateJob(ctx, job)
			return err
		}
	},
	"UpdateProduct": func(t *testing.T, st store.Store) func(context.Context) error {
		p := mustCreateProduct(t, st, uuid.NewString())
		p.Description = "fenced"
		return func(ctx context.Context) error {
			_, err := st.UpdateProduct(ctx, p)
			return err
		}
	},
	"UpdateQueue": func(t *testing.T, st store.Store) func(context.Context) error {
		_, q := fenceFarmQueue(t, st)
		q.MaxConcurrentTasks = 7
		return func(ctx context.Context) error {
			_, err := st.UpdateQueue(ctx, q)
			return err
		}
	},
	"UpdateStorageLocation": func(t *testing.T, st store.Store) func(context.Context) error {
		loc := mustCreateStorageLocation(t, st, uuid.NewString())
		loc.Description = "fenced"
		return func(ctx context.Context) error {
			_, err := st.UpdateStorageLocation(ctx, loc)
			return err
		}
	},
	"UpdateUsagePool": func(t *testing.T, st store.Store) func(context.Context) error {
		p := seedPool(t, st, 1)
		p.MaxConcurrent = 5
		return func(ctx context.Context) error {
			_, err := st.UpdateUsagePool(ctx, p)
			return err
		}
	},
	"UpdateUser": func(t *testing.T, st store.Store) func(context.Context) error {
		u := fenceUser(t, st, "viewer")
		u.DisplayName = "Renamed"
		return func(ctx context.Context) error {
			_, err := st.UpdateUser(ctx, u)
			return err
		}
	},
	"UpdateUserKeepingAdmin": func(t *testing.T, st store.Store) func(context.Context) error {
		u := fenceUser(t, st, "viewer")
		u.DisplayName = "Renamed"
		return func(ctx context.Context) error {
			_, err := st.UpdateUserKeepingAdmin(ctx, u)
			return err
		}
	},
	"UpdateWorkerHeartbeat": func(t *testing.T, st store.Store) func(context.Context) error {
		w := fenceWorker(t, st, store.WorkerStatusOnline, time.Now().Add(-time.Hour))
		return func(ctx context.Context) error {
			now := time.Now().UTC()
			return st.UpdateWorkerHeartbeat(ctx, w.ID, now, &now)
		}
	},
}

// fenceFarm creates a farm of its own.
func fenceFarm(t *testing.T, st store.Store) store.Farm {
	t.Helper()
	return mustCreateFarm(t, st, uuid.NewString())
}

// fenceFarmQueue creates a farm and a queue in it, both of their own.
func fenceFarmQueue(t *testing.T, st store.Store) (store.Farm, store.Queue) {
	t.Helper()
	farm := fenceFarm(t, st)
	return farm, mustCreateQueue(t, st, store.Queue{FarmID: farm.ID, Name: uuid.NewString()})
}

func fenceUser(t *testing.T, st store.Store, role string) store.User {
	t.Helper()
	u, err := st.CreateUser(t.Context(), mkUser(role))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u
}

// fenceWorker registers a worker of its own, in a farm of its own.
func fenceWorker(t *testing.T, st store.Store, status store.WorkerStatus, beat time.Time) store.Worker {
	t.Helper()
	beat = beat.UTC()
	w, _, err := st.RegisterWorker(t.Context(), store.Worker{
		ID: uuid.NewString(), FarmID: fenceFarm(t, st).ID, Hostname: "node", Status: status, LastHeartbeatAt: &beat,
	})
	if err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	return w
}

// fenceLiveJob seeds a pending job with one ready step "a" holding one task in
// the given status (leased and started, for assigned and running).
func fenceLiveJob(t *testing.T, st store.Store, task store.TaskStatus) jobGraph {
	t.Helper()
	return seedGraph(t, st, graphOpts{},
		stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{task}})
}

// fenceBlockedJob seeds a blocked job whose only upstream has completed, so it
// can be released as well as canceled.
func fenceBlockedJob(t *testing.T, st store.Store) jobGraph {
	t.Helper()
	up := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning})
	if _, _, err := st.FinalizeJob(t.Context(), up.Job.ID, time.Now().UTC()); err != nil {
		t.Fatalf("FinalizeJob upstream: %v", err)
	}
	return blockedOn(t, st, up)
}

// TestFence_EveryStoreMethodClassified: every method of store.Store is either a
// fenced write with an entry in fenceMutators or is listed in fenceReadOnly,
// and never both. A method added to the interface fails here until it is
// classified, so a new write cannot ship outside the fence unnoticed.
func TestFence_EveryStoreMethodClassified(t *testing.T) {
	iface := reflect.TypeFor[store.Store]()
	readOnly := map[string]bool{}
	for _, name := range fenceReadOnly {
		if readOnly[name] {
			t.Errorf("%s is listed twice in fenceReadOnly", name)
		}
		readOnly[name] = true
	}
	methods := map[string]bool{}
	for m := range iface.Methods() {
		name := m.Name
		methods[name] = true
		_, mutator := fenceMutators[name]
		switch {
		case mutator && readOnly[name]:
			t.Errorf("%s is in both fenceMutators and fenceReadOnly", name)
		case !mutator && !readOnly[name]:
			t.Errorf("%s is in neither fenceMutators nor fenceReadOnly: classify it", name)
		}
	}
	for name := range fenceMutators {
		if !methods[name] {
			t.Errorf("fenceMutators has %s, which is not a store.Store method", name)
		}
	}
	for name := range readOnly {
		if !methods[name] {
			t.Errorf("fenceReadOnly has %s, which is not a store.Store method", name)
		}
	}
}

// fenceStoresAfterTakeover returns every backend, the fake included, with the lease taken
// over so that epoch 1 is stale and epoch 2 current.
func fenceStoresAfterTakeover(t *testing.T) (stores map[string]store.Store, stale, current context.Context) {
	t.Helper()
	stores = newStores(t)
	for _, st := range stores {
		takeOver(t, st)
	}
	return stores, store.WithLeaderEpoch(t.Context(), 1), store.WithLeaderEpoch(t.Context(), 2)
}

// TestParity_Fence_EveryMutatorRefusesStaleEpoch: on every backend, every
// fenced write made under a deposed leader's epoch fails with ErrFenced.
func TestParity_Fence_EveryMutatorRefusesStaleEpoch(t *testing.T) {
	stores, stale, _ := fenceStoresAfterTakeover(t)
	for name, st := range stores {
		t.Run(name, func(t *testing.T) {
			for _, m := range slices.Sorted(maps.Keys(fenceMutators)) {
				t.Run(m, func(t *testing.T) {
					call := fenceMutators[m](t, st)
					if err := call(stale); !errors.Is(err, store.ErrFenced) {
						t.Fatalf("%s under a stale epoch: err = %v, want ErrFenced", m, err)
					}
				})
			}
		})
	}
}

// TestParity_Fence_EveryMutatorSucceedsAtCurrentEpoch: the same writes under
// the current leader's epoch succeed, so the fence is not a blanket refusal
// and each fixture is a write that would have landed.
func TestParity_Fence_EveryMutatorSucceedsAtCurrentEpoch(t *testing.T) {
	stores, _, current := fenceStoresAfterTakeover(t)
	for name, st := range stores {
		t.Run(name, func(t *testing.T) {
			for _, m := range slices.Sorted(maps.Keys(fenceMutators)) {
				t.Run(m, func(t *testing.T) {
					call := fenceMutators[m](t, st)
					if err := call(current); err != nil {
						t.Fatalf("%s under the current epoch: %v", m, err)
					}
				})
			}
		})
	}
}
