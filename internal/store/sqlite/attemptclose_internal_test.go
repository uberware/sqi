// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
)

// seedClaimedAttempt seeds task t1 in the given status with a running attempt
// that holds one claim on a fresh pool, and returns the attempt and the claim.
//
// A running task is leased holding the claim, the way production makes one. A
// canceled task cannot be built that way: a cancel closes the task's attempts
// and releases their claims in the same write, so a running attempt and an
// active claim behind a canceled task is a state production never produces. It
// is what a report that arrives after the cancel is written against, so that
// case injects the attempt and the claim.
func seedClaimedAttempt(t *testing.T, s *Store, status store.TaskStatus) (store.TaskAttempt, store.UsageClaim) {
	t.Helper()
	ctx := t.Context()
	pool, err := s.CreateUsagePool(ctx, store.UsagePool{ID: uuid.NewString(), Name: uuid.NewString(), MaxConcurrent: 1})
	if err != nil {
		t.Fatalf("CreateUsagePool: %v", err)
	}
	claimID := uuid.NewString()

	if status == store.TaskStatusRunning {
		// Submit the task ready, then lease it here so the claim rides on the
		// lease: seedCASTask's own lease holds no claim.
		seedCASTask(t, s, "t1", store.TaskStatusReady)
		a := storetest.Running(t, s, store.LeaseRequest{
			TaskID: "t1", WorkerID: "w",
			Claims: []store.UsagePoolClaim{{ClaimID: claimID, PoolID: pool.ID, PoolName: pool.Name}},
		})
		return a, store.UsageClaim{ID: claimID, PoolID: pool.ID, TaskAttemptID: a.ID}
	}

	seedCASTask(t, s, "t1", status)
	a := storetest.InjectAttempt(t, s, store.TaskAttempt{
		TaskID: "t1", WorkerID: "w", AttemptNumber: 1, Status: store.AttemptStatusRunning,
	})
	claim := storetest.InjectClaim(t, s, store.UsageClaim{ID: claimID, PoolID: pool.ID, TaskAttemptID: a.ID})
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
