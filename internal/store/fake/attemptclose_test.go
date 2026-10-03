// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// seedClaimedAttempt seeds task t1 in the given status with a running attempt
// that holds one claim, and returns the attempt and the claim.
func seedClaimedAttempt(t *testing.T, s *Store, status store.TaskStatus) (store.TaskAttempt, store.UsageClaim) {
	t.Helper()
	mustCreateJob(t, s, "j1", "f1", "q1")
	mustCreateTask(t, s, "t1", "j1", "s1", status)
	a := mustCreateAttempt(t, s, "a1", "t1", 1, store.AttemptStatusRunning)
	claim := mustCreateClaim(t, s, store.UsageClaim{ID: "c1", PoolID: "p1", TaskAttemptID: a.ID, ClaimedAt: time.Now().UTC()})
	return a, claim
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
		from store.TaskStatus
		want store.CompletionResult
	}{
		{"applied", store.TaskStatusRunning, store.CompletionResult{Applied: true}},
		{"rejected", store.TaskStatusCanceled, store.CompletionResult{Rejected: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			defer s.Close()
			a, claim := seedClaimedAttempt(t, s, tc.from)
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
			if age := time.Since(claimReleasedAt(t, s, claim.ID)).Abs(); age > 5*time.Second {
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
	a, claim := seedClaimedAttempt(t, s, store.TaskStatusRunning)
	skewed := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	if _, _, firstClose, err := s.RecordTaskFailure(ctx(), a.ID, "t1", nil, "", "boom", skewed); err != nil || !firstClose {
		t.Fatalf("RecordTaskFailure = (firstClose %v, %v), want first close", firstClose, err)
	}
	if age := time.Since(claimReleasedAt(t, s, claim.ID)).Abs(); age > 5*time.Second {
		t.Fatalf("claim released_at is %v from now, want server time (the report was an hour old)", age)
	}
}
