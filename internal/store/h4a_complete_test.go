// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

func completion(task store.Task, a store.TaskAttempt, ts store.TaskStatus, as store.AttemptStatus) store.AttemptCompletion {
	return store.AttemptCompletion{AttemptID: a.ID, TaskID: task.ID, TaskStatus: ts, AttemptStatus: as, EndedAt: time.Now().UTC()}
}

func activeClaims(t *testing.T, st store.Store, poolID string) int {
	t.Helper()
	n, err := st.ActiveClaimCount(t.Context(), poolID)
	if err != nil {
		t.Fatalf("ActiveClaimCount: %v", err)
	}
	return n
}

func mustAttempt(t *testing.T, st store.Store, id string) store.TaskAttempt {
	t.Helper()
	a, err := st.GetTaskAttempt(t.Context(), id)
	if err != nil {
		t.Fatalf("GetTaskAttempt %s: %v", id, err)
	}
	return a
}

// seedRunningTask seeds a ready step holding one task in the given status with
// a running attempt and one active claim on a fresh pool.
func seedRunningTask(t *testing.T, st store.Store, status store.TaskStatus) (store.Task, store.TaskAttempt, store.UsagePool) {
	t.Helper()
	g := seedGraph(t, st, graphOpts{}, stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{status}})
	task := g.Tasks["a"][0]
	a := seedAttempt(t, st, task, store.AttemptStatusRunning)
	pool := seedPool(t, st, 1)
	seedClaim(t, st, pool.ID, a.ID)
	return task, a, pool
}

func TestCompleteTaskAttempt_Success(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			task, a, pool := seedRunningTask(t, st, store.TaskStatusRunning)

			res, err := st.CompleteTaskAttempt(t.Context(), completion(task, a, store.TaskStatusSucceeded, store.AttemptStatusSucceeded))
			if err != nil || !res.Applied || res.Rejected {
				t.Fatalf("CompleteTaskAttempt = (%+v, %v), want applied", res, err)
			}
			if mustTask(t, st, task.ID).Status != store.TaskStatusSucceeded {
				t.Fatal("task not succeeded")
			}
			if got := mustAttempt(t, st, a.ID); got.Status != store.AttemptStatusSucceeded || got.EndedAt == nil {
				t.Fatalf("attempt = %+v, want succeeded with ended_at", got)
			}
			if n := activeClaims(t, st, pool.ID); n != 0 {
				t.Fatalf("active claims = %d, want 0", n)
			}
			if v := claimViolations(t, st); len(v) != 0 {
				t.Fatalf("I3 violations: %v", v)
			}
		})
	}
}

// TestCompleteTaskAttempt_AssignedToTerminal pins that a terminal report landing
// on a task still in assigned (the worker's running publish was dropped) is
// applied, as the state machine allows.
func TestCompleteTaskAttempt_AssignedToTerminal(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			task, a, pool := seedRunningTask(t, st, store.TaskStatusAssigned)

			res, err := st.CompleteTaskAttempt(t.Context(), completion(task, a, store.TaskStatusSucceeded, store.AttemptStatusSucceeded))
			if err != nil || !res.Applied || res.Rejected {
				t.Fatalf("CompleteTaskAttempt = (%+v, %v), want applied", res, err)
			}
			if mustTask(t, st, task.ID).Status != store.TaskStatusSucceeded {
				t.Fatal("task not succeeded")
			}
			if n := activeClaims(t, st, pool.ID); n != 0 {
				t.Fatalf("active claims = %d, want 0", n)
			}
		})
	}
}

// TestCompleteTaskAttempt_RecordsAttemptDetails pins that the attempt's exit
// code, session and message are written, and that the failure reason is
// stamped on the task when it ends up holding the requested status.
func TestCompleteTaskAttempt_RecordsAttemptDetails(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			task, a, _ := seedRunningTask(t, st, store.TaskStatusRunning)
			code := 3
			c := completion(task, a, store.TaskStatusFailed, store.AttemptStatusFailed)
			c.ExitCode, c.SessionID, c.Message, c.FailureReason = &code, "sess-1", "it broke", "worker reported failure"

			res, err := st.CompleteTaskAttempt(t.Context(), c)
			if err != nil || !res.Applied || res.Rejected {
				t.Fatalf("CompleteTaskAttempt = (%+v, %v), want applied", res, err)
			}
			got := mustAttempt(t, st, a.ID)
			if got.ExitCode == nil || *got.ExitCode != 3 || got.SessionID != "sess-1" || got.Message != "it broke" {
				t.Fatalf("attempt = %+v, want exit 3, session sess-1, message %q", got, "it broke")
			}
			gotTask := mustTask(t, st, task.ID)
			if gotTask.Status != store.TaskStatusFailed || gotTask.FailureReason != "worker reported failure" {
				t.Fatalf("task = (%s, %q), want failed with the reason", gotTask.Status, gotTask.FailureReason)
			}
		})
	}
}

// TestCompleteTaskAttempt_TaskRowUsesServerTime pins that EndedAt, which comes
// from the worker's clock, is the attempt's end time only: the task row's
// updated_at (what TaskSortByUpdatedAt orders by) is stamped with server time,
// as UpdateTaskStatus does, so worker clock skew cannot reorder tasks. The
// released claim's released_at is server time too; neither backend exposes a
// claim read, so that is pinned by TestCompleteTaskAttempt_ClaimReleasedAtUsesServerTime
// in each backend's own package (internal/store/sqlite and internal/store/fake).
func TestCompleteTaskAttempt_TaskRowUsesServerTime(t *testing.T) {
	cases := []struct {
		name   string
		from   store.TaskStatus
		to     store.TaskStatus
		as     store.AttemptStatus
		reason string
	}{
		{"status write", store.TaskStatusRunning, store.TaskStatusSucceeded, store.AttemptStatusSucceeded, ""},
		{"status write and failure reason", store.TaskStatusRunning, store.TaskStatusFailed, store.AttemptStatusFailed, "boom"},
		{"failure reason on a task already there", store.TaskStatusFailed, store.TaskStatusFailed, store.AttemptStatusFailed, "boom"},
	}
	for _, tc := range cases {
		for name, st := range newStores(t) {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				task, a, _ := seedRunningTask(t, st, tc.from)
				skewed := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
				c := completion(task, a, tc.to, tc.as)
				c.EndedAt, c.FailureReason = skewed, tc.reason

				res, err := st.CompleteTaskAttempt(t.Context(), c)
				if err != nil || !res.Applied {
					t.Fatalf("CompleteTaskAttempt = (%+v, %v), want applied", res, err)
				}
				if got := mustAttempt(t, st, a.ID); got.EndedAt == nil || !got.EndedAt.Equal(skewed) {
					t.Fatalf("attempt ended_at = %v, want the supplied %v", got.EndedAt, skewed)
				}
				if age := time.Since(mustTask(t, st, task.ID).UpdatedAt).Abs(); age > 5*time.Second {
					t.Fatalf("task updated_at is %v old, want server time within a few seconds (EndedAt was an hour ago)", age)
				}
			})
		}
	}
}

// TestCompleteTaskAttempt_RejectedStillReleases pins F3: the task was canceled
// while running; the worker's terminal report is rejected but the claim must
// still be released and the attempt closed.
func TestCompleteTaskAttempt_RejectedStillReleases(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			task, a, pool := seedRunningTask(t, st, store.TaskStatusCanceled)

			c := completion(task, a, store.TaskStatusSucceeded, store.AttemptStatusSucceeded)
			c.FailureReason = "must not be stamped on a rejected report"
			res, err := st.CompleteTaskAttempt(t.Context(), c)
			if err != nil || res.Applied || !res.Rejected {
				t.Fatalf("CompleteTaskAttempt = (%+v, %v), want rejected", res, err)
			}
			got := mustTask(t, st, task.ID)
			if got.Status != store.TaskStatusCanceled {
				t.Fatal("canceled task was overwritten")
			}
			if got.FailureReason != "" {
				t.Fatalf("failure reason = %q, want it left alone on a rejected report", got.FailureReason)
			}
			if n := activeClaims(t, st, pool.ID); n != 0 {
				t.Fatalf("active claims = %d, want 0 (F3)", n)
			}
			if got := mustAttempt(t, st, a.ID); got.Status != store.AttemptStatusSucceeded || got.EndedAt == nil {
				t.Fatalf("attempt = %+v, want it closed even though the task transition was rejected", got)
			}
			if v := claimViolations(t, st); len(v) != 0 {
				t.Fatalf("I3 violations: %v", v)
			}
		})
	}
}

// TestCompleteTaskAttempt_Redelivery pins Review Focus #1.
func TestCompleteTaskAttempt_Redelivery(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			task, a, pool := seedRunningTask(t, st, store.TaskStatusRunning)
			code := 0
			c := completion(task, a, store.TaskStatusSucceeded, store.AttemptStatusSucceeded)
			c.ExitCode = &code
			if _, err := st.CompleteTaskAttempt(t.Context(), c); err != nil {
				t.Fatalf("first: %v", err)
			}
			first := mustAttempt(t, st, a.ID)

			// The redelivery carries different attempt details: none of them may
			// overwrite what the first delivery wrote.
			other := 9
			again := c
			again.ExitCode, again.Message, again.EndedAt = &other, "late echo", c.EndedAt.Add(time.Minute)
			res, err := st.CompleteTaskAttempt(t.Context(), again)
			if err != nil || !res.Applied || res.Rejected {
				t.Fatalf("redelivery = (%+v, %v), want applied (same status is a no-op, not a rejection)", res, err)
			}

			got := mustAttempt(t, st, a.ID)
			if got.ExitCode == nil || *got.ExitCode != 0 || got.Message != first.Message ||
				got.EndedAt == nil || first.EndedAt == nil || !got.EndedAt.Equal(*first.EndedAt) {
				t.Fatalf("redelivery rewrote the closed attempt: first %+v, now %+v", first, got)
			}
			if mustTask(t, st, task.ID).Status != store.TaskStatusSucceeded {
				t.Fatal("task status changed on redelivery")
			}
			if n := activeClaims(t, st, pool.ID); n != 0 {
				t.Fatalf("active claims = %d, want 0", n)
			}
			if v := claimViolations(t, st); len(v) != 0 {
				t.Fatalf("I3 violations: %v", v)
			}
		})
	}
}

// supersededWorker is the worker a superseded task is leased to again.
const supersededWorker = "w2"

// seedSupersededAttempt seeds the shape of a superseded attempt through the
// store's own operations: a task assigned to the fixture worker with a running
// attempt (#1) holding a claim; the reaper takes it back (the attempt closed
// as failed, its claim released, the task ready); then a new lease hands it to
// supersededWorker (attempt #2, running, holding a claim on the same pool).
// When taskStatus is running the new worker has also reported running. It
// returns the task, the superseded attempt, the new attempt and the pool.
func seedSupersededAttempt(t *testing.T, st store.Store, taskStatus store.TaskStatus) (store.Task, store.TaskAttempt, store.TaskAttempt, store.UsagePool) {
	t.Helper()
	task, old, pool := seedRunningTask(t, st, store.TaskStatusAssigned)
	if got, err := st.ReclaimStaleAssignedTasks(t.Context(), time.Now().UTC().Add(time.Minute)); err != nil || len(got) != 1 {
		t.Fatalf("ReclaimStaleAssignedTasks = (%+v, %v), want the one assigned task", got, err)
	}
	req := leaseReq(task, poolClaim(pool))
	req.WorkerID = supersededWorker
	fresh := mustLease(t, st, req, store.LeaseLeased).Attempt
	if taskStatus == store.TaskStatusRunning {
		if err := fixtures(t, st).UpdateTaskStatus(t.Context(), task.ID, store.TaskStatusRunning); err != nil {
			t.Fatalf("UpdateTaskStatus running: %v", err)
		}
	}
	return task, old, fresh, pool
}

// TestCompleteTaskAttempt_SupersededAttemptIsRejected pins the report path's
// half of I3: a worker's late terminal report for an attempt that the reaper
// closed and a new lease replaced must not end the task while the new attempt
// is open and holds its claims. The arrow alone (assigned or running to
// succeeded, failed or canceled) is legal, so only the latest-attempt check
// stops it. The report is Rejected, so the scheduler acks it, and the task,
// its failure reason, the new attempt and its claim are left as they were.
// The new attempt's own report still applies, and its redelivery is a no-op.
func TestCompleteTaskAttempt_SupersededAttemptIsRejected(t *testing.T) {
	cases := []struct {
		name    string
		current store.TaskStatus // the re-leased task's status when the late report lands
		report  store.TaskStatus
		attempt store.AttemptStatus
	}{
		{"succeeded on an assigned task", store.TaskStatusAssigned, store.TaskStatusSucceeded, store.AttemptStatusSucceeded},
		{"succeeded on a running task", store.TaskStatusRunning, store.TaskStatusSucceeded, store.AttemptStatusSucceeded},
		{"canceled echo", store.TaskStatusRunning, store.TaskStatusCanceled, store.AttemptStatusCanceled},
		{"failed", store.TaskStatusRunning, store.TaskStatusFailed, store.AttemptStatusFailed},
	}
	for _, tc := range cases {
		for name, st := range newStores(t) {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				task, old, fresh, pool := seedSupersededAttempt(t, st, tc.current)
				before := mustTask(t, st, task.ID)

				late := completion(task, old, tc.report, tc.attempt)
				late.FailureReason = "must not be stamped by a superseded report"
				res, err := st.CompleteTaskAttempt(t.Context(), late)
				if err != nil || res.Applied || !res.Rejected {
					t.Fatalf("late report = (%+v, %v), want rejected with no error", res, err)
				}
				got := mustTask(t, st, task.ID)
				if got.Status != tc.current || got.AssignedWorkerID != supersededWorker || got.FailureReason != "" ||
					!got.UpdatedAt.Equal(before.UpdatedAt) {
					t.Fatalf("task = (%s on %q, reason %q, updated %v), want it untouched: %s on %q, no reason, updated %v",
						got.Status, got.AssignedWorkerID, got.FailureReason, got.UpdatedAt, tc.current, supersededWorker, before.UpdatedAt)
				}
				if a := mustAttempt(t, st, fresh.ID); a.Status != store.AttemptStatusRunning || a.EndedAt != nil {
					t.Fatalf("new attempt = %q (ended %v), want running and open", a.Status, a.EndedAt)
				}
				if a := mustAttempt(t, st, old.ID); a.Status != store.AttemptStatusFailed {
					t.Fatalf("superseded attempt = %q, want the reaper's failed close left as it was", a.Status)
				}
				if n := activeClaims(t, st, pool.ID); n != 1 {
					t.Fatalf("active claims = %d, want 1 (the new attempt's)", n)
				}
				if v := claimViolations(t, st); len(v) != 0 {
					t.Fatalf("I3 violations: %v", v)
				}

				// The latest attempt's report applies, and redelivering it is a no-op.
				current := completion(task, fresh, tc.report, tc.attempt)
				for i := range 2 {
					res, err := st.CompleteTaskAttempt(t.Context(), current)
					if err != nil || !res.Applied || res.Rejected {
						t.Fatalf("latest attempt's report, delivery %d = (%+v, %v), want applied", i+1, res, err)
					}
					if got := mustTask(t, st, task.ID); got.Status != tc.report {
						t.Fatalf("delivery %d: task = %q, want %q", i+1, got.Status, tc.report)
					}
					if n := activeClaims(t, st, pool.ID); n != 0 {
						t.Fatalf("delivery %d: active claims = %d, want 0", i+1, n)
					}
					if v := claimViolations(t, st); len(v) != 0 {
						t.Fatalf("delivery %d: I3 violations: %v", i+1, v)
					}
				}
			})
		}
	}
}

// TestCompleteTaskAttempt_SupersededOpenAttemptIsClosed pins the order the
// latest-attempt check runs in: after the reporting attempt is closed and its
// claims released, as on every path, so only the task move is refused. The
// shape, an older attempt still open beside the latest one, cannot come from
// H4a's operations (each closes a task's attempts whenever it takes the task
// back), but it is what a v0.3.0 reaper race (F5) could leave behind, and the
// old worker's report must release that attempt's slot rather than keep it.
func TestCompleteTaskAttempt_SupersededOpenAttemptIsClosed(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{}, stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusRunning}})
			task := g.Tasks["a"][0]
			pool := seedPool(t, st, 2)
			old := seedAttempt(t, st, task, store.AttemptStatusRunning)
			seedClaim(t, st, pool.ID, old.ID)
			fresh := seedAttempt(t, st, task, store.AttemptStatusRunning)
			seedClaim(t, st, pool.ID, fresh.ID)

			res, err := st.CompleteTaskAttempt(t.Context(), completion(task, old, store.TaskStatusSucceeded, store.AttemptStatusSucceeded))
			if err != nil || res.Applied || !res.Rejected {
				t.Fatalf("CompleteTaskAttempt = (%+v, %v), want rejected with no error", res, err)
			}
			if got := mustTask(t, st, task.ID); got.Status != store.TaskStatusRunning {
				t.Fatalf("task = %q, want running (the latest attempt still holds it)", got.Status)
			}
			if a := mustAttempt(t, st, old.ID); a.Status != store.AttemptStatusSucceeded || a.EndedAt == nil {
				t.Fatalf("reporting attempt = %q (ended %v), want closed as reported", a.Status, a.EndedAt)
			}
			if a := mustAttempt(t, st, fresh.ID); a.Status != store.AttemptStatusRunning || a.EndedAt != nil {
				t.Fatalf("latest attempt = %q (ended %v), want running and open", a.Status, a.EndedAt)
			}
			if n := activeClaims(t, st, pool.ID); n != 1 {
				t.Fatalf("active claims = %d, want 1 (only the latest attempt's)", n)
			}
			if v := claimViolations(t, st); len(v) != 0 {
				t.Fatalf("I3 violations: %v", v)
			}
		})
	}
}

func TestCompleteTaskAttempt_UnknownTask(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			task, a, pool := seedRunningTask(t, st, store.TaskStatusRunning)
			c := completion(task, a, store.TaskStatusSucceeded, store.AttemptStatusSucceeded)
			c.TaskID = "no-such-task"

			_, err := st.CompleteTaskAttempt(t.Context(), c)
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("CompleteTaskAttempt(unknown task) = %v, want ErrNotFound", err)
			}
			if got := mustAttempt(t, st, a.ID); got.Status != store.AttemptStatusRunning {
				t.Fatalf("attempt = %s, want untouched (running)", got.Status)
			}
			if n := activeClaims(t, st, pool.ID); n != 1 {
				t.Fatalf("active claims = %d, want 1 (nothing written)", n)
			}
		})
	}
}

// TestRecordTaskFailure_ReleasesClaims pins I3 on the retry path.
func TestRecordTaskFailure_ReleasesClaims(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			task, a, pool := seedRunningTask(t, st, store.TaskStatusRunning)
			taskFailed, _, firstClose, err := st.RecordTaskFailure(t.Context(), a.ID, task.ID, nil, "", "boom", time.Now().UTC())
			if err != nil {
				t.Fatalf("RecordTaskFailure: %v", err)
			}
			if !firstClose || taskFailed != 1 {
				t.Fatalf("RecordTaskFailure = (taskFailed %d, firstClose %v), want (1, true)", taskFailed, firstClose)
			}
			if n := activeClaims(t, st, pool.ID); n != 0 {
				t.Fatalf("active claims = %d, want 0", n)
			}
			if v := claimViolations(t, st); len(v) != 0 {
				t.Fatalf("I3 violations: %v", v)
			}

			// A redelivery neither re-counts nor errors.
			taskFailed, _, firstClose, err = st.RecordTaskFailure(t.Context(), a.ID, task.ID, nil, "", "boom", time.Now().UTC())
			if err != nil || firstClose || taskFailed != 1 {
				t.Fatalf("redelivery = (taskFailed %d, firstClose %v, %v), want (1, false, nil)", taskFailed, firstClose, err)
			}
		})
	}
}

func TestRecordTaskFailure_UnknownTask(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			_, a, pool := seedRunningTask(t, st, store.TaskStatusRunning)
			_, _, _, err := st.RecordTaskFailure(t.Context(), a.ID, "no-such-task", nil, "", "boom", time.Now().UTC())
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("RecordTaskFailure(unknown task) = %v, want ErrNotFound", err)
			}
			if got := mustAttempt(t, st, a.ID); got.Status != store.AttemptStatusRunning {
				t.Fatalf("attempt = %s, want untouched (running)", got.Status)
			}
			if n := activeClaims(t, st, pool.ID); n != 1 {
				t.Fatalf("active claims = %d, want 1 (nothing written)", n)
			}
		})
	}
}
