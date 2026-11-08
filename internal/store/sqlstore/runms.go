// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/uberware/sqi/internal/store"
)

const (
	// sqlLatestAttemptTimes reads a task's latest attempt as latestAttemptOf
	// picks it: the highest attempt_number, then the greatest id.
	sqlLatestAttemptTimes = `
SELECT status, started_at, ended_at FROM task_attempts
WHERE  task_id = ?
ORDER BY attempt_number DESC, id DESC
LIMIT 1`

	sqlSetTaskRunMs = `UPDATE tasks SET run_ms = ? WHERE id = ?`
)

// rowWriter is what [refreshTaskRunMs] needs: a transaction, or the
// injector's writer.
type rowWriter interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *dbRow
}

// refreshTaskRunMs stores taskID's tasks.run_ms from its current rows: the
// latest attempt's run time in whole milliseconds (rounded) when the task
// succeeded and that attempt succeeded with an ended_at no earlier than its
// started_at, and NULL otherwise. The remaining-time read sums the column, so
// every write that can make a task succeeded, or change which attempt is its
// latest, calls this.
func refreshTaskRunMs(ctx context.Context, w rowWriter, taskID string) error {
	ms, err := taskRunMs(ctx, w, taskID)
	if err != nil {
		return err
	}
	if _, err := w.ExecContext(ctx, sqlSetTaskRunMs, ms, taskID); err != nil {
		return fmt.Errorf("store: store run time of task %s: %w", taskID, mapErr(err))
	}
	return nil
}

func taskRunMs(ctx context.Context, w rowWriter, taskID string) (sql.NullInt64, error) {
	var none sql.NullInt64
	var status string
	if err := w.QueryRowContext(ctx, sqlSelectTaskStatus, taskID).Scan(&status); err != nil {
		return none, fmt.Errorf("store: status of task %s: %w", taskID, mapErr(err))
	}
	if store.TaskStatus(status) != store.TaskStatusSucceeded {
		return none, nil
	}
	var attemptStatus, startedAt string
	var endedAt sql.NullString
	err := w.QueryRowContext(ctx, sqlLatestAttemptTimes, taskID).Scan(&attemptStatus, &startedAt, &endedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return none, nil
	case err != nil:
		return none, fmt.Errorf("store: latest attempt of task %s: %w", taskID, mapErr(err))
	case store.AttemptStatus(attemptStatus) != store.AttemptStatusSucceeded || !endedAt.Valid:
		return none, nil
	}
	start, err := textToTime(startedAt)
	if err != nil {
		return none, err
	}
	end, err := textToTime(endedAt.String)
	if err != nil {
		return none, err
	}
	if end.Before(start) {
		return none, nil
	}
	return sql.NullInt64{Int64: store.RunMs(start, end), Valid: true}, nil
}
