// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uberware/sqi/internal/store"
)

const (
	sqlSelectAttemptStatus = `SELECT status FROM task_attempts WHERE id = ? AND task_id = ?`

	// sqlSetRunningAttemptSession records a session ID on an attempt only while
	// it is open. Binds: session_id, id.
	sqlSetRunningAttemptSession = `
UPDATE task_attempts SET session_id = ? WHERE id = ? AND status = 'running'`
)

// liveAttemptTx reports whether attemptID is still running and is taskID's
// latest attempt, and returns the task's current status. sql.ErrNoRows on the
// task maps to store.ErrNotFound.
func liveAttemptTx(ctx context.Context, tx *sql.Tx, attemptID, taskID string) (bool, store.TaskStatus, error) {
	var current string
	if err := tx.QueryRowContext(ctx, sqlSelectTaskStatus, taskID).Scan(&current); err != nil {
		return false, "", mapErr(err)
	}
	var attemptStatus string
	err := tx.QueryRowContext(ctx, sqlSelectAttemptStatus, attemptID, taskID).Scan(&attemptStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return false, store.TaskStatus(current), nil
	}
	if err != nil {
		return false, "", fmt.Errorf("sqlite: attempt %s status: %w", attemptID, mapErr(err))
	}
	var latest bool
	if err := tx.QueryRowContext(ctx, sqlIsLatestAttempt, attemptID, taskID, taskID).Scan(&latest); err != nil {
		return false, "", fmt.Errorf("sqlite: latest attempt of task %s: %w", taskID, mapErr(err))
	}
	return latest && attemptStatus == string(store.AttemptStatusRunning), store.TaskStatus(current), nil
}

// beginTaskTx opens a transaction and takes the task's job-row anchor.
func (s *Store) beginTaskTx(ctx context.Context, taskID, op string) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin %s: %w", op, mapErr(err))
	}
	var jobID string
	if err := tx.QueryRowContext(ctx, sqlTaskJobID, taskID).Scan(&jobID); err != nil {
		_ = tx.Rollback() //nolint:errcheck // the lookup error is what matters
		return nil, mapErr(err)
	}
	if err := lockAnchors(ctx, tx, jobAnchor(jobID)); err != nil {
		_ = tx.Rollback() //nolint:errcheck // the anchor error is what matters
		return nil, err
	}
	return tx, nil
}

// StartTaskAttempt implements [store.TaskStore].
func (s *Store) StartTaskAttempt(ctx context.Context, attemptID, taskID, sessionID string, now time.Time) (bool, error) {
	tx, err := s.beginTaskTx(ctx, taskID, "start task attempt")
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback after commit is a no-op

	live, current, err := liveAttemptTx(ctx, tx, attemptID, taskID)
	if err != nil {
		return false, err
	}
	if !live || (current != store.TaskStatusAssigned && current != store.TaskStatusRunning) {
		return false, tx.Commit()
	}
	if current == store.TaskStatusAssigned {
		// Under the job anchor and the single write connection the guarded
		// write cannot miss, so its bool is not consulted.
		if _, err := casWriteTaskStatus(ctx, tx, taskID, store.TaskStatusAssigned, store.TaskStatusRunning, now); err != nil {
			return false, err
		}
	}
	if sessionID != "" {
		if _, err := tx.ExecContext(ctx, sqlSetRunningAttemptSession, sessionID, attemptID); err != nil {
			return false, fmt.Errorf("sqlite: record session of attempt %s: %w", attemptID, mapErr(err))
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("sqlite: commit start task attempt: %w", mapErr(err))
	}
	return true, nil
}

// sqlReclaimOneTask returns one in-flight task to ready. Binds: updated_at, id.
const sqlReclaimOneTask = `
UPDATE tasks
SET    status = 'ready', assigned_worker_id = NULL, assigned_at = NULL, updated_at = ?, unschedulable_reason = ''
WHERE  id = ? AND status IN ('assigned', 'running')`

// ReclaimTaskAttempt implements [store.TaskStore].
//
// Anchor and statement order are offlineWorker's for one task: the job row
// (beginTaskTx), then the task, then its attempts and claims
// (closeTaskAttemptsTx). No worker anchor is taken, because the worker row is
// neither read nor written here. The task is written before its attempt is
// closed for offlineWorker's reason. On Postgres (H4c) the task row must be
// locked FOR UPDATE after the anchor and before the latest-attempt check, as
// for StartTaskAttempt.
func (s *Store) ReclaimTaskAttempt(ctx context.Context, attemptID, taskID string, now time.Time) (bool, error) {
	tx, err := s.beginTaskTx(ctx, taskID, "reclaim task attempt")
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback after commit is a no-op

	live, current, err := liveAttemptTx(ctx, tx, attemptID, taskID)
	if err != nil {
		return false, err
	}
	if !live || (current != store.TaskStatusAssigned && current != store.TaskStatusRunning) {
		return false, tx.Commit()
	}
	nowText := timeToText(now.UTC())
	if _, err := tx.ExecContext(ctx, sqlReclaimOneTask, nowText, taskID); err != nil {
		return false, fmt.Errorf("sqlite: reclaim task %s: %w", taskID, mapErr(err))
	}
	if err := closeTaskAttemptsTx(ctx, tx, taskID, store.AttemptStatusFailed, store.FailureReasonWorkerShutdown, nowText); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("sqlite: commit reclaim task attempt: %w", mapErr(err))
	}
	return true, nil
}
