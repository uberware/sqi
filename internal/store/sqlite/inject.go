// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// InjectTaskAttempt inserts attempt exactly as given, with no state checks.
//
// Corruption injection for invariant, recovery and repair tests: it exists to
// build states production cannot reach, and must never be used to seed a
// reachable one — seed through CreateJobSubmission and production writes
// instead (internal/store/storetest). It is not part of store.Store. Foreign
// keys still apply: the attempt's task must exist.
func (s *Store) InjectTaskAttempt(ctx context.Context, attempt store.TaskAttempt) (store.TaskAttempt, error) {
	now := timeToText(time.Now().UTC())

	var exitCode sql.NullInt64
	if attempt.ExitCode != nil {
		exitCode = sql.NullInt64{Int64: int64(*attempt.ExitCode), Valid: true}
	}

	row := s.stmtInsertAttempt.QueryRowContext(ctx,
		attempt.ID, attempt.TaskID, attempt.WorkerID,
		nullString(attempt.SessionID), attempt.AttemptNumber, string(attempt.Status),
		exitCode, timeToText(attempt.StartedAt), nullTimeToText(attempt.EndedAt), now, attempt.Message)
	out, err := scanAttempt(row)
	return out, mapErr(err)
}

// InjectClaim inserts an active claim exactly as given, stamped claimed now,
// with no capacity or state checks. Same contract as [Store.InjectTaskAttempt]:
// corruption injection only, not part of store.Store. Foreign keys still
// apply: the claim's pool and attempt must exist.
func (s *Store) InjectClaim(ctx context.Context, claim store.UsageClaim) (store.UsageClaim, error) {
	now := timeToText(time.Now().UTC())
	_, err := s.stmtInsertClaim.ExecContext(ctx,
		claim.ID, claim.PoolID, claim.TaskAttemptID, now)
	if err != nil {
		return store.UsageClaim{}, mapErr(err)
	}
	claim.ClaimedAt = time.Now().UTC()
	claim.ReleasedAt = nil
	return claim, nil
}
