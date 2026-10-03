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
