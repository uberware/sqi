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

const workerCols = `
	id, farm_id, queue_id, name, hostname, ip_address, compute_location,
	os, os_version, arch, version, cpu_count, ram_mb, gpu_info, tags, expr_limits, extensions, status,
	last_heartbeat_at, registered_at, updated_at, instance_id, disabled, registration_broker_at,
	heartbeat_broker_at, capacity_group_id, ephemeral,
	power_state, power_state_at, power_reason, interfaces, cloud_instance_id`

// sqlWorkerHasWorkInFlight is true when the worker row's id holds an assigned
// or running task. It is a fragment for a WHERE clause over workers.
const sqlWorkerHasWorkInFlight = `EXISTS (SELECT 1 FROM tasks t
  WHERE t.assigned_worker_id = workers.id AND t.status IN ('assigned', 'running'))`

// sqlWorkerEffectiveStatus is [store.Worker.EffectiveStatus] in SQL, for the
// status filter and sort: 'disabled' while the flag is set, else the liveness.
const sqlWorkerEffectiveStatus = `CASE WHEN disabled THEN 'disabled' ELSE status END`

// sqlDeleteEphemeralWorkerWhere deletes the ephemeral worker id while it is
// offline and holds no task. It binds only the id, so a statement extending
// it binds its own placeholders after.
const sqlDeleteEphemeralWorkerWhere = `
DELETE FROM workers
WHERE id = ? AND ephemeral AND status = 'offline'
  AND NOT ` + sqlWorkerHasWorkInFlight

const (
	// ON CONFLICT preserves registered_at, so re-registration does not reset it,
	// and an empty instance_id (a worker that sends none) keeps the stored one,
	// as a NULL registration_broker_at does. disabled, heartbeat_broker_at,
	// capacity_group_id, ephemeral, power_reason and the power-state columns
	// are not in the column list, so a new worker is enabled, in no group and
	// awake, and a re-registration never changes disabled, heartbeat_broker_at,
	// capacity_group_id or ephemeral.
	//
	// interfaces and cloud_instance_id are written from the registration, except
	// that an empty report ('[]', '') keeps the stored value: a failed interface
	// or metadata read on one start must not erase the only record of the
	// machine's wake address and instance. The power state is decided in the
	// same statement: a waking, asleep or wake-failed worker has registered, so
	// it is awake as of now with no reason; a retiring worker is kept retiring
	// (its machine is still going to sleep); an awake one is unchanged.
	sqlUpsertWorker = `
INSERT INTO workers (
	id, farm_id, queue_id, name, hostname, ip_address, compute_location,
	os, os_version, arch, version, cpu_count, ram_mb, gpu_info, tags, expr_limits, extensions, status,
	last_heartbeat_at, registered_at, updated_at, instance_id, registration_broker_at,
	interfaces, cloud_instance_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
	extensions        = excluded.extensions,
	status            = excluded.status,
	last_heartbeat_at = excluded.last_heartbeat_at,
	updated_at        = excluded.updated_at,
	instance_id       = CASE WHEN excluded.instance_id = '' THEN workers.instance_id ELSE excluded.instance_id END,
	registration_broker_at = COALESCE(excluded.registration_broker_at, workers.registration_broker_at),
	interfaces        = CASE WHEN excluded.interfaces = '[]' THEN workers.interfaces ELSE excluded.interfaces END,
	cloud_instance_id = CASE WHEN excluded.cloud_instance_id = '' THEN workers.cloud_instance_id
	                         ELSE excluded.cloud_instance_id END,
	power_state_at    = CASE WHEN workers.power_state IN ('waking', 'asleep', 'wake_failed')
	                         THEN excluded.updated_at ELSE workers.power_state_at END,
	power_reason      = CASE WHEN workers.power_state IN ('waking', 'asleep', 'wake_failed')
	                         THEN '' ELSE workers.power_reason END,
	power_state       = CASE WHEN workers.power_state IN ('waking', 'asleep', 'wake_failed')
	                         THEN 'awake' ELSE workers.power_state END
RETURNING ` + workerCols

	sqlGetWorker = `SELECT ` + workerCols + ` FROM workers WHERE id = ?`

	// sqlGetWorkerRegistration reads what RegisterWorker compares a
	// registration with: the instance it last came from and its broker stamp.
	sqlGetWorkerRegistration = `SELECT instance_id, registration_broker_at, power_state, power_state_at FROM workers WHERE id = ?`

	sqlSetWorkerDisabled = `
UPDATE workers SET disabled = ?, updated_at = ? WHERE id = ?
RETURNING ` + workerCols

	// sqlUpdateWorkerHeartbeat orders heartbeats by the broker's stamp, not by
	// the time recorded: one stamped earlier than heartbeat_broker_at (a late
	// redelivery) matches no row, and any other is recorded whatever its
	// last_heartbeat_at. A NULL stamp (both stamp arguments) matches as the
	// stored one does, so an unstamped heartbeat is recorded unconditionally
	// and keeps the stored stamp.
	sqlUpdateWorkerHeartbeat = `
UPDATE workers SET last_heartbeat_at = ?, updated_at = ?,
	heartbeat_broker_at = COALESCE(?, heartbeat_broker_at)
WHERE id = ? AND (heartbeat_broker_at IS NULL OR heartbeat_broker_at <= COALESCE(?, heartbeat_broker_at))`

	sqlWorkerExists = `SELECT 1 FROM workers WHERE id = ?`

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
  AND  w.status  = 'online' AND NOT w.disabled
  AND  NOT EXISTS (
         SELECT 1 FROM tasks t
         WHERE  t.assigned_worker_id = w.id
           AND  t.status IN ('assigned', 'running')
       )`

	sqlCountIdleWorkersAllFarms = `
SELECT COUNT(*)
FROM   workers w
WHERE  w.status = 'online' AND NOT w.disabled
  AND  NOT EXISTS (
         SELECT 1 FROM tasks t
         WHERE  t.assigned_worker_id = w.id
           AND  t.status IN ('assigned', 'running')
       )`

	// sqlDeleteWorkerIfRemovable carries the removability rule in its WHERE so
	// the check and the delete are one statement. It mirrors
	// [store.Worker.Removable], the one Go statement of the rule: SQL cannot
	// call Go, so the two are kept in step by hand and pinned against each
	// other by TestDeleteWorkerIfRemovable. The in-flight arm is the one
	// condition a Worker value cannot see, so it is stated here and in the
	// fake, not in Removable.
	sqlDeleteWorkerIfRemovable = `
DELETE FROM workers
WHERE id = ? AND status = 'offline'
  AND NOT ` + sqlWorkerHasWorkInFlight

	// Deletes enabled offline workers in no capacity group last seen before
	// the cutoff and returns the removed rows so the caller can emit
	// notifications. A disabled worker is kept until an operator removes it,
	// and a group member is left to the capacity sweep, which revokes an
	// ephemeral member's credential as it deletes the row. NULL
	// last_heartbeat_at never matches (NULL < ? is NULL), so a never-seen
	// worker is left alone.
	sqlDeleteOfflineWorkersBefore = `
DELETE FROM workers
WHERE status = 'offline' AND NOT disabled AND last_heartbeat_at < ?
  AND capacity_group_id IS NULL
RETURNING ` + workerCols

	sqlGetCapacityGroupExists = `SELECT 1 FROM capacity_groups WHERE id = ?`

	// sqlSetWorkerCapacityGroup writes the membership.
	sqlSetWorkerCapacityGroup = `
UPDATE workers SET capacity_group_id = ?, ephemeral = ?, updated_at = ? WHERE id = ?
RETURNING ` + workerCols

	// sqlLeaveWorkerCapacityGroup clears the membership and the ephemeral flag
	// and returns the worker to awake: power state belongs to the group, and a
	// worker outside one is not asleep. The state's time moves only when the
	// state does. Binds: ephemeral (false), the time, updated_at, worker.
	sqlLeaveWorkerCapacityGroup = `
UPDATE workers SET capacity_group_id = NULL, ephemeral = ?,
	power_state_at = CASE WHEN power_state <> 'awake' THEN ? ELSE power_state_at END,
	power_reason = '', power_state = 'awake', updated_at = ?
WHERE id = ?
RETURNING ` + workerCols

	sqlListCapacityGroupMembers = `SELECT ` + workerCols + `
FROM workers WHERE capacity_group_id = ? ORDER BY hostname, id`

	// sqlDeleteEphemeralWorker carries the whole rule in its WHERE, so the
	// check and the delete are one statement.
	sqlDeleteEphemeralWorker = sqlDeleteEphemeralWorkerWhere + `
RETURNING ` + workerCols

	// sqlDeleteVanishedEphemeralWorker adds the capacity sweep's conditions to
	// sqlDeleteEphemeralWorker's: not disabled, and last seen before the
	// cutoff. NULL last_heartbeat_at never matches (NULL < ? is NULL).
	sqlDeleteVanishedEphemeralWorker = sqlDeleteEphemeralWorkerWhere + `
  AND NOT disabled AND last_heartbeat_at < ?
RETURNING ` + workerCols
)

func scanWorker(row scanner) (store.Worker, error) {
	var w store.Worker
	var farmID, queueID, lastHeartbeat, registrationBroker, heartbeatBroker, capacityGroupID, powerAt sql.NullString
	var gpuJSON, tagsJSON, exprJSON, extJSON, ifaceJSON, status, powerState string
	var registeredAt, updatedAt string
	var disabled, ephemeral dbBool

	if err := row.Scan(
		&w.ID, &farmID, &queueID, &w.Name, &w.Hostname, &w.IPAddress, &w.ComputeLocation,
		&w.OS, &w.OSVersion, &w.Arch, &w.Version, &w.CPUCount, &w.RAMMb, &gpuJSON, &tagsJSON, &exprJSON, &extJSON, &status,
		&lastHeartbeat, &registeredAt, &updatedAt, &w.InstanceID, &disabled, &registrationBroker,
		&heartbeatBroker, &capacityGroupID, &ephemeral,
		&powerState, &powerAt, &w.PowerReason, &ifaceJSON, &w.CloudInstanceID,
	); err != nil {
		return store.Worker{}, err
	}

	w.FarmID = farmID.String
	w.QueueID = queueID.String
	w.Status = store.WorkerStatus(status)
	w.LastHeartbeatAt = nullTextToTime(lastHeartbeat)
	w.RegistrationBrokerAt = nullTextToTime(registrationBroker)
	w.HeartbeatBrokerAt = nullTextToTime(heartbeatBroker)
	w.RegisteredAt = mustTime(registeredAt)
	w.UpdatedAt = mustTime(updatedAt)
	w.Disabled = bool(disabled)
	w.CapacityGroupID = capacityGroupID.String
	w.Ephemeral = bool(ephemeral)
	w.PowerState = store.PowerState(powerState)
	w.PowerStateAt = nullTextToTime(powerAt)

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

	extensions, err := unmarshalJSON(extJSON, []string(nil))
	if err != nil {
		return store.Worker{}, err
	}
	w.Extensions = extensions

	interfaces, err := unmarshalJSON(ifaceJSON, []store.NetInterface(nil))
	if err != nil {
		return store.Worker{}, err
	}
	if len(interfaces) > 0 {
		w.Interfaces = interfaces
	}

	return w, nil
}

// workerJSONCols marshals the worker columns stored as JSON TEXT, in the order
// they appear in [workerCols].
//
// Every caller that persists a worker needs all of the columns, so they are
// marshaled in this one function: a JSON column added to only one of several
// copies would still compile and would be dropped on the other write paths.
func workerJSONCols(w store.Worker) (gpuJSON, tagsJSON, exprJSON, extJSON string, err error) {
	if gpuJSON, err = marshalJSON(w.GPUInfo); err != nil {
		return "", "", "", "", err
	}
	if tagsJSON, err = marshalJSON(w.Tags); err != nil {
		return "", "", "", "", err
	}
	if exprJSON, err = marshalJSON(w.ExprLimits); err != nil {
		return "", "", "", "", err
	}
	// A nil slice marshals as "null"; the column holds a list, '[]' when empty,
	// as its default does.
	exts := w.Extensions
	if exts == nil {
		exts = []string{}
	}
	if extJSON, err = marshalJSON(exts); err != nil {
		return "", "", "", "", err
	}
	return gpuJSON, tagsJSON, exprJSON, extJSON, nil
}

// workerBindArgs are the arguments of sqlUpsertWorker, in its column order.
func workerBindArgs(w store.Worker, now string) ([]any, error) {
	gpuJSON, tagsJSON, exprJSON, extJSON, err := workerJSONCols(w)
	if err != nil {
		return nil, err
	}
	// A nil slice marshals as "null"; the column holds a list, '[]' when empty.
	ifaces := w.Interfaces
	if ifaces == nil {
		ifaces = []store.NetInterface{}
	}
	ifaceJSON, err := marshalJSON(ifaces)
	if err != nil {
		return nil, err
	}
	return []any{
		w.ID, nullString(w.FarmID), nullString(w.QueueID), w.Name, w.Hostname, w.IPAddress, w.ComputeLocation,
		w.OS, w.OSVersion, w.Arch, w.Version, w.CPUCount, w.RAMMb, gpuJSON, tagsJSON, exprJSON, extJSON, string(w.Status),
		nullTimeToText(w.LastHeartbeatAt), now, now, w.InstanceID, nullTimeToText(w.RegistrationBrokerAt),
		ifaceJSON, w.CloudInstanceID,
	}, nil
}

// RegisterWorker implements [store.WorkerStore].
//
// Anchor: the worker row, then (on a restart) the reclaim's job rows, the same
// order as offlineWorker. A restart's reclaim is ordered against leases the way
// offlineWorker is: LeaseTask holds the worker row FOR SHARE, so a lease in
// flight is waited for and its task reclaimed.
func (s *Store) RegisterWorker(ctx context.Context, worker store.Worker) (store.Worker, store.Registration, error) {
	now := time.Now().UTC()
	args, err := workerBindArgs(worker, TimeText(now))
	if err != nil {
		return store.Worker{}, store.Registration{}, err
	}

	type result struct {
		worker store.Worker
		reg    store.Registration
		stale  bool
	}
	r, err := inTxValue(ctx, s, "register worker", func(tx *dbTx) (result, txOutcome, error) {
		if err := tx.lockAnchors(ctx, workerAnchor(worker.ID)); err != nil {
			return result{}, txRollback, err
		}
		prior, found, err := readRegistrationPrior(ctx, tx, worker.ID)
		if err != nil {
			return result{}, txRollback, err
		}
		if found && store.StaleRegistration(prior, worker) {
			// Nothing is written; the refusal returns the worker as stored.
			// Committing runs the fence check, so a deposed leader gets
			// ErrFenced here as it does for any other write.
			stored, err := scanWorker(tx.QueryRowContext(ctx, sqlGetWorker, worker.ID))
			if err != nil {
				return result{}, txRollback, fmt.Errorf("store: read worker %s: %w", worker.ID, mapErr(err))
			}
			return result{worker: stored, stale: true}, txCommit, nil
		}
		upsert := tx.Stmt(ctx, s.stmtUpsertWorker)
		defer upsert.Close()
		var out result
		out.worker, err = scanWorker(upsert.QueryRowContext(ctx, args...))
		if err != nil {
			return result{}, txRollback, mapErr(err)
		}
		if out.worker.PowerState == store.PowerAwake {
			out.reg.WokeFrom, out.reg.WokeFromAt = store.RegistrationWake(prior.PowerState, prior.PowerStateAt)
		}
		if prior.InstanceID != "" && worker.InstanceID != "" && prior.InstanceID != worker.InstanceID {
			if out.reg.Reclaimed, err = reclaimWorkerTasksTx(ctx, tx, worker.ID, store.FailureReasonWorkerRestarted, now); err != nil {
				return result{}, txRollback, err
			}
		}
		return out, txCommit, nil
	})
	if err == nil && r.stale {
		err = fmt.Errorf("store: register worker %s stamped %s, recorded %s: %w", worker.ID,
			worker.RegistrationBrokerAt.Format(time.RFC3339Nano), r.worker.RegistrationBrokerAt.Format(time.RFC3339Nano),
			store.ErrStaleRegistration)
	}
	return r.worker, r.reg, err
}

// readRegistrationPrior reads, under the worker's anchor, what RegisterWorker
// needs of the row it replaces: its instance ID, its registration's broker
// time and its power state since when. found is false when there is no row.
func readRegistrationPrior(ctx context.Context, tx *dbTx, workerID string) (prior store.Worker, found bool, err error) {
	var brokerAt, powerAt sql.NullString
	var power string
	err = tx.QueryRowContext(ctx, sqlGetWorkerRegistration, workerID).Scan(&prior.InstanceID, &brokerAt, &power, &powerAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return store.Worker{}, false, nil
	case err != nil:
		return store.Worker{}, false, fmt.Errorf("store: read worker %s: %w", workerID, mapErr(err))
	}
	prior.RegistrationBrokerAt, prior.PowerState, prior.PowerStateAt = nullTextToTime(brokerAt), store.PowerState(power), nullTextToTime(powerAt)
	return prior, true, nil
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
	if opts.CapacityGroupID != "" {
		where += ` AND capacity_group_id = ?`
		args = append(args, opts.CapacityGroupID)
	}
	if opts.Status != "" {
		where += ` AND ` + sqlWorkerEffectiveStatus + ` = ?`
		args = append(args, string(opts.Status))
	}
	if frag, sargs := searchClause(s.d, []string{"name", "hostname", "id", "compute_location"}, opts.Search); frag != "" {
		where += frag
		args = append(args, sargs...)
	}

	var total int
	if err := s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM workers`+where, args...).Scan(&total); err != nil {
		return store.Page[store.Worker]{}, mapErr(err)
	}

	col, ok := workerSortColumns[opts.SortBy]
	if !ok {
		col = "hostname"
	}
	dir := sortDirKeyword(opts.SortDir)

	// col comes from workerSortColumns (hard-coded allow-list); dir is "ASC" or
	// "DESC" from sortDirKeyword; orderByClause renders both, with an id
	// tie-break; where uses only ? placeholders for user values.
	q := `SELECT ` + workerCols + ` FROM workers` + where +
		orderByClause(col, dir, opts.SortBy == store.WorkerSortByLastHeartbeatAt) +
		` LIMIT ? OFFSET ?`
	rows, err := s.r.QueryContext(ctx, q, append(args, opts.Pagination.Limit, opts.Pagination.Offset)...)
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

// SetWorkerDisabled implements [store.WorkerStore].
func (s *Store) SetWorkerDisabled(ctx context.Context, id string, disabled bool) (store.Worker, error) {
	out, err := writeRow(ctx, s, "set worker disabled", scanWorker, s.stmtSetWorkerDisabled,
		disabled, TimeText(time.Now().UTC()), id)
	return out, mapErr(err)
}

// UpdateWorkerHeartbeat implements [store.WorkerStore].
func (s *Store) UpdateWorkerHeartbeat(ctx context.Context, id string, at time.Time, brokerAt *time.Time) error {
	now, beat, stamp := TimeText(time.Now().UTC()), TimeText(at), nullTimeToText(brokerAt)
	var unknown bool
	err := s.write(ctx, "update worker heartbeat", func(w writer) error {
		unknown = false
		res, err := w.Stmt(ctx, s.stmtUpdateWorkerHeartbeat).ExecContext(ctx, beat, now, stamp, id, stamp)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n > 0 {
			return err
		}
		// Nothing matched: an unknown worker, or a heartbeat the broker stamped
		// before the recorded one, which is ignored. Not-found is carried out
		// rather than returned, so a leadership context's fence check still
		// runs and a deposed leader gets ErrFenced for it.
		var one int
		switch err := w.QueryRowContext(ctx, sqlWorkerExists, id).Scan(&one); {
		case errors.Is(err, sql.ErrNoRows):
			unknown = true
			return nil
		default:
			return err
		}
	})
	if err == nil && unknown {
		return store.ErrNotFound
	}
	return mapErr(err)
}

// ListStaleWorkers implements [store.WorkerStore].
func (s *Store) ListStaleWorkers(ctx context.Context, before time.Time) ([]store.Worker, error) {
	rows, err := s.stmtListStaleWorkers.QueryContext(ctx, TimeText(before))
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

// DeleteWorkerIfRemovable implements [store.WorkerStore].
//
// The zero-rows case is told apart by a read after the delete: a worker that
// exists but did not match is [store.ErrConflict], one that is gone is
// [store.ErrNotFound]. A concurrent change between the two statements can only
// move the answer between those two errors, never delete a row the rule refused.
func (s *Store) DeleteWorkerIfRemovable(ctx context.Context, id string) error {
	res, err := s.exec(ctx, "delete worker", sqlDeleteWorkerIfRemovable, id)
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
// The deleted rows are read inside the write, before a fenced one commits.
func (s *Store) DeleteOfflineWorkersBefore(ctx context.Context, cutoff time.Time) ([]store.Worker, error) {
	var workers []store.Worker
	err := s.write(ctx, "delete offline workers", func(w writer) error {
		workers = nil
		rows, err := w.Stmt(ctx, s.stmtDeleteOfflineWorkers).QueryContext(ctx, TimeText(cutoff))
		if err != nil {
			return mapErr(err)
		}
		defer rows.Close()
		for rows.Next() {
			worker, err := scanWorker(rows)
			if err != nil {
				return err
			}
			workers = append(workers, worker)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return workers, nil
}

// SetWorkerCapacityGroup implements [store.WorkerStore].
//
// Anchor: the worker row. The group is read, not locked: deleting it while
// this transaction runs makes the foreign key refuse the write, which is
// [store.ErrConflict].
func (s *Store) SetWorkerCapacityGroup(ctx context.Context, workerID, groupID string, ephemeral bool) (store.Worker, error) {
	now := TimeText(time.Now().UTC())
	out, err := inTxValue(ctx, s, "set worker capacity group", func(tx *dbTx) (store.Worker, txOutcome, error) {
		if err := tx.lockAnchors(ctx, workerAnchor(workerID)); err != nil {
			return store.Worker{}, txRollback, err
		}
		current, err := scanWorker(tx.QueryRowContext(ctx, sqlGetWorker, workerID))
		if err != nil {
			return store.Worker{}, txRollback, mapErr(err)
		}
		if groupID != "" {
			var one int
			if err := tx.QueryRowContext(ctx, sqlGetCapacityGroupExists, groupID).Scan(&one); err != nil {
				return store.Worker{}, txRollback, mapErr(err)
			}
			if current.CapacityGroupID != "" && current.CapacityGroupID != groupID {
				return store.Worker{}, txRollback, fmt.Errorf("store: worker %s is already in capacity group %s: %w",
					workerID, current.CapacityGroupID, store.ErrConflict)
			}
		}
		var row scanner
		if groupID == "" {
			row = tx.QueryRowContext(ctx, sqlLeaveWorkerCapacityGroup, false, now, now, workerID)
		} else {
			row = tx.QueryRowContext(ctx, sqlSetWorkerCapacityGroup, groupID, ephemeral, now, workerID)
		}
		updated, err := scanWorker(row)
		if err != nil {
			return store.Worker{}, txRollback, mapErr(err)
		}
		return updated, txCommit, nil
	})
	return out, err
}

// ListCapacityGroupMembers implements [store.WorkerStore].
func (s *Store) ListCapacityGroupMembers(ctx context.Context, groupID string) ([]store.Worker, error) {
	rows, err := s.r.QueryContext(ctx, sqlListCapacityGroupMembers, groupID)
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
	return workers, mapErr(rows.Err())
}

// DeleteEphemeralWorker implements [store.WorkerStore].
//
// Anchor: the worker row, which orders the delete against a lease (LeaseTask
// holds it FOR SHARE) so a task leased to the worker is seen by the in-flight
// condition. The credential is revoked in the same transaction, only when the
// worker was deleted.
func (s *Store) DeleteEphemeralWorker(ctx context.Context, id string, now time.Time) (store.Worker, bool, error) {
	return s.deleteEphemeralWorker(ctx, "delete ephemeral worker", id, now, sqlDeleteEphemeralWorker, id)
}

// DeleteVanishedEphemeralWorker implements [store.WorkerStore], as
// [Store.DeleteEphemeralWorker] does.
func (s *Store) DeleteVanishedEphemeralWorker(ctx context.Context, id string, cutoff, now time.Time) (store.Worker, bool, error) {
	return s.deleteEphemeralWorker(ctx, "delete vanished ephemeral worker", id, now,
		sqlDeleteVanishedEphemeralWorker, id, TimeText(cutoff.UTC()))
}

// deleteEphemeralWorker runs query, a DELETE of worker id returning its row,
// under the worker's anchor and revokes the worker's credential at now when it
// deleted the row.
func (s *Store) deleteEphemeralWorker(
	ctx context.Context, op, id string, now time.Time, query string, args ...any,
) (store.Worker, bool, error) {
	type result struct {
		worker  store.Worker
		deleted bool
	}
	r, err := inTxValue(ctx, s, op, func(tx *dbTx) (result, txOutcome, error) {
		if err := tx.lockAnchors(ctx, workerAnchor(id)); err != nil {
			return result{}, txRollback, err
		}
		w, deleted, err := deleteEphemeralWorkerTx(ctx, tx, id, now, query, args...)
		if err != nil {
			return result{}, txRollback, err
		}
		// Nothing matched is not an error. Committing runs the fence check, so
		// a deposed leader still gets ErrFenced here.
		return result{worker: w, deleted: deleted}, txCommit, nil
	})
	return r.worker, r.deleted, err
}

// deleteEphemeralWorkerTx runs query, a DELETE of worker id returning its row,
// in tx, which holds the worker's anchor, and revokes the worker's credential
// at now when it deleted the row. deleted is false, with no error, when the
// DELETE matched nothing. [Store.FinishRetirement] shares it with the deletes
// above.
func deleteEphemeralWorkerTx(
	ctx context.Context, tx *dbTx, id string, now time.Time, query string, args ...any,
) (w store.Worker, deleted bool, err error) {
	w, err = scanWorker(tx.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Worker{}, false, nil
	}
	if err != nil {
		return store.Worker{}, false, mapErr(err)
	}
	if _, err := tx.ExecContext(ctx, sqlRevokeWorkerCredential, TimeText(now.UTC()), id); err != nil {
		return store.Worker{}, false, fmt.Errorf("store: revoke credential of worker %s: %w", id, mapErr(err))
	}
	return w, true, nil
}
