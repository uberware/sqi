// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
)

// seedClaimedAttempt seeds task t1 in the given status with a running attempt
// that holds one claim on a fresh pool, and returns the attempt and the claim.
func seedClaimedAttempt(t *testing.T, s *Store, status store.TaskStatus) (store.TaskAttempt, store.UsageClaim) {
	t.Helper()
	ctx := t.Context()
	seedCASTask(t, s, "t1", status)
	now := time.Now().UTC()
	a, err := s.CreateTaskAttempt(ctx, store.TaskAttempt{
		ID: uuid.NewString(), TaskID: "t1", WorkerID: "w", AttemptNumber: 1,
		Status: store.AttemptStatusRunning, StartedAt: now, CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateTaskAttempt: %v", err)
	}
	pool, err := s.CreateUsagePool(ctx, store.UsagePool{ID: uuid.NewString(), Name: uuid.NewString(), MaxConcurrent: 1})
	if err != nil {
		t.Fatalf("CreateUsagePool: %v", err)
	}
	claim, err := s.CreateClaim(ctx, store.UsageClaim{ID: uuid.NewString(), PoolID: pool.ID, TaskAttemptID: a.ID, ClaimedAt: now})
	if err != nil {
		t.Fatalf("CreateClaim: %v", err)
	}
	return a, claim
}

// claimReleasedAt reads a claim's released_at straight from the table; no store
// method exposes it.
func claimReleasedAt(t *testing.T, s *Store, claimID string) time.Time {
	t.Helper()
	var text sql.NullString
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT released_at FROM usage_claims WHERE id = ?`, claimID).Scan(&text); err != nil {
		t.Fatalf("read released_at: %v", err)
	}
	if !text.Valid {
		t.Fatal("claim is still active (released_at is NULL)")
	}
	got, err := textToTime(text.String)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestCompleteTaskAttempt_ClaimReleasedAtUsesServerTime pins that
// AttemptCompletion.EndedAt, which is the worker's clock, feeds only the
// attempt's ended_at. The claim's released_at has always been server time, so a
// skewed worker clock can never put it before the claim's claimed_at. The
// rejected case matters too: the release commits on that path as well.
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
			s := openTestStoreWB(t)
			a, claim := seedClaimedAttempt(t, s, tc.from)
			skewed := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

			res, err := s.CompleteTaskAttempt(t.Context(), store.AttemptCompletion{
				AttemptID: a.ID, TaskID: "t1", TaskStatus: store.TaskStatusSucceeded,
				AttemptStatus: store.AttemptStatusSucceeded, EndedAt: skewed,
			})
			if err != nil || res != tc.want {
				t.Fatalf("CompleteTaskAttempt = (%+v, %v), want %+v", res, err, tc.want)
			}
			got, err := s.GetTaskAttempt(t.Context(), a.ID)
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
	s := openTestStoreWB(t)
	a, claim := seedClaimedAttempt(t, s, store.TaskStatusRunning)
	skewed := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	if _, _, firstClose, err := s.RecordTaskFailure(t.Context(), a.ID, "t1", nil, "", "boom", skewed); err != nil || !firstClose {
		t.Fatalf("RecordTaskFailure = (firstClose %v, %v), want first close", firstClose, err)
	}
	if age := time.Since(claimReleasedAt(t, s, claim.ID)).Abs(); age > 5*time.Second {
		t.Fatalf("claim released_at is %v from now, want server time (the report was an hour old)", age)
	}
}
