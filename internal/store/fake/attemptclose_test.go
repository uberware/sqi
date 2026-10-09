// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
)

// seedClaimedAttempt seeds task t1 running, with a running attempt that holds
// one claim, and returns the attempt and the claim's ID. The task is leased
// holding the claim and then started, so the attempt and the claim are the ones
// a real lease makes.
func seedClaimedAttempt(t *testing.T, s *Store) (store.TaskAttempt, string) {
	t.Helper()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	pool := mustCreatePool(t, s, "p1", 0)
	newJob("j1", "farm-f1", "q1").task("t1", "s1", store.TaskStatusReady).submit(t, s)
	a := claimOn(t, s, "t1", "c1", pool)
	storetest.Start(t, s, a)
	return a, "c1"
}

// seedClaimedAttemptOnCanceledTask seeds task t1 already canceled, with a
// running attempt that holds one claim, and returns the attempt and the claim's
// ID.
//
// This state is unreachable through production writes: every cancel closes the
// task's running attempt and releases its claims in the same write, so a
// canceled task never keeps an open attempt. It is what an attempt closing
// after its task was canceled must cope with, so the attempt and the claim are
// injected, which is the point of an injector.
func seedClaimedAttemptOnCanceledTask(t *testing.T, s *Store) (store.TaskAttempt, string) {
	t.Helper()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	newJob("j1", "farm-f1", "q1").task("t1", "s1", store.TaskStatusCanceled).submit(t, s)
	a := storetest.InjectAttempt(t, s, store.TaskAttempt{
		ID: "a1", TaskID: "t1", WorkerID: "w1", AttemptNumber: 1, Status: store.AttemptStatusRunning,
	})
	storetest.InjectClaim(t, s, store.UsageClaim{ID: "c1", PoolID: "p1", TaskAttemptID: a.ID})
	return a, "c1"
}

// claimReleasedAt reads a claim's released_at from the fake's own table; no
// store method exposes it.
func claimReleasedAt(t *testing.T, s *Store, claimID string) time.Time {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.usageClaims[claimID]
	if !ok {
		t.Fatalf("claim %s not found", claimID)
	}
	if c.ReleasedAt == nil {
		t.Fatal("claim is still active (ReleasedAt is nil)")
	}
	return *c.ReleasedAt
}

// TestCompleteTaskAttempt_ClaimReleasedAtUsesServerTime mirrors the SQLite
// test of the same name: AttemptCompletion.EndedAt, the worker's clock, feeds
// only the attempt's ended_at; the claim's released_at is server time. The
// rejected case matters too, because the release commits on that path as well.
func TestCompleteTaskAttempt_ClaimReleasedAtUsesServerTime(t *testing.T) {
	cases := []struct {
		name string
		seed func(t *testing.T, s *Store) (store.TaskAttempt, string)
		want store.CompletionResult
	}{
		{"applied", seedClaimedAttempt, store.CompletionResult{Applied: true}},
		{"rejected", seedClaimedAttemptOnCanceledTask, store.CompletionResult{Rejected: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			defer s.Close()
			a, claimID := tc.seed(t, s)
			skewed := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

			res, err := s.CompleteTaskAttempt(ctx(), store.AttemptCompletion{
				AttemptID: a.ID, TaskID: "t1", TaskStatus: store.TaskStatusSucceeded,
				AttemptStatus: store.AttemptStatusSucceeded, EndedAt: skewed,
			})
			if err != nil || res != tc.want {
				t.Fatalf("CompleteTaskAttempt = (%+v, %v), want %+v", res, err, tc.want)
			}
			got, err := s.GetTaskAttempt(ctx(), a.ID)
			if err != nil {
				t.Fatalf("GetTaskAttempt: %v", err)
			}
			if got.EndedAt == nil || !got.EndedAt.Equal(skewed) {
				t.Fatalf("attempt ended_at = %v, want the supplied %v", got.EndedAt, skewed)
			}
			if age := time.Since(claimReleasedAt(t, s, claimID)).Abs(); age > 5*time.Second {
				t.Fatalf("claim released_at is %v from now, want server time (EndedAt was an hour ago)", age)
			}
		})
	}
}

// TestRecordTaskFailure_ClaimReleasedAtUsesServerTime is the same pin for the
// failure path, whose caller passes the worker-reported time as now.
func TestRecordTaskFailure_ClaimReleasedAtUsesServerTime(t *testing.T) {
	s := New()
	defer s.Close()
	a, claimID := seedClaimedAttempt(t, s)
	skewed := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	if _, _, firstClose, err := s.RecordTaskFailure(ctx(), a.ID, "t1", nil, "", "boom", skewed); err != nil || !firstClose {
		t.Fatalf("RecordTaskFailure = (firstClose %v, %v), want first close", firstClose, err)
	}
	if age := time.Since(claimReleasedAt(t, s, claimID)).Abs(); age > 5*time.Second {
		t.Fatalf("claim released_at is %v from now, want server time (the report was an hour old)", age)
	}
}
