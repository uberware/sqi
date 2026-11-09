// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// ── Capacity power transitions ────────────────────────────────────────────────
//
// Each transition is one guarded UPDATE run under the anchors of the workers
// it may move, so the state it requires is re-checked under the lock and a
// transition that no longer applies writes nothing. The transitions that move
// the members of a group found by a query (FailWakes, ExpireRetirements) read
// the candidates first, lock them in id order, and re-check the whole
// condition in the UPDATE, so a candidate that changed meanwhile is skipped.

const (
	// sqlMarkWakingHead moves the offline, enabled members of a group that
	// are awake, asleep or wake-failed to waking. Binds: the time, updated_at,
	// group, then the IN list the caller appends with sqlInIDsReturningID.
	sqlMarkWakingHead = `
UPDATE workers SET power_state = 'waking', power_state_at = ?, power_reason = '', updated_at = ?
WHERE capacity_group_id = ? AND status = 'offline' AND NOT disabled
  AND power_state IN ('awake', 'asleep', 'wake_failed')
  AND id IN (`

	// sqlListOverdueWakes lists the members of a group waking since before
	// the cutoff. Binds: group, cutoff.
	sqlListOverdueWakes = `
SELECT id FROM workers
WHERE capacity_group_id = ? AND power_state = 'waking' AND power_state_at < ?`

	// sqlFailWakesHead is sqlListOverdueWakes' condition as an UPDATE to
	// wake_failed. Binds: the time, the reason, updated_at, group, cutoff,
	// then the IN list.
	sqlFailWakesHead = `
UPDATE workers SET power_state = 'wake_failed', power_state_at = ?, power_reason = ?, updated_at = ?
WHERE capacity_group_id = ? AND power_state = 'waking' AND power_state_at < ?
  AND id IN (`

	// sqlListOverdueRetirements lists the members of a group retiring since
	// before the cutoff. Binds: group, cutoff.
	sqlListOverdueRetirements = `
SELECT id FROM workers
WHERE capacity_group_id = ? AND power_state = 'retiring' AND power_state_at < ?`

	// sqlExpireRetirementsHead is sqlListOverdueRetirements' condition as an
	// UPDATE back to awake. Binds: the time, the reason, updated_at, group,
	// cutoff, then the IN list.
	sqlExpireRetirementsHead = `
UPDATE workers SET power_state = 'awake', power_state_at = ?, power_reason = ?, updated_at = ?
WHERE capacity_group_id = ? AND power_state = 'retiring' AND power_state_at < ?
  AND id IN (`

	// sqlBeginRetirement moves an idle, online, enabled, awake member of a
	// group to retiring. Its in-flight condition is DeleteEphemeralWorker's.
	// Binds: the time, updated_at, worker, group.
	sqlBeginRetirement = `
UPDATE workers SET power_state = 'retiring', power_state_at = ?, power_reason = '', updated_at = ?
WHERE id = ? AND capacity_group_id = ? AND status = 'online' AND NOT disabled AND power_state = 'awake'
  AND NOT ` + sqlWorkerHasWorkInFlight + `
RETURNING ` + workerCols

	// sqlFinishRetirement puts a retiring worker that holds no task to sleep
	// and offline. Binds: the time, updated_at, worker.
	sqlFinishRetirement = `
UPDATE workers SET status = 'offline', power_state = 'asleep', power_state_at = ?, power_reason = '', updated_at = ?
WHERE id = ? AND power_state = 'retiring' AND NOT ` + sqlWorkerHasWorkInFlight + `
RETURNING ` + workerCols

	// sqlAbortRetirement moves a retiring worker back to awake. Binds: the
	// time, the reason, updated_at, worker.
	sqlAbortRetirement = `
UPDATE workers SET power_state = 'awake', power_state_at = ?, power_reason = ?, updated_at = ?
WHERE id = ? AND power_state = 'retiring'`
)

// sqlInIDsReturningID closes the IN list of a transition head and returns the
// ids it moved.
const sqlInIDsReturningID = `)
RETURNING id`

// sqlInIDsReturningWorker closes the IN list of a transition head and returns
// the rows it moved.
const sqlInIDsReturningWorker = `)
RETURNING ` + workerCols

// lockWorkerAnchorsTx locks the worker rows ids, sorted and without
// duplicates, in id order, and returns them in that order.
func lockWorkerAnchorsTx(ctx context.Context, tx *dbTx, ids []string) ([]string, error) {
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	anchors := make([]Anchor, len(ids))
	for i, id := range ids {
		anchors[i] = workerAnchor(id)
	}
	if err := tx.lockAnchors(ctx, anchors...); err != nil {
		return nil, err
	}
	return ids, nil
}

// idArgs returns ids as bind arguments, after head.
func idArgs(ids []string, head ...any) []any {
	args := make([]any, 0, len(head)+len(ids))
	args = append(args, head...)
	for _, id := range ids {
		args = append(args, id)
	}
	return args
}

// queryIDsTx runs query in tx and returns the single id column of its rows.
func queryIDsTx(ctx context.Context, tx *dbTx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, mapErr(err)
		}
		ids = append(ids, id)
	}
	return ids, mapErr(rows.Err())
}

// MarkWaking implements [store.CapacityPowerStore].
//
// Anchors: each named worker, in id order.
func (s *Store) MarkWaking(ctx context.Context, groupID string, workerIDs []string, now time.Time) ([]string, error) {
	at := TimeText(now.UTC())
	return inTxValue(ctx, s, "mark workers waking", func(tx *dbTx) ([]string, txOutcome, error) {
		ids, err := lockWorkerAnchorsTx(ctx, tx, workerIDs)
		if err != nil {
			return nil, txRollback, err
		}
		if len(ids) == 0 {
			// Nothing to move. Committing runs the fence check, so a deposed
			// leader still gets ErrFenced.
			return nil, txCommit, nil
		}
		placeholders := qmarks(len(ids))
		args := idArgs(ids, at, at, groupID)
		moved, err := queryIDsTx(ctx, tx, sqlMarkWakingHead+placeholders+sqlInIDsReturningID, args...)
		if err != nil {
			return nil, txRollback, fmt.Errorf("store: mark workers waking in group %s: %w", groupID, err)
		}
		slices.Sort(moved)
		return moved, txCommit, nil
	})
}

// FailWakes implements [store.CapacityPowerStore].
//
// Anchors: each overdue member, in id order.
func (s *Store) FailWakes(ctx context.Context, groupID string, cutoff, now time.Time, reason string) ([]store.Worker, error) {
	at, before := TimeText(now.UTC()), TimeText(cutoff.UTC())
	return inTxValue(ctx, s, "fail wakes", func(tx *dbTx) ([]store.Worker, txOutcome, error) {
		ids, err := lockOverdueTx(ctx, tx, sqlListOverdueWakes, groupID, before)
		if err != nil || len(ids) == 0 {
			return nil, txOutcomeFor(err), err
		}
		placeholders := qmarks(len(ids))
		args := idArgs(ids, at, reason, at, groupID, before)
		rows, err := tx.QueryContext(ctx, sqlFailWakesHead+placeholders+sqlInIDsReturningWorker, args...)
		if err != nil {
			return nil, txRollback, fmt.Errorf("store: fail wakes in group %s: %w", groupID, mapErr(err))
		}
		defer rows.Close()
		var failed []store.Worker
		for rows.Next() {
			w, err := scanWorker(rows)
			if err != nil {
				return nil, txRollback, mapErr(err)
			}
			failed = append(failed, w)
		}
		if err := rows.Err(); err != nil {
			return nil, txRollback, mapErr(err)
		}
		slices.SortFunc(failed, func(a, b store.Worker) int { return cmp.Compare(a.ID, b.ID) })
		return failed, txCommit, nil
	})
}

// ExpireRetirements implements [store.CapacityPowerStore].
//
// Anchors: each overdue member, in id order.
func (s *Store) ExpireRetirements(ctx context.Context, groupID string, cutoff, now time.Time, reason string) ([]string, error) {
	at, before := TimeText(now.UTC()), TimeText(cutoff.UTC())
	return inTxValue(ctx, s, "expire retirements", func(tx *dbTx) ([]string, txOutcome, error) {
		ids, err := lockOverdueTx(ctx, tx, sqlListOverdueRetirements, groupID, before)
		if err != nil || len(ids) == 0 {
			return nil, txOutcomeFor(err), err
		}
		placeholders := qmarks(len(ids))
		args := idArgs(ids, at, reason, at, groupID, before)
		expired, err := queryIDsTx(ctx, tx, sqlExpireRetirementsHead+placeholders+sqlInIDsReturningID, args...)
		if err != nil {
			return nil, txRollback, fmt.Errorf("store: expire retirements in group %s: %w", groupID, err)
		}
		slices.Sort(expired)
		return expired, txCommit, nil
	})
}

// lockOverdueTx reads the ids query lists for groupID and cutoff and locks
// their rows in id order. The caller's UPDATE re-checks the query's condition
// under the locks.
func lockOverdueTx(ctx context.Context, tx *dbTx, query, groupID, cutoff string) ([]string, error) {
	ids, err := queryIDsTx(ctx, tx, query, groupID, cutoff)
	if err != nil {
		return nil, fmt.Errorf("store: list overdue members of group %s: %w", groupID, err)
	}
	return lockWorkerAnchorsTx(ctx, tx, ids)
}

// txOutcomeFor is the outcome of a transaction body that ends with err: a
// rollback on an error, else a commit (which runs the fence check, so a body
// that found nothing to write still fences a deposed leader).
func txOutcomeFor(err error) txOutcome {
	if err != nil {
		return txRollback
	}
	return txCommit
}

// BeginRetirement implements [store.CapacityPowerStore].
//
// Anchor: the worker row, exclusive. LeaseTask holds it shared and re-checks
// the power state under it, so the two serialize on it: a lease that committed
// first holds a task the in-flight condition sees, and one that waits sees the
// worker retiring and is lost.
func (s *Store) BeginRetirement(ctx context.Context, groupID, workerID string, now time.Time) (store.Worker, bool, error) {
	at := TimeText(now.UTC())
	type result struct {
		worker store.Worker
		ok     bool
	}
	r, err := inTxValue(ctx, s, "begin retirement", func(tx *dbTx) (result, txOutcome, error) {
		if err := tx.lockAnchors(ctx, workerAnchor(workerID)); err != nil {
			return result{}, txRollback, err
		}
		w, err := scanWorker(tx.QueryRowContext(ctx, sqlBeginRetirement, at, at, workerID, groupID))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return result{}, txCommit, nil // refused; the commit runs the fence check
		case err != nil:
			return result{}, txRollback, fmt.Errorf("store: begin retirement of worker %s: %w", workerID, mapErr(err))
		}
		return result{worker: w, ok: true}, txCommit, nil
	})
	return r.worker, r.ok, err
}

// FinishRetirement implements [store.CapacityPowerStore].
//
// Anchor: the worker row. An ephemeral member is first put to sleep and
// offline like a named one, which makes it match DeleteEphemeralWorker's
// rule, and is then deleted with its credential revoked by the same statement
// and helper DeleteEphemeralWorker uses.
func (s *Store) FinishRetirement(
	ctx context.Context, workerID string, now time.Time,
) (w store.Worker, deleted, ok bool, err error) {
	at := TimeText(now.UTC())
	type result struct {
		worker      store.Worker
		deleted, ok bool
	}
	r, err := inTxValue(ctx, s, "finish retirement", func(tx *dbTx) (result, txOutcome, error) {
		if err := tx.lockAnchors(ctx, workerAnchor(workerID)); err != nil {
			return result{}, txRollback, err
		}
		current, err := scanWorker(tx.QueryRowContext(ctx, sqlGetWorker, workerID))
		if errors.Is(err, sql.ErrNoRows) || err == nil && current.PowerState != store.PowerRetiring {
			return result{}, txCommit, nil // not retiring; the commit runs the fence check
		}
		if err != nil {
			return result{}, txRollback, fmt.Errorf("store: read worker %s: %w", workerID, mapErr(err))
		}
		asleep, err := scanWorker(tx.QueryRowContext(ctx, sqlFinishRetirement, at, at, workerID))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// Retiring under the lock, so the in-flight condition refused it.
			return result{}, txRollback, fmt.Errorf("store: finish retirement of worker %s, which holds a task: %w",
				workerID, store.ErrConflict)
		case err != nil:
			return result{}, txRollback, fmt.Errorf("store: finish retirement of worker %s: %w", workerID, mapErr(err))
		}
		if !asleep.Ephemeral {
			return result{worker: asleep, ok: true}, txCommit, nil
		}
		gone, deleted, err := deleteEphemeralWorkerTx(ctx, tx, workerID, now, sqlDeleteEphemeralWorker, workerID)
		if err != nil {
			return result{}, txRollback, err
		}
		if !deleted {
			return result{}, txRollback, fmt.Errorf("store: retired ephemeral worker %s was not deleted: %w",
				workerID, store.ErrConflict)
		}
		return result{worker: gone, deleted: true, ok: true}, txCommit, nil
	})
	return r.worker, r.deleted, r.ok, err
}

// AbortRetirement implements [store.CapacityPowerStore].
//
// Anchor: the worker row.
func (s *Store) AbortRetirement(ctx context.Context, workerID string, now time.Time, reason string) (bool, error) {
	at := TimeText(now.UTC())
	return inTxValue(ctx, s, "abort retirement", func(tx *dbTx) (bool, txOutcome, error) {
		if err := tx.lockAnchors(ctx, workerAnchor(workerID)); err != nil {
			return false, txRollback, err
		}
		res, err := tx.ExecContext(ctx, sqlAbortRetirement, at, reason, at, workerID)
		if err != nil {
			return false, txRollback, fmt.Errorf("store: abort retirement of worker %s: %w", workerID, mapErr(err))
		}
		n, err := res.RowsAffected()
		if err != nil {
			return false, txRollback, mapErr(err)
		}
		return n > 0, txCommit, nil
	})
}

// ── Capacity group state ──────────────────────────────────────────────────────

const (
	sqlCapacityGroupStateCols = `group_id, requested, requested_at, degraded_reason, degraded_at, backoff_until, updated_at`

	sqlListCapacityGroupStates = `SELECT ` + sqlCapacityGroupStateCols + ` FROM capacity_group_state`

	// sqlPutCapacityGroupState inserts or replaces a group's state.
	sqlPutCapacityGroupState = `
INSERT INTO capacity_group_state (` + sqlCapacityGroupStateCols + `)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (group_id) DO UPDATE SET
	requested       = excluded.requested,
	requested_at    = excluded.requested_at,
	degraded_reason = excluded.degraded_reason,
	degraded_at     = excluded.degraded_at,
	backoff_until   = excluded.backoff_until,
	updated_at      = excluded.updated_at`
)

// ListCapacityGroupStates implements [store.CapacityPowerStore].
func (s *Store) ListCapacityGroupStates(ctx context.Context) (map[string]store.CapacityGroupState, error) {
	rows, err := s.r.QueryContext(ctx, sqlListCapacityGroupStates)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	states := map[string]store.CapacityGroupState{}
	for rows.Next() {
		var st store.CapacityGroupState
		var requestedAt, degradedAt, backoffUntil sql.NullString
		var updatedAt string
		if err := rows.Scan(&st.GroupID, &st.Requested, &requestedAt, &st.DegradedReason,
			&degradedAt, &backoffUntil, &updatedAt); err != nil {
			return nil, mapErr(err)
		}
		st.RequestedAt = nullTextToTime(requestedAt)
		st.DegradedAt = nullTextToTime(degradedAt)
		st.BackoffUntil = nullTextToTime(backoffUntil)
		st.UpdatedAt = mustTime(updatedAt)
		states[st.GroupID] = st
	}
	return states, mapErr(rows.Err())
}

// PutCapacityGroupState implements [store.CapacityPowerStore]. An unknown
// group is found by a read before the write, so a leadership context's fence
// check still runs for it. A group deleted between the read and the write is
// refused by the foreign key, which is reported as the group not existing too.
func (s *Store) PutCapacityGroupState(ctx context.Context, st store.CapacityGroupState) error {
	updated := st.UpdatedAt
	if updated.IsZero() {
		updated = time.Now()
	}
	var unknown bool
	err := s.write(ctx, "put capacity group state", func(w writer) error {
		unknown = false
		var one int
		switch err := w.QueryRowContext(ctx, sqlGetCapacityGroupExists, st.GroupID).Scan(&one); {
		case errors.Is(err, sql.ErrNoRows):
			unknown = true
			return nil
		case err != nil:
			return mapErr(err)
		}
		_, err := w.ExecContext(ctx, sqlPutCapacityGroupState,
			st.GroupID, st.Requested, nullTimeToText(st.RequestedAt), st.DegradedReason,
			nullTimeToText(st.DegradedAt), nullTimeToText(st.BackoffUntil), TimeText(updated))
		return mapErr(err)
	})
	switch {
	case err != nil && !errors.Is(err, store.ErrConflict):
		return err
	case err != nil || unknown:
		return fmt.Errorf("store: put state of capacity group %s: %w", st.GroupID, store.ErrNotFound)
	}
	return nil
}
