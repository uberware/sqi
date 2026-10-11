// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uberware/sqi/internal/store"
)

const (
	// sqlSelectAttemptLive reports whether an attempt of the task is still
	// running and is the task's latest attempt (sqlIsLatestAttempt's rule). No
	// row means the attempt does not exist or belongs to another task. Binds:
	// attempt id, task id.
	sqlSelectAttemptLive = `
SELECT a.status = 'running'
   AND a.attempt_number = (SELECT MAX(attempt_number) FROM task_attempts WHERE task_id = a.task_id)
FROM   task_attempts a
WHERE  a.id = ? AND a.task_id = ?`

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
	if err := tx.QueryRowContext(ctx, sqlSelectTaskStatus, taskID).Scan(&current); err != nil {
		return false, "", mapErr(err)
	}
	cur := store.TaskStatus(current)
	if cur != store.TaskStatusAssigned && cur != store.TaskStatusRunning {
		return false, cur, nil
	}
	var live bool
	err := tx.QueryRowContext(ctx, sqlSelectAttemptLive, attemptID, taskID).Scan(&live)
	if errors.Is(err, sql.ErrNoRows) {
		return false, cur, nil
	}
	if err != nil {
		return false, "", fmt.Errorf("store: attempt %s of task %s: %w", attemptID, taskID, mapErr(err))
	}
	return live, cur, nil
}

// lockTaskJobTx takes the task's job-row anchor inside tx.
func lockTaskJobTx(ctx context.Context, tx *dbTx, taskID string) error {
	var jobID string
	if err := tx.QueryRowContext(ctx, sqlTaskJobID, taskID).Scan(&jobID); err != nil {
		return mapErr(err)
	}
	return tx.d.LockAnchors(ctx, tx.tx, jobAnchor(jobID))
}

// StartTaskAttempt implements [store.TaskStore].
func (s *Store) StartTaskAttempt(ctx context.Context, attemptID, taskID, sessionID string, now time.Time) (bool, error) {
	var started bool
	err := s.inTx(ctx, "start task attempt", func(tx *dbTx) (txOutcome, error) {
		started = false
		if err := lockTaskJobTx(ctx, tx, taskID); err != nil {
			return txRollback, err
		}
		live, current, err := liveAttemptTx(ctx, tx, attemptID, taskID)
		if err != nil {
			return txRollback, err
		}
		if !live {
			return txCommit, nil
		}
		if current == store.TaskStatusAssigned {
			// Under the job anchor and the single write connection the guarded
			// write cannot miss here. It is still consulted, so the method stays
			// correct on a store where it can (PostgreSQL): a miss means another
			// writer moved the task after the read above, and the report is then
			// stale.
			moved, err := casWriteTaskStatus(ctx, tx, taskID, store.TaskStatusAssigned, store.TaskStatusRunning, now)
			if err != nil {
				return txRollback, err
			}
			if !moved {
				return txRollback, nil // the rollback discards nothing: no write landed
			}
		}
		if sessionID != "" {
			if _, err := tx.ExecContext(ctx, sqlSetRunningAttemptSession, sessionID, attemptID); err != nil {
				return txRollback, fmt.Errorf("store: record session of attempt %s: %w", attemptID, mapErr(err))
			}
		}
		started = true
		return txCommit, nil
	})
	if err != nil {
		return false, err
	}
	return started, nil
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
// closed for offlineWorker's reason. On Postgres the task row must be
// locked FOR UPDATE after the anchor and before the latest-attempt check, as
// for StartTaskAttempt.
func (s *Store) ReclaimTaskAttempt(ctx context.Context, attemptID, taskID string, now time.Time) (bool, error) {
	var reclaimed bool
	err := s.inTx(ctx, "reclaim task attempt", func(tx *dbTx) (txOutcome, error) {
		reclaimed = false
		if err := lockTaskJobTx(ctx, tx, taskID); err != nil {
			return txRollback, err
		}
		live, _, err := liveAttemptTx(ctx, tx, attemptID, taskID)
		if err != nil {
			return txRollback, err
		}
		if !live {
			return txCommit, nil
		}
		nowText := TimeText(now.UTC())
		if _, err := tx.ExecContext(ctx, sqlReclaimOneTask, nowText, taskID); err != nil {
			return txRollback, fmt.Errorf("store: reclaim task %s: %w", taskID, mapErr(err))
		}
		if err := closeTaskAttemptsTx(ctx, tx, taskID, store.AttemptStatusFailed, store.FailureReasonWorkerShutdown, nowText); err != nil {
			return txRollback, err
		}
		reclaimed = true
		return txCommit, nil
	})
	if err != nil {
		return false, err
	}
	return reclaimed, nil
}
