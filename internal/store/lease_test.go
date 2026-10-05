// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

func leaseReq(task store.Task, claims ...store.UsagePoolClaim) store.LeaseRequest {
	return store.LeaseRequest{TaskID: task.ID, WorkerID: fixtureWorkerID, AttemptID: uuid.NewString(), Now: time.Now().UTC(), Claims: claims}
}

// poolClaim builds a claim on p whose MaxConcurrent is deliberately wrong:
// LeaseTask must read the pool's cap in its own transaction, never trust the
// caller's copy.
func poolClaim(p store.UsagePool) store.UsagePoolClaim {
	return store.UsagePoolClaim{ClaimID: uuid.NewString(), PoolID: p.ID, PoolName: p.Name, MaxConcurrent: 999}
}

func mustAttempts(t *testing.T, st store.Store, taskID string) []store.TaskAttempt {
	t.Helper()
	as, err := st.ListTaskAttempts(t.Context(), taskID)
	if err != nil {
		t.Fatalf("ListTaskAttempts %s: %v", taskID, err)
	}
	return as
}

// mustLease leases req and fails unless the outcome is want. A non-leased
// outcome must not carry an attempt.
func mustLease(t *testing.T, st store.Store, req store.LeaseRequest, want store.LeaseOutcome) store.LeaseResult {
	t.Helper()
	res, err := st.LeaseTask(t.Context(), req)
	if err != nil || res.Outcome != want {
		t.Fatalf("LeaseTask = (%+v, %v), want %s", res, err, want)
	}
	if want != store.LeaseLeased && res.Attempt != (store.TaskAttempt{}) {
		t.Fatalf("LeaseTask %s carried attempt %+v, want none", want, res.Attempt)
	}
	return res
}

// assertNothingWritten checks a non-leased outcome left no trace: the task row
// is exactly as it was before the call, the task (which had no attempts) still
// has none, and each given pool (which had no active claims) still has none.
func assertNothingWritten(t *testing.T, st store.Store, before store.Task, pools ...store.UsagePool) {
	t.Helper()
	assertTaskUntouched(t, st, before)
	if as := mustAttempts(t, st, before.ID); len(as) != 0 {
		t.Fatalf("attempts = %+v, want none", as)
	}
	for _, p := range pools {
		if n := activeClaims(t, st, p.ID); n != 0 {
			t.Fatalf("pool %s active claims = %d, want 0", p.Name, n)
		}
	}
}

// assertTaskUntouched fails unless the task still matches before in every field
// a lease or a refused lease would write.
func assertTaskUntouched(t *testing.T, st store.Store, before store.Task) {
	t.Helper()
	got := mustTask(t, st, before.ID)
	if got.Status != before.Status || got.AssignedWorkerID != before.AssignedWorkerID ||
		got.UnschedulableReason != before.UnschedulableReason || !got.UpdatedAt.Equal(before.UpdatedAt) ||
		!sameTime(got.AssignedAt, before.AssignedAt) {
		t.Fatalf("task = %+v, want it left as %+v", got, before)
	}
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// seedStepA seeds a graph whose one ready step, "a", holds tasks in the given
// statuses.
func seedStepA(t *testing.T, st store.Store, opts graphOpts, tasks ...store.TaskStatus) jobGraph {
	t.Helper()
	return seedGraph(t, st, opts, stepSpec{name: "a", status: store.StepStatusReady, tasks: tasks})
}

// holdClaims gives each task a running attempt holding a claim on pool, so
// the pool has one active claim per task.
func holdClaims(t *testing.T, st store.Store, pool store.UsagePool, tasks ...store.Task) {
	t.Helper()
	for _, task := range tasks {
		seedClaim(t, st, pool.ID, seedAttempt(t, st, task, store.AttemptStatusRunning).ID)
	}
}

func TestLeaseTask_Leased(t *testing.T) {
	cases := []struct {
		name  string
		prior int // failed attempts before this lease
	}{
		{"first attempt", 0},
		{"retry", 1},
		{"third attempt", 2},
	}
	for _, tc := range cases {
		for name, st := range newStores(t) {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				g := seedStepA(t, st, graphOpts{}, store.TaskStatusReady)
				task := g.Tasks["a"][0]
				pool := seedPool(t, st, 1)
				for range tc.prior {
					seedAttempt(t, st, task, store.AttemptStatusFailed)
				}
				req := leaseReq(task, poolClaim(pool))

				res := mustLease(t, st, req, store.LeaseLeased)
				want := tc.prior + 1
				if a := res.Attempt; a.ID != req.AttemptID || a.TaskID != task.ID || a.AttemptNumber != want ||
					a.Status != store.AttemptStatusRunning || a.WorkerID != fixtureWorkerID || a.EndedAt != nil {
					t.Fatalf("attempt = %+v, want #%d %s running on %s", a, want, req.AttemptID, fixtureWorkerID)
				}
				if stored := mustAttempt(t, st, req.AttemptID); stored.AttemptNumber != want || stored.Status != store.AttemptStatusRunning {
					t.Fatalf("stored attempt = %+v, want #%d running", stored, want)
				}
				if as := mustAttempts(t, st, task.ID); len(as) != want {
					t.Fatalf("attempts = %d, want %d", len(as), want)
				}
				got := mustTask(t, st, task.ID)
				if got.Status != store.TaskStatusAssigned || got.AssignedWorkerID != fixtureWorkerID || got.AssignedAt == nil {
					t.Fatalf("task = %+v, want assigned to %s", got, fixtureWorkerID)
				}
				if n := activeClaims(t, st, pool.ID); n != 1 {
					t.Fatalf("active claims = %d, want 1", n)
				}
				// A claim on the wrong (failed) attempt would show up here.
				if v := claimViolations(t, st); len(v) != 0 {
					t.Fatalf("I3 violations: %v", v)
				}
			})
		}
	}
}

func TestLeaseTask_Lost(t *testing.T) {
	cases := []struct {
		name string
		opts graphOpts
		task store.TaskStatus
	}{
		{"already assigned", graphOpts{}, store.TaskStatusAssigned},
		{"already running", graphOpts{}, store.TaskStatusRunning},
		{"not yet ready", graphOpts{}, store.TaskStatusPending},
		{"job paused", graphOpts{jobStatus: store.JobStatusPaused}, store.TaskStatusReady},
		{"job canceled", graphOpts{jobStatus: store.JobStatusCanceled}, store.TaskStatusReady},
		{"job completed", graphOpts{jobStatus: store.JobStatusCompleted}, store.TaskStatusReady},
		{"queue paused", graphOpts{queuePaused: true}, store.TaskStatusReady},
	}
	for _, tc := range cases {
		for name, st := range newStores(t) {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				g := seedStepA(t, st, tc.opts, tc.task)
				pool := seedPool(t, st, 0)
				mustLease(t, st, leaseReq(g.Tasks["a"][0], poolClaim(pool)), store.LeaseLost)
				assertNothingWritten(t, st, g.Tasks["a"][0], pool)
			})
		}
	}
	for name, st := range newStores(t) {
		t.Run("unknown task/"+name, func(t *testing.T) {
			pool := seedPool(t, st, 0)
			mustLease(t, st, leaseReq(store.Task{ID: "nope"}, poolClaim(pool)), store.LeaseLost)
			if as := mustAttempts(t, st, "nope"); len(as) != 0 {
				t.Fatalf("attempts = %+v, want none", as)
			}
			if n := activeClaims(t, st, pool.ID); n != 0 {
				t.Fatalf("active claims = %d, want 0", n)
			}
		})
	}
}

// TestLeaseTask_PausedAfterListing pins Review Focus #3: the task's job or
// queue is paused after ListReadyTasks chose it and before LeaseTask runs. The
// lease must come back Lost with no attempt or claim rows.
func TestLeaseTask_PausedAfterListing(t *testing.T) {
	pauses := []struct {
		name  string
		pause func(t *testing.T, st store.Store, g jobGraph)
	}{
		{"job", func(t *testing.T, st store.Store, g jobGraph) {
			t.Helper()
			if err := st.PauseJob(t.Context(), g.Job.ID, time.Now().UTC()); err != nil {
				t.Fatalf("PauseJob: %v", err)
			}
		}},
		{"queue", func(t *testing.T, st store.Store, g jobGraph) {
			t.Helper()
			q := g.Queue
			q.Paused = true
			if _, err := st.UpdateQueue(t.Context(), q); err != nil {
				t.Fatalf("UpdateQueue: %v", err)
			}
		}},
	}
	for _, p := range pauses {
		for name, st := range newStores(t) {
			t.Run(p.name+"/"+name, func(t *testing.T) {
				g := seedStepA(t, st, graphOpts{}, store.TaskStatusReady)
				task := g.Tasks["a"][0]
				pool := seedPool(t, st, 1)
				listed, err := st.ListReadyTasks(t.Context(), g.Farm.ID, time.Now().UTC(), 10)
				if err != nil {
					t.Fatalf("ListReadyTasks: %v", err)
				}
				if !slices.ContainsFunc(listed, func(x store.Task) bool { return x.ID == task.ID }) {
					t.Fatalf("ListReadyTasks = %+v, want it to offer %s", listed, task.ID)
				}

				p.pause(t, st, g)
				before := mustTask(t, st, task.ID)
				mustLease(t, st, leaseReq(task, poolClaim(pool)), store.LeaseLost)
				assertNothingWritten(t, st, before, pool)
			})
		}
	}
}

// TestLeaseTask_Backoff pins the retry_after half of the eligibility guard.
func TestLeaseTask_Backoff(t *testing.T) {
	cases := []struct {
		name  string
		after time.Duration // retry_after relative to now
		want  store.LeaseOutcome
	}{
		{"still backing off", time.Hour, store.LeaseLost},
		{"backoff elapsed", -time.Minute, store.LeaseLeased},
	}
	for _, tc := range cases {
		for name, st := range newStores(t) {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				g := seedStepA(t, st, graphOpts{}, store.TaskStatusAssigned)
				now := time.Now().UTC()
				// The requeue is guarded on the reporting attempt being the
				// task's latest, so this seeds the task's live (running) attempt
				// for the guard to pass.
				failed := seedAttempt(t, st, g.Tasks["a"][0], store.AttemptStatusRunning)
				if ok, err := st.RequeueTaskForRetry(t.Context(), g.Tasks["a"][0].ID, failed.ID, now.Add(tc.after), now); err != nil || !ok {
					t.Fatalf("RequeueTaskForRetry = (%v, %v), want requeued", ok, err)
				}
				before := mustTask(t, st, g.Tasks["a"][0].ID)

				mustLease(t, st, leaseReq(before), tc.want)
				if tc.want == store.LeaseLost {
					// assertNothingWritten demands no attempts; the seeded one stays, and no second is opened.
					assertTaskUntouched(t, st, before)
					if as := mustAttempts(t, st, before.ID); len(as) != 1 {
						t.Fatalf("attempts = %+v, want only the seeded one", as)
					}
				} else if got := mustTask(t, st, before.ID); got.Status != store.TaskStatusAssigned {
					t.Fatalf("task = %q, want assigned", got.Status)
				}
			})
		}
	}
}

// TestLeaseTask_Caps pins invariant I5 for queues and farms: the active count,
// including the task being leased, may reach the cap but never pass it.
func TestLeaseTask_Caps(t *testing.T) {
	cases := []struct {
		name   string
		opts   graphOpts
		active store.TaskStatus // the step's other task
		want   store.LeaseOutcome
	}{
		{"queue full", graphOpts{queueCap: 1}, store.TaskStatusRunning, store.LeaseQueueFull},
		{"farm full", graphOpts{farmCap: 1}, store.TaskStatusAssigned, store.LeaseFarmFull},
		{"queue checked before farm", graphOpts{queueCap: 1, farmCap: 1}, store.TaskStatusRunning, store.LeaseQueueFull},
		{"queue has room", graphOpts{queueCap: 2}, store.TaskStatusRunning, store.LeaseLeased},
		{"farm has room", graphOpts{farmCap: 2}, store.TaskStatusAssigned, store.LeaseLeased},
		{"finished work does not count", graphOpts{queueCap: 1, farmCap: 1}, store.TaskStatusSucceeded, store.LeaseLeased},
	}
	for _, tc := range cases {
		for name, st := range newStores(t) {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				g := seedStepA(t, st, tc.opts, tc.active, store.TaskStatusReady)
				task := g.Tasks["a"][1]
				mustLease(t, st, leaseReq(task), tc.want)
				if tc.want != store.LeaseLeased {
					assertNothingWritten(t, st, task)
					return
				}
				if got := mustTask(t, st, task.ID); got.Status != store.TaskStatusAssigned {
					t.Fatalf("task = %q, want assigned", got.Status)
				}
			})
		}
	}
}

// TestLeaseTask_PoolCapReadInTransaction pins Review Focus #4 and invariant
// I5 for pools: the cap is the pool row's as read in the lease's transaction,
// never the caller's copy (poolClaim says 999), so a pool deleted, or lowered
// below its current use, after the eligibility check makes the lease PoolFull
// with nothing written.
func TestLeaseTask_PoolCapReadInTransaction(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run("at cap/"+name, func(t *testing.T) {
			g := seedStepA(t, st, graphOpts{}, store.TaskStatusRunning, store.TaskStatusReady)
			pool := seedPool(t, st, 1)
			holdClaims(t, st, pool, g.Tasks["a"][0])

			res := mustLease(t, st, leaseReq(g.Tasks["a"][1], poolClaim(pool)), store.LeasePoolFull)
			if res.FullPool != pool.Name {
				t.Fatalf("FullPool = %q, want %q", res.FullPool, pool.Name)
			}
			assertNothingWritten(t, st, g.Tasks["a"][1])
			if n := activeClaims(t, st, pool.ID); n != 1 {
				t.Fatalf("active claims = %d, want the 1 held", n)
			}
		})
		t.Run("lowered below use/"+name, func(t *testing.T) {
			g := seedStepA(t, st, graphOpts{}, store.TaskStatusRunning, store.TaskStatusRunning, store.TaskStatusReady)
			pool := seedPool(t, st, 3)
			holdClaims(t, st, pool, g.Tasks["a"][:2]...)
			req := leaseReq(g.Tasks["a"][2], poolClaim(pool)) // built while the pool had room
			pool.MaxConcurrent = 1
			if _, err := st.UpdateUsagePool(t.Context(), pool); err != nil {
				t.Fatalf("UpdateUsagePool: %v", err)
			}

			res := mustLease(t, st, req, store.LeasePoolFull)
			if res.FullPool != pool.Name {
				t.Fatalf("FullPool = %q, want %q", res.FullPool, pool.Name)
			}
			assertNothingWritten(t, st, g.Tasks["a"][2])
			if n := activeClaims(t, st, pool.ID); n != 2 {
				t.Fatalf("active claims = %d, want the 2 held", n)
			}
		})
		t.Run("deleted/"+name, func(t *testing.T) {
			g := seedStepA(t, st, graphOpts{}, store.TaskStatusReady)
			gone := seedPool(t, st, 1)
			req := leaseReq(g.Tasks["a"][0], poolClaim(gone)) // built while the pool existed
			if err := st.DeleteUsagePool(t.Context(), gone.ID); err != nil {
				t.Fatalf("DeleteUsagePool: %v", err)
			}

			res := mustLease(t, st, req, store.LeasePoolFull)
			if res.FullPool != gone.Name {
				t.Fatalf("FullPool = %q, want the caller's name %q", res.FullPool, gone.Name)
			}
			assertNothingWritten(t, st, g.Tasks["a"][0])
		})
		t.Run("unlimited/"+name, func(t *testing.T) {
			g := seedStepA(t, st, graphOpts{}, store.TaskStatusRunning, store.TaskStatusRunning, store.TaskStatusReady)
			pool := seedPool(t, st, 0)
			holdClaims(t, st, pool, g.Tasks["a"][:2]...)

			mustLease(t, st, leaseReq(g.Tasks["a"][2], poolClaim(pool)), store.LeaseLeased)
			if n := activeClaims(t, st, pool.ID); n != 3 {
				t.Fatalf("active claims = %d, want 3", n)
			}
		})
	}
}

// seedPoolWithID creates a pool whose ID starts with prefix, so a test can
// control the order LeaseTask visits pools in (sorted by ID).
func seedPoolWithID(t *testing.T, st store.Store, prefix string, maxConcurrent int) store.UsagePool {
	t.Helper()
	p, err := st.CreateUsagePool(t.Context(), store.UsagePool{ID: prefix + uuid.NewString(), Name: uuid.NewString(), MaxConcurrent: maxConcurrent})
	if err != nil {
		t.Fatalf("CreateUsagePool: %v", err)
	}
	return p
}

// TestLeaseTask_PoolsVisitedInIDOrder pins that pools are checked in pool-ID
// order, whatever order the request lists them in, so both backends report
// the same full pool, and that a pool found full after an earlier pool's claim
// was written leaves that earlier claim unwritten too.
func TestLeaseTask_PoolsVisitedInIDOrder(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run("earlier claim rolled back/"+name, func(t *testing.T) {
			g := seedStepA(t, st, graphOpts{}, store.TaskStatusRunning, store.TaskStatusReady)
			free := seedPoolWithID(t, st, "a-", 5)
			full := seedPoolWithID(t, st, "b-", 1)
			holdClaims(t, st, full, g.Tasks["a"][0])

			res := mustLease(t, st, leaseReq(g.Tasks["a"][1], poolClaim(full), poolClaim(free)), store.LeasePoolFull)
			if res.FullPool != full.Name {
				t.Fatalf("FullPool = %q, want %q", res.FullPool, full.Name)
			}
			assertNothingWritten(t, st, g.Tasks["a"][1], free)
		})
		t.Run("first full pool by ID/"+name, func(t *testing.T) {
			g := seedStepA(t, st, graphOpts{}, store.TaskStatusRunning, store.TaskStatusRunning, store.TaskStatusReady)
			first := seedPoolWithID(t, st, "a-", 1)
			second := seedPoolWithID(t, st, "b-", 1)
			holdClaims(t, st, first, g.Tasks["a"][0])
			holdClaims(t, st, second, g.Tasks["a"][1])

			res := mustLease(t, st, leaseReq(g.Tasks["a"][2], poolClaim(second), poolClaim(first)), store.LeasePoolFull)
			if res.FullPool != first.Name {
				t.Fatalf("FullPool = %q, want the lower-ID pool %q", res.FullPool, first.Name)
			}
			assertNothingWritten(t, st, g.Tasks["a"][2])
		})
	}
}

// TestLeaseTask_SamePoolTwice pins the outcome of a request that names one
// pool twice. SQLite visits the two claims in turn, so the second sees the
// first: it is PoolFull when that fills the pool, and otherwise the unique
// (attempt, pool) index refuses it. Either way nothing is written.
func TestLeaseTask_SamePoolTwice(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run("fills the pool/"+name, func(t *testing.T) {
			g := seedStepA(t, st, graphOpts{}, store.TaskStatusReady)
			pool := seedPool(t, st, 1)
			mustLease(t, st, leaseReq(g.Tasks["a"][0], poolClaim(pool), poolClaim(pool)), store.LeasePoolFull)
			assertNothingWritten(t, st, g.Tasks["a"][0], pool)
		})
		t.Run("room for both/"+name, func(t *testing.T) {
			g := seedStepA(t, st, graphOpts{}, store.TaskStatusReady)
			pool := seedPool(t, st, 0)
			res, err := st.LeaseTask(t.Context(), leaseReq(g.Tasks["a"][0], poolClaim(pool), poolClaim(pool)))
			if !errors.Is(err, store.ErrConflict) {
				t.Fatalf("LeaseTask = (%+v, %v), want ErrConflict", res, err)
			}
			assertNothingWritten(t, st, g.Tasks["a"][0], pool)
		})
	}
}

// TestLeaseTask_MissingFarmIsLost pins the fake against SQLite, whose lease
// joins the farm row: a task whose farm row is gone is Lost there. SQLite
// itself cannot reach this state, because jobs.farm_id and queues.farm_id are
// foreign keys, so DeleteFarm refuses while anything references the farm; the
// fake enforces no foreign keys, so only the fake is exercised.
func TestLeaseTask_MissingFarmIsLost(t *testing.T) {
	st := fake.New()
	g := seedStepA(t, st, graphOpts{}, store.TaskStatusReady)
	if err := st.DeleteFarm(t.Context(), g.Farm.ID); err != nil {
		t.Fatalf("DeleteFarm: %v", err)
	}
	mustLease(t, st, leaseReq(g.Tasks["a"][0]), store.LeaseLost)
	assertNothingWritten(t, st, g.Tasks["a"][0])
}
