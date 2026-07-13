// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/uberware/sqi/internal/store"
)

const workerCols = `
	id, farm_id, queue_id, name, hostname, ip_address, compute_location,
	os, os_version, version, cpu_count, ram_mb, gpu_info, tags, status,
	last_heartbeat_at, registered_at, updated_at`

const (
	// ON CONFLICT preserves registered_at so re-registration does not reset it.
	sqlUpsertWorker = `
INSERT INTO workers (
	id, farm_id, queue_id, name, hostname, ip_address, compute_location,
	os, os_version, version, cpu_count, ram_mb, gpu_info, tags, status,
	last_heartbeat_at, registered_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
	farm_id           = excluded.farm_id,
	queue_id          = excluded.queue_id,
	name              = excluded.name,
	hostname          = excluded.hostname,
	ip_address        = excluded.ip_address,
	compute_location  = excluded.compute_location,
	os                = excluded.os,
	os_version        = excluded.os_version,
	version           = excluded.version,
	cpu_count         = excluded.cpu_count,
	ram_mb            = excluded.ram_mb,
	gpu_info          = excluded.gpu_info,
	tags              = excluded.tags,
	status            = excluded.status,
	last_heartbeat_at = excluded.last_heartbeat_at,
	updated_at        = excluded.updated_at
RETURNING ` + workerCols

	sqlGetWorker = `SELECT ` + workerCols + ` FROM workers WHERE id = ?`

	sqlUpdateWorker = `
UPDATE workers
SET farm_id = ?, queue_id = ?, name = ?, hostname = ?, ip_address = ?, compute_location = ?,
	os = ?, os_version = ?, version = ?, cpu_count = ?, ram_mb = ?, gpu_info = ?, tags = ?,
	status = ?, last_heartbeat_at = ?, updated_at = ?
WHERE id = ?
RETURNING ` + workerCols

	sqlUpdateWorkerStatus = `
UPDATE workers SET status = ?, updated_at = ? WHERE id = ?`

	sqlUpdateWorkerHeartbeat = `
UPDATE workers SET last_heartbeat_at = ?, updated_at = ? WHERE id = ?`

	sqlListStaleWorkers = `SELECT ` + workerCols + `
FROM workers WHERE status = 'online' AND last_heartbeat_at < ?`

	// Online workers with no active (assigned or running) task.
	// An empty farmID is handled in Go by choosing the appropriate variant.
	sqlCountIdleWorkers = `
SELECT COUNT(*)
FROM   workers w
WHERE  w.farm_id = ?
  AND  w.status  = 'online'
  AND  NOT EXISTS (
         SELECT 1 FROM tasks t
         WHERE  t.assigned_worker_id = w.id
           AND  t.status IN ('assigned', 'running')
       )`

	sqlCountIdleWorkersAllFarms = `
SELECT COUNT(*)
FROM   workers w
WHERE  w.status = 'online'
  AND  NOT EXISTS (
         SELECT 1 FROM tasks t
         WHERE  t.assigned_worker_id = w.id
           AND  t.status IN ('assigned', 'running')
       )`

	sqlDeleteWorker = `DELETE FROM workers WHERE id = ?`

	// Deletes offline workers last seen before the cutoff and returns the
	// removed rows so the caller can emit notifications. NULL last_heartbeat_at
	// never matches (NULL < ? is NULL), so a never-seen worker is left alone.
	sqlDeleteOfflineWorkersBefore = `
DELETE FROM workers
WHERE status = 'offline' AND last_heartbeat_at < ?
RETURNING ` + workerCols
)

func scanWorker(row scanner) (store.Worker, error) {
	var w store.Worker
	var farmID, queueID, lastHeartbeat sql.NullString
	var gpuJSON, tagsJSON, status string
	var registeredAt, updatedAt string

	if err := row.Scan(
		&w.ID, &farmID, &queueID, &w.Name, &w.Hostname, &w.IPAddress, &w.ComputeLocation,
		&w.OS, &w.OSVersion, &w.Version, &w.CPUCount, &w.RAMMb, &gpuJSON, &tagsJSON, &status,
		&lastHeartbeat, &registeredAt, &updatedAt,
	); err != nil {
		return store.Worker{}, err
	}

	w.FarmID = farmID.String
	w.QueueID = queueID.String
	w.Status = store.WorkerStatus(status)
	w.LastHeartbeatAt = nullTextToTime(lastHeartbeat)
	w.RegisteredAt = mustTime(registeredAt)
	w.UpdatedAt = mustTime(updatedAt)

	gpu, err := unmarshalJSON(gpuJSON, store.GPUInfo{})
	if err != nil {
		return store.Worker{}, err
	}
	w.GPUInfo = gpu

	tags, err := unmarshalJSON(tagsJSON, map[string]string{})
	if err != nil {
		return store.Worker{}, err
	}
	w.Tags = tags

	return w, nil
}

func workerBindArgs(w store.Worker, now string) ([]any, error) {
	gpuJSON, err := marshalJSON(w.GPUInfo)
	if err != nil {
		return nil, err
	}
	tagsJSON, err := marshalJSON(w.Tags)
	if err != nil {
		return nil, err
	}
	return []any{
		w.ID, nullString(w.FarmID), nullString(w.QueueID), w.Name, w.Hostname, w.IPAddress, w.ComputeLocation,
		w.OS, w.OSVersion, w.Version, w.CPUCount, w.RAMMb, gpuJSON, tagsJSON, string(w.Status),
		nullTimeToText(w.LastHeartbeatAt), now, now,
	}, nil
}

// RegisterWorker implements [store.WorkerStore].
func (s *Store) RegisterWorker(ctx context.Context, worker store.Worker) (store.Worker, error) {
	now := timeToText(time.Now().UTC())
	args, err := workerBindArgs(worker, now)
	if err != nil {
		return store.Worker{}, err
	}
	row := s.stmtUpsertWorker.QueryRowContext(ctx, args...)
	out, err := scanWorker(row)
	return out, mapErr(err)
}

// GetWorker implements [store.WorkerStore].
func (s *Store) GetWorker(ctx context.Context, id string) (store.Worker, error) {
	row := s.stmtGetWorker.QueryRowContext(ctx, id)
	out, err := scanWorker(row)
	return out, mapErr(err)
}

// workerSortColumns maps [store.WorkerSortField] values to safe SQL column names.
var workerSortColumns = map[store.WorkerSortField]string{
	store.WorkerSortByHostname:        "hostname",
	store.WorkerSortByStatus:          "status",
	store.WorkerSortByRegisteredAt:    "registered_at",
	store.WorkerSortByLastHeartbeatAt: "last_heartbeat_at",
}

// ListWorkers implements [store.WorkerStore].
// Dynamic filters are applied via parameterised ad-hoc queries because the
// WHERE clause varies; this is safe as all values are bound via placeholders.
// The sort column is looked up from a hard-coded allow-list.
func (s *Store) ListWorkers(ctx context.Context, opts store.ListWorkersOptions) (store.Page[store.Worker], error) {
	opts.Pagination.Validate() //nolint:errcheck // Validate only clamps; never errors

	where := ` WHERE 1=1`
	args := make([]any, 0, 4)

	if opts.FarmID != "" {
		if opts.IncludeUnaffiliated {
			where += ` AND (farm_id = ? OR farm_id IS NULL)`
		} else {
			where += ` AND farm_id = ?`
		}
		args = append(args, opts.FarmID)
	}
	if opts.QueueID != "" {
		where += ` AND queue_id = ?`
		args = append(args, opts.QueueID)
	}
	if opts.ComputeLocation != "" {
		where += ` AND compute_location = ?`
		args = append(args, opts.ComputeLocation)
	}
	if opts.Status != "" {
		where += ` AND status = ?`
		args = append(args, string(opts.Status))
	}
	if frag, sargs := searchClause([]string{"name", "hostname", "id", "compute_location"}, opts.Search); frag != "" {
		where += frag
		args = append(args, sargs...)
	}

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workers`+where, args...).Scan(&total); err != nil {
		return store.Page[store.Worker]{}, mapErr(err)
	}

	col, ok := workerSortColumns[opts.SortBy]
	if !ok {
		col = "hostname"
	}
	dir := sortDirKeyword(opts.SortDir)

	// col comes from workerSortColumns (hard-coded allow-list); dir is "ASC" or
	// "DESC" from sortDirKeyword; where uses only ? placeholders for user values.
	q := `SELECT ` + workerCols + ` FROM workers` + where + //nolint:gosec // see comment above
		` ORDER BY ` + col + ` ` + dir +
		` LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(ctx, q, append(args, opts.Pagination.Limit, opts.Pagination.Offset)...)
	if err != nil {
		return store.Page[store.Worker]{}, mapErr(err)
	}
	defer rows.Close()

	workers := make([]store.Worker, 0)
	for rows.Next() {
		w, err := scanWorker(rows)
		if err != nil {
			return store.Page[store.Worker]{}, err
		}
		workers = append(workers, w)
	}
	if err := rows.Err(); err != nil {
		return store.Page[store.Worker]{}, err
	}
	return store.Page[store.Worker]{
		Items:  workers,
		Total:  total,
		Limit:  opts.Pagination.Limit,
		Offset: opts.Pagination.Offset,
	}, nil
}

// UpdateWorker implements [store.WorkerStore].
func (s *Store) UpdateWorker(ctx context.Context, worker store.Worker) (store.Worker, error) {
	gpuJSON, err := marshalJSON(worker.GPUInfo)
	if err != nil {
		return store.Worker{}, err
	}
	tagsJSON, err := marshalJSON(worker.Tags)
	if err != nil {
		return store.Worker{}, err
	}
	now := timeToText(time.Now().UTC())
	row := s.stmtUpdateWorker.QueryRowContext(ctx,
		nullString(worker.FarmID), nullString(worker.QueueID), worker.Name, worker.Hostname, worker.IPAddress,
		worker.ComputeLocation, worker.OS, worker.OSVersion, worker.Version, worker.CPUCount, worker.RAMMb,
		gpuJSON, tagsJSON, string(worker.Status), nullTimeToText(worker.LastHeartbeatAt),
		now, worker.ID)
	out, err := scanWorker(row)
	return out, mapErr(err)
}

// UpdateWorkerStatus implements [store.WorkerStore].
func (s *Store) UpdateWorkerStatus(ctx context.Context, id string, status store.WorkerStatus) error {
	res, err := s.stmtUpdateWorkerStatus.ExecContext(ctx, string(status), timeToText(time.Now().UTC()), id)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}

// UpdateWorkerHeartbeat implements [store.WorkerStore].
func (s *Store) UpdateWorkerHeartbeat(ctx context.Context, id string, at time.Time) error {
	now := timeToText(time.Now().UTC())
	res, err := s.stmtUpdateWorkerHeartbeat.ExecContext(ctx, timeToText(at), now, id)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}

// ListStaleWorkers implements [store.WorkerStore].
func (s *Store) ListStaleWorkers(ctx context.Context, before time.Time) ([]store.Worker, error) {
	rows, err := s.stmtListStaleWorkers.QueryContext(ctx, timeToText(before))
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var workers []store.Worker
	for rows.Next() {
		w, err := scanWorker(rows)
		if err != nil {
			return nil, err
		}
		workers = append(workers, w)
	}
	return workers, rows.Err()
}

// CountIdleWorkers implements [store.WorkerStore].
// When farmID is empty all farms are counted.
func (s *Store) CountIdleWorkers(ctx context.Context, farmID string) (int, error) {
	var (
		n   int
		err error
	)
	if farmID == "" {
		err = s.stmtCountIdleWorkersAllFarms.QueryRowContext(ctx).Scan(&n)
	} else {
		err = s.stmtCountIdleWorkers.QueryRowContext(ctx, farmID).Scan(&n)
	}
	return n, mapErr(err)
}

// DeleteWorker implements [store.WorkerStore].
func (s *Store) DeleteWorker(ctx context.Context, id string) error {
	res, err := s.stmtDeleteWorker.ExecContext(ctx, id)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}

// DeleteOfflineWorkersBefore implements [store.WorkerStore].
func (s *Store) DeleteOfflineWorkersBefore(ctx context.Context, cutoff time.Time) ([]store.Worker, error) {
	rows, err := s.stmtDeleteOfflineWorkers.QueryContext(ctx, timeToText(cutoff))
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var workers []store.Worker
	for rows.Next() {
		w, err := scanWorker(rows)
		if err != nil {
			return nil, err
		}
		workers = append(workers, w)
	}
	return workers, rows.Err()
}
