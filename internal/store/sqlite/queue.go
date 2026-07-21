// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/uberware/sqi/internal/store"
)

const queueCols = `
	id, farm_id, name, description, priority, max_concurrent_tasks, paused,
	max_attempts, retry_delay_seconds, failure_limit, run_as_user, run_as_group,
	created_at, updated_at`

const (
	sqlInsertQueue = `
INSERT INTO queues (
	id, farm_id, name, description, priority, max_concurrent_tasks, paused,
	max_attempts, retry_delay_seconds, failure_limit, run_as_user, run_as_group,
	created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING ` + queueCols

	sqlGetQueue = `SELECT ` + queueCols + ` FROM queues WHERE id = ?`

	sqlUpdateQueue = `
UPDATE queues
SET farm_id = ?, name = ?, description = ?, priority = ?, max_concurrent_tasks = ?, paused = ?,
	max_attempts = ?, retry_delay_seconds = ?, failure_limit = ?, run_as_user = ?, run_as_group = ?,
	updated_at = ?
WHERE id = ?
RETURNING ` + queueCols

	sqlDeleteQueue = `DELETE FROM queues WHERE id = ?`
)

// queueSortColumns maps [store.QueueSortField] values to safe SQL column names.
var queueSortColumns = map[store.QueueSortField]string{
	store.QueueSortByName:      "name",
	store.QueueSortByPriority:  "priority",
	store.QueueSortByCreatedAt: "created_at",
}

func scanQueue(row scanner) (store.Queue, error) {
	var q store.Queue
	var paused int
	var createdAt, updatedAt string
	var maxAtt, delay, failLim sql.NullInt64
	var runAsUser, runAsGroup sql.NullString
	if err := row.Scan(
		&q.ID, &q.FarmID, &q.Name, &q.Description,
		&q.Priority, &q.MaxConcurrentTasks, &paused,
		&maxAtt, &delay, &failLim, &runAsUser, &runAsGroup,
		&createdAt, &updatedAt,
	); err != nil {
		return store.Queue{}, err
	}
	q.Paused = paused != 0
	q.MaxAttempts, q.RetryDelaySeconds, q.FailureLimit = intPtr(maxAtt), intPtr(delay), intPtr(failLim)
	q.RunAsUser, q.RunAsGroup = strPtr(runAsUser), strPtr(runAsGroup)
	q.CreatedAt = mustTime(createdAt)
	q.UpdatedAt = mustTime(updatedAt)
	return q, nil
}

// CreateQueue implements [store.QueueStore].
func (s *Store) CreateQueue(ctx context.Context, queue store.Queue) (store.Queue, error) {
	now := timeToText(time.Now().UTC())
	row := s.stmtInsertQueue.QueryRowContext(ctx,
		queue.ID, queue.FarmID, queue.Name, queue.Description,
		queue.Priority, queue.MaxConcurrentTasks, boolToInt(queue.Paused),
		nullInt(queue.MaxAttempts), nullInt(queue.RetryDelaySeconds), nullInt(queue.FailureLimit),
		nullStrPtr(queue.RunAsUser), nullStrPtr(queue.RunAsGroup),
		now, now)
	out, err := scanQueue(row)
	return out, mapErr(err)
}

// GetQueue implements [store.QueueStore].
func (s *Store) GetQueue(ctx context.Context, id string) (store.Queue, error) {
	row := s.stmtGetQueue.QueryRowContext(ctx, id)
	out, err := scanQueue(row)
	return out, mapErr(err)
}

// ListQueues implements [store.QueueStore].
// Dynamic filters use parameterised ad-hoc queries; the sort column is looked
// up from a hard-coded allow-list to prevent injection.
func (s *Store) ListQueues(ctx context.Context, opts store.ListQueuesOptions) (store.Page[store.Queue], error) {
	opts.Pagination.Validate() //nolint:errcheck // Validate only clamps; never errors

	where := ` WHERE 1=1`
	args := make([]any, 0, 3)

	if opts.FarmID != "" {
		where += ` AND farm_id = ?`
		args = append(args, opts.FarmID)
	}
	if opts.Paused != nil {
		where += ` AND paused = ?`
		args = append(args, boolToInt(*opts.Paused))
	}

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM queues`+where, args...).Scan(&total); err != nil {
		return store.Page[store.Queue]{}, mapErr(err)
	}

	col, ok := queueSortColumns[opts.SortBy]
	if !ok {
		col = "name"
	}
	dir := sortDirKeyword(opts.SortDir)

	// col comes from queueSortColumns (hard-coded allow-list); dir is "ASC" or
	// "DESC" from sortDirKeyword; where uses only ? placeholders for user values.
	q := `SELECT ` + queueCols + ` FROM queues` + where + //nolint:gosec // see comment above
		` ORDER BY ` + col + ` ` + dir +
		` LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(ctx, q, append(args, opts.Pagination.Limit, opts.Pagination.Offset)...)
	if err != nil {
		return store.Page[store.Queue]{}, mapErr(err)
	}
	defer rows.Close()

	queues := make([]store.Queue, 0)
	for rows.Next() {
		q, err := scanQueue(rows)
		if err != nil {
			return store.Page[store.Queue]{}, err
		}
		queues = append(queues, q)
	}
	if err := rows.Err(); err != nil {
		return store.Page[store.Queue]{}, err
	}
	return store.Page[store.Queue]{
		Items:  queues,
		Total:  total,
		Limit:  opts.Pagination.Limit,
		Offset: opts.Pagination.Offset,
	}, nil
}

// UpdateQueue implements [store.QueueStore].
func (s *Store) UpdateQueue(ctx context.Context, queue store.Queue) (store.Queue, error) {
	now := timeToText(time.Now().UTC())
	row := s.stmtUpdateQueue.QueryRowContext(ctx,
		queue.FarmID, queue.Name, queue.Description,
		queue.Priority, queue.MaxConcurrentTasks, boolToInt(queue.Paused),
		nullInt(queue.MaxAttempts), nullInt(queue.RetryDelaySeconds), nullInt(queue.FailureLimit),
		nullStrPtr(queue.RunAsUser), nullStrPtr(queue.RunAsGroup),
		now, queue.ID)
	out, err := scanQueue(row)
	return out, mapErr(err)
}

// DeleteQueue implements [store.QueueStore].
func (s *Store) DeleteQueue(ctx context.Context, id string) error {
	res, err := s.stmtDeleteQueue.ExecContext(ctx, id)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}
