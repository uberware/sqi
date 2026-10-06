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

const workerCols = `
	id, farm_id, queue_id, name, hostname, ip_address, compute_location,
	os, os_version, arch, version, cpu_count, ram_mb, gpu_info, tags, expr_limits, status,
	last_heartbeat_at, registered_at, updated_at, instance_id, disabled`

// sqlWorkerHasWorkInFlight is true when the worker row's id holds an assigned
// or running task. It is a fragment for a WHERE clause over workers.
const sqlWorkerHasWorkInFlight = `EXISTS (SELECT 1 FROM tasks t
  WHERE t.assigned_worker_id = workers.id AND t.status IN ('assigned', 'running'))`

// sqlWorkerEffectiveStatus is [store.Worker.EffectiveStatus] in SQL, for the
// status filter and sort: 'disabled' while the flag is set, else the liveness.
const sqlWorkerEffectiveStatus = `CASE WHEN disabled = 1 THEN 'disabled' ELSE status END`

const (
	// ON CONFLICT preserves registered_at so re-registration does not reset it,
	// and an empty instance_id (a worker that sends none) keeps the stored one.
	// disabled is not in the column list, so a new worker is enabled and a
	// re-registration never changes the flag.
	sqlUpsertWorker = `
INSERT INTO workers (
	id, farm_id, queue_id, name, hostname, ip_address, compute_location,
	os, os_version, arch, version, cpu_count, ram_mb, gpu_info, tags, expr_limits, status,
	last_heartbeat_at, registered_at, updated_at, instance_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
	farm_id           = excluded.farm_id,
	queue_id          = excluded.queue_id,
	name              = excluded.name,
	hostname          = excluded.hostname,
	ip_address        = excluded.ip_address,
	compute_location  = excluded.compute_location,
	os                = excluded.os,
	os_version        = excluded.os_version,
	arch              = excluded.arch,
	version           = excluded.version,
	cpu_count         = excluded.cpu_count,
	ram_mb            = excluded.ram_mb,
	gpu_info          = excluded.gpu_info,
	tags              = excluded.tags,
	expr_limits       = excluded.expr_limits,
	status            = excluded.status,
	last_heartbeat_at = excluded.last_heartbeat_at,
	updated_at        = excluded.updated_at,
	instance_id       = CASE WHEN excluded.instance_id = '' THEN workers.instance_id ELSE excluded.instance_id END
RETURNING ` + workerCols

	sqlGetWorker = `SELECT ` + workerCols + ` FROM workers WHERE id = ?`

	sqlUpdateWorker = `
UPDATE workers
SET farm_id = ?, queue_id = ?, name = ?, hostname = ?, ip_address = ?, compute_location = ?,
	os = ?, os_version = ?, arch = ?, version = ?, cpu_count = ?, ram_mb = ?, gpu_info = ?, tags = ?,
	expr_limits = ?, status = ?, last_heartbeat_at = ?, updated_at = ?
WHERE id = ?
RETURNING ` + workerCols

	sqlSetWorkerDisabled = `
UPDATE workers SET disabled = ?, updated_at = ? WHERE id = ?
RETURNING ` + workerCols

	sqlUpdateWorkerHeartbeat = `
UPDATE workers SET last_heartbeat_at = ?, updated_at = ? WHERE id = ?`

	// sqlListStaleWorkers lists the heartbeat sweep's candidates: online
	// workers, disabled or not, whose heartbeat is older than the cutoff.
	sqlListStaleWorkers = `SELECT ` + workerCols + `
FROM workers WHERE status = 'online' AND last_heartbeat_at < ?`

	// Online, enabled workers with no active (assigned or running) task.
	// An empty farmID is handled in Go by choosing the appropriate variant.
	sqlCountIdleWorkers = `
SELECT COUNT(*)
FROM   workers w
WHERE  w.farm_id = ?
  AND  w.status  = 'online' AND w.disabled = 0
  AND  NOT EXISTS (
         SELECT 1 FROM tasks t
         WHERE  t.assigned_worker_id = w.id
           AND  t.status IN ('assigned', 'running')
       )`

	sqlCountIdleWorkersAllFarms = `
SELECT COUNT(*)
FROM   workers w
WHERE  w.status = 'online' AND w.disabled = 0
  AND  NOT EXISTS (
         SELECT 1 FROM tasks t
         WHERE  t.assigned_worker_id = w.id
           AND  t.status IN ('assigned', 'running')
       )`

	sqlDeleteWorker = `DELETE FROM workers WHERE id = ?`

	// sqlDeleteWorkerIfRemovable carries the removability rule in its WHERE so
	// the check and the delete are one statement (I1). It mirrors
	// [store.Worker.Removable], the one Go statement of the rule: SQL cannot
	// call Go, so the two are kept in step by hand and pinned against each
	// other by TestDeleteWorkerIfRemovable. The in-flight arm is the one
	// condition a Worker value cannot see, so it is stated here and in the
	// fake, not in Removable.
	sqlDeleteWorkerIfRemovable = `
DELETE FROM workers
WHERE id = ? AND status = 'offline'
  AND NOT ` + sqlWorkerHasWorkInFlight

	// Deletes enabled offline workers last seen before the cutoff and returns
	// the removed rows so the caller can emit notifications. A disabled worker
	// is kept until an operator removes it. NULL last_heartbeat_at never
	// matches (NULL < ? is NULL), so a never-seen worker is left alone.
	sqlDeleteOfflineWorkersBefore = `
DELETE FROM workers
WHERE status = 'offline' AND disabled = 0 AND last_heartbeat_at < ?
RETURNING ` + workerCols
)

func scanWorker(row scanner) (store.Worker, error) {
	var w store.Worker
	var farmID, queueID, lastHeartbeat sql.NullString
	var gpuJSON, tagsJSON, exprJSON, status string
	var registeredAt, updatedAt string

	if err := row.Scan(
		&w.ID, &farmID, &queueID, &w.Name, &w.Hostname, &w.IPAddress, &w.ComputeLocation,
		&w.OS, &w.OSVersion, &w.Arch, &w.Version, &w.CPUCount, &w.RAMMb, &gpuJSON, &tagsJSON, &exprJSON, &status,
		&lastHeartbeat, &registeredAt, &updatedAt, &w.InstanceID, &w.Disabled,
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

	exprLimits, err := unmarshalJSON(exprJSON, store.WorkerExprLimits{})
	if err != nil {
		return store.Worker{}, err
	}
	w.ExprLimits = exprLimits

	return w, nil
}

// workerJSONCols marshals the worker columns stored as JSON TEXT, in the order
// they appear in [workerCols].
//
// One function, not one copy per write path: every caller that persists a
// worker needs all of them, so a new JSON column added to only one copy would
// still compile and would silently drop that column on the paths it was not
// added to.
func workerJSONCols(w store.Worker) (gpuJSON, tagsJSON, exprJSON string, err error) {
	if gpuJSON, err = marshalJSON(w.GPUInfo); err != nil {
		return "", "", "", err
	}
	if tagsJSON, err = marshalJSON(w.Tags); err != nil {
		return "", "", "", err
	}
	if exprJSON, err = marshalJSON(w.ExprLimits); err != nil {
		return "", "", "", err
	}
	return gpuJSON, tagsJSON, exprJSON, nil
}

func workerBindArgs(w store.Worker, now string) ([]any, error) {
	gpuJSON, tagsJSON, exprJSON, err := workerJSONCols(w)
	if err != nil {
		return nil, err
	}
	return []any{
		w.ID, nullString(w.FarmID), nullString(w.QueueID), w.Name, w.Hostname, w.IPAddress, w.ComputeLocation,
		w.OS, w.OSVersion, w.Arch, w.Version, w.CPUCount, w.RAMMb, gpuJSON, tagsJSON, exprJSON, string(w.Status),
		nullTimeToText(w.LastHeartbeatAt), now, now,
	}, nil
}

// RegisterWorker implements [store.WorkerStore].
//
// Anchor: the worker row, then (on a restart) the reclaim's job rows, the same
// order as offlineWorker, and sharing the gap it leaves on PostgreSQL
// (LeaseTask does not anchor the worker row).
func (s *Store) RegisterWorker(ctx context.Context, worker store.Worker) (store.Worker, []store.Task, error) {
	now := time.Now().UTC()
	args, err := workerBindArgs(worker, timeToText(now))
	if err != nil {
		return store.Worker{}, nil, err
	}
	args = append(args, worker.InstanceID)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Worker{}, nil, fmt.Errorf("sqlite: begin register worker: %w", mapErr(err))
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback after commit is a no-op

	if err := lockAnchors(ctx, tx, workerAnchor(worker.ID)); err != nil {
		return store.Worker{}, nil, err
	}
	var prior string
	err = tx.QueryRowContext(ctx, `SELECT instance_id FROM workers WHERE id = ?`, worker.ID).Scan(&prior)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return store.Worker{}, nil, fmt.Errorf("sqlite: read worker %s instance: %w", worker.ID, mapErr(err))
	}
	upsert := tx.StmtContext(ctx, s.stmtUpsertWorker)
	defer upsert.Close()
	out, err := scanWorker(upsert.QueryRowContext(ctx, args...))
	if err != nil {
		return store.Worker{}, nil, mapErr(err)
	}
	var reclaimed []store.Task
	if prior != "" && worker.InstanceID != "" && prior != worker.InstanceID {
		if reclaimed, err = reclaimWorkerTasksTx(ctx, tx, worker.ID, store.FailureReasonWorkerRestarted, now); err != nil {
			return store.Worker{}, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return store.Worker{}, nil, fmt.Errorf("sqlite: commit register worker: %w", mapErr(err))
	}
	return out, reclaimed, nil
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
	store.WorkerSortByStatus:          sqlWorkerEffectiveStatus,
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
		where += ` AND ` + sqlWorkerEffectiveStatus + ` = ?`
		args = append(args, string(opts.Status))
	}
	if frag, sargs := searchClause([]string{"name", "hostname", "id", "compute_location"}, opts.Search); frag != "" {
		where += frag
		args = append(args, sargs...)
	}

	var total int
	if err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM workers`+where, args...).Scan(&total); err != nil {
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
	rows, err := s.rdb.QueryContext(ctx, q, append(args, opts.Pagination.Limit, opts.Pagination.Offset)...)
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
	gpuJSON, tagsJSON, exprJSON, err := workerJSONCols(worker)
	if err != nil {
		return store.Worker{}, err
	}
	now := timeToText(time.Now().UTC())
	row := s.stmtUpdateWorker.QueryRowContext(ctx,
		nullString(worker.FarmID), nullString(worker.QueueID), worker.Name, worker.Hostname, worker.IPAddress,
		worker.ComputeLocation, worker.OS, worker.OSVersion, worker.Arch, worker.Version, worker.CPUCount, worker.RAMMb,
		gpuJSON, tagsJSON, exprJSON, string(worker.Status), nullTimeToText(worker.LastHeartbeatAt),
		now, worker.ID)
	out, err := scanWorker(row)
	return out, mapErr(err)
}

// SetWorkerDisabled implements [store.WorkerStore].
func (s *Store) SetWorkerDisabled(ctx context.Context, id string, disabled bool) (store.Worker, error) {
	row := s.stmtSetWorkerDisabled.QueryRowContext(ctx, disabled, timeToText(time.Now().UTC()), id)
	out, err := scanWorker(row)
	return out, mapErr(err)
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

// DeleteWorker hard-deletes the worker unconditionally. Returns
// [store.ErrNotFound] if no such worker exists. Task and task-attempt rows that
// reference the worker by ID are left intact.
//
// Test fixture only: an unguarded delete that is not part of store.Store;
// removal goes through DeleteWorkerIfRemovable.
func (s *Store) DeleteWorker(ctx context.Context, id string) error {
	res, err := s.stmtDeleteWorker.ExecContext(ctx, id)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}

// DeleteWorkerIfRemovable implements [store.WorkerStore].
//
// The zero-rows case is told apart by a read AFTER the delete: a worker that
// exists but did not match is [store.ErrConflict], one that is gone is
// [store.ErrNotFound]. A concurrent change between the two statements can only
// move the answer between those two errors, never delete a row the rule refused.
func (s *Store) DeleteWorkerIfRemovable(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, sqlDeleteWorkerIfRemovable, id)
	if err != nil {
		return mapErr(err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 1 {
		return err
	}
	if _, err := s.GetWorker(ctx, id); err != nil {
		return err // ErrNotFound
	}
	return store.ErrConflict
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
