// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/uberware/sqi/internal/store"
)

const (
	// sqlSelectTaskAndAttemptLive reads a task's status and whether one of its
	// attempts is still running and is the task's latest (sqlIsLatestAttempt's
	// rule) in one statement. No row means the task does not exist; a NULL live
	// column means the attempt does not exist or belongs to another task. Binds:
	// attempt id, task id.
	sqlSelectTaskAndAttemptLive = `
SELECT t.status,
       a.status = 'running'
   AND a.attempt_number = (SELECT MAX(attempt_number) FROM task_attempts WHERE task_id = a.task_id)
FROM   tasks t
LEFT JOIN task_attempts a ON a.id = ? AND a.task_id = t.id
WHERE  t.id = ?`

	// sqlSetRunningAttemptSession records a session ID on an attempt only while
	// it is open. Binds: session_id, id.
	sqlSetRunningAttemptSession = `
UPDATE task_attempts SET session_id = ? WHERE id = ? AND status = 'running'`
)

// liveAttemptTx reports whether a worker report from attemptID may act on
// taskID: the attempt is still running and is the task's latest attempt, and
// the task is in flight (assigned or running). It also returns the task's
// current status. sql.ErrNoRows on the task maps to store.ErrNotFound.
func liveAttemptTx(ctx context.Context, tx *dbTx, attemptID, taskID string) (bool, store.TaskStatus, error) {
	var current string
	var live sql.NullBool
	if err := tx.QueryRowContext(ctx, sqlSelectTaskAndAttemptLive, attemptID, taskID).Scan(&current, &live); err != nil {
		return false, "", fmt.Errorf("store: attempt %s of task %s: %w", attemptID, taskID, mapErr(err))
	}
	cur := store.TaskStatus(current)
	if cur != store.TaskStatusAssigned && cur != store.TaskStatusRunning {
		return false, cur, nil
	}
	return live.Valid && live.Bool, cur, nil
}

// lockTaskJobTx takes the task's job-row anchor inside tx.
func lockTaskJobTx(ctx context.Context, tx *dbTx, taskID string) error {
	var jobID string
	if err := tx.QueryRowContext(ctx, sqlTaskJobID, taskID).Scan(&jobID); err != nil {
		return mapErr(err)
	}
	return tx.lockAnchors(ctx, jobAnchor(jobID))
}

// StartTaskAttempt implements [store.TaskStore].
func (s *Store) StartTaskAttempt(ctx context.Context, attemptID, taskID, sessionID string, now time.Time) (bool, error) {
	return inTxValue(ctx, s, "start task attempt", func(tx *dbTx) (bool, txOutcome, error) {
		if err := lockTaskJobTx(ctx, tx, taskID); err != nil {
			return false, txRollback, err
		}
		live, current, err := liveAttemptTx(ctx, tx, attemptID, taskID)
		if err != nil {
			return false, txRollback, err
		}
		if !live {
			return false, txCommit, nil
		}
		if current == store.TaskStatusAssigned {
			// On SQLite, under the job anchor and the single write
			// connection, the guarded write cannot miss. It is still
			// consulted for a store where it can (PostgreSQL): a miss means
			// another writer moved the task after the read above, and the
			// report is then stale.
			moved, err := casWriteTaskStatus(ctx, tx, taskID, store.TaskStatusAssigned, store.TaskStatusRunning, now)
			if err != nil {
				return false, txRollback, err
			}
			if !moved {
				return false, txRollback, nil // the rollback discards nothing: no write landed
			}
		}
		if sessionID != "" {
			if _, err := tx.ExecContext(ctx, sqlSetRunningAttemptSession, sessionID, attemptID); err != nil {
				return false, txRollback, fmt.Errorf("store: record session of attempt %s: %w", attemptID, mapErr(err))
			}
		}
		return true, txCommit, nil
	})
}

// sqlRecordAttemptProgress overwrites an open attempt's progress snapshot
// unless a snapshot at or after the new one is stored. Times are fixed-width
// text, so the comparison orders them. Binds: progress, status_text,
// progress_at, id, progress_at.
const sqlRecordAttemptProgress = `
UPDATE task_attempts SET progress = ?, status_text = ?, progress_at = ?
WHERE  id = ? AND status = 'running' AND (progress_at IS NULL OR progress_at < ?)`

// RecordAttemptProgress implements [store.TaskStore].
//
// No job-row anchor is taken: the liveness read and the guarded update touch
// only the task and attempt rows, and a snapshot that races a close is
// harmless because the update re-checks status = 'running'.
func (s *Store) RecordAttemptProgress(ctx context.Context, p store.AttemptProgress) (store.ProgressOutcome, error) {
	return inTxValue(ctx, s, "record attempt progress", func(tx *dbTx) (store.ProgressOutcome, txOutcome, error) {
		live, current, err := liveAttemptTx(ctx, tx, p.AttemptID, p.TaskID)
		if err != nil {
			return store.ProgressNotLive, txRollback, err
		}
		if !live {
			return store.ProgressNotLive, txCommit, nil
		}
		if current == store.TaskStatusAssigned {
			return store.ProgressTaskAssigned, txCommit, nil
		}
		var progress any
		if p.Progress != nil {
			progress = int64(*p.Progress)
		}
		at := TimeText(p.At.UTC())
		res, err := tx.ExecContext(ctx, sqlRecordAttemptProgress, progress, p.StatusText, at, p.AttemptID, at)
		if err != nil {
			return store.ProgressNotLive, txRollback, fmt.Errorf("store: record progress of attempt %s: %w", p.AttemptID, mapErr(err))
		}
		n, err := res.RowsAffected()
		if err != nil {
			return store.ProgressNotLive, txRollback, fmt.Errorf("store: record progress of attempt %s: %w", p.AttemptID, mapErr(err))
		}
		if n == 0 {
			return store.ProgressOutOfOrder, txCommit, nil
		}
		return store.ProgressRecorded, txCommit, nil
	})
}

// sqlReclaimOneTask returns one in-flight task to ready. Binds: updated_at, id.
const sqlReclaimOneTask = sqlReclaimTaskSet + `
WHERE  id = ? AND status IN ('assigned', 'running')`

// ReclaimTaskAttempt implements [store.TaskStore].
//
// Anchor and statement order are offlineWorker's for one task: the job row
// (lockTaskJobTx), then the task, then its attempts and claims
// (closeTaskAttemptsTx). No worker anchor is taken, because the worker row is
// neither read nor written here. The task is written before its attempt is
// closed for offlineWorker's reason. As for StartTaskAttempt, no task-row
// lock is needed for the latest-attempt check: a lease holds the job row FOR
// SHARE, so none commits between the check and the write.
func (s *Store) ReclaimTaskAttempt(ctx context.Context, attemptID, taskID string, now time.Time) (bool, error) {
	return inTxValue(ctx, s, "reclaim task attempt", func(tx *dbTx) (bool, txOutcome, error) {
		if err := lockTaskJobTx(ctx, tx, taskID); err != nil {
			return false, txRollback, err
		}
		live, _, err := liveAttemptTx(ctx, tx, attemptID, taskID)
		if err != nil {
			return false, txRollback, err
		}
		if !live {
			return false, txCommit, nil
		}
		nowText := TimeText(now.UTC())
		if _, err := tx.ExecContext(ctx, sqlReclaimOneTask, nowText, taskID); err != nil {
			return false, txRollback, fmt.Errorf("store: reclaim task %s: %w", taskID, mapErr(err))
		}
		if err := closeTaskAttemptsTx(ctx, tx, taskID, store.AttemptStatusFailed, store.FailureReasonWorkerShutdown, nowText); err != nil {
			return false, txRollback, err
		}
		return true, txCommit, nil
	})
}
