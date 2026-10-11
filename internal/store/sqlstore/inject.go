// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

import (
	"context"
	"database/sql"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// InjectTaskAttempt inserts attempt as given, with no state checks; a zero
// CreatedAt is stamped now.
//
// Corruption injection for invariant, recovery and repair tests: it exists to
// build states production cannot reach, and must never be used to seed a
// reachable one — seed through CreateJobSubmission and production writes
// instead (internal/store/storetest). It is not part of store.Store. Foreign
// keys still apply: the attempt's task must exist.
func (s *Store) InjectTaskAttempt(ctx context.Context, attempt store.TaskAttempt) (store.TaskAttempt, error) {
	if attempt.CreatedAt.IsZero() {
		attempt.CreatedAt = time.Now().UTC()
	}
	var exitCode sql.NullInt64
	if attempt.ExitCode != nil {
		exitCode = sql.NullInt64{Int64: int64(*attempt.ExitCode), Valid: true}
	}
	row := s.w.QueryRowContext(ctx, sqlInsertAttempt,
		attempt.ID, attempt.TaskID, attempt.WorkerID,
		nullString(attempt.SessionID), attempt.AttemptNumber, string(attempt.Status),
		exitCode, TimeText(attempt.StartedAt), nullTimeToText(attempt.EndedAt),
		TimeText(attempt.CreatedAt), attempt.Message)
	out, err := scanAttempt(row)
	return out, mapErr(err)
}

// InjectClaim inserts claim active, with no capacity or state checks; a zero
// ClaimedAt is stamped now and ReleasedAt is ignored. Same contract as
// [Store.InjectTaskAttempt]: corruption injection only, not part of
// store.Store. Foreign keys still apply: the claim's pool and attempt must
// exist.
func (s *Store) InjectClaim(ctx context.Context, claim store.UsageClaim) (store.UsageClaim, error) {
	if claim.ClaimedAt.IsZero() {
		claim.ClaimedAt = time.Now().UTC()
	}
	claim.ReleasedAt = nil
	if _, err := s.w.ExecContext(ctx, sqlInsertClaim,
		claim.ID, claim.PoolID, claim.TaskAttemptID, TimeText(claim.ClaimedAt)); err != nil {
		return store.UsageClaim{}, mapErr(err)
	}
	return claim, nil
}
