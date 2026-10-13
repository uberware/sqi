// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

import (
	"errors"
	"fmt"
)

// The anchor constructors name the rows an invariant-guarding transaction must
// hold before it reads the other rows its decision depends on, and pass them
// to [Dialect.LockAnchors]. Job, worker and user anchors are exclusive
// ([LockUpdate]) except the lease's worker and job rows and the upstream job
// rows of the blocked-job operations and of a submission's dependency check,
// which only need a shared lock ([sharedAnchor]). Queue, farm and pool anchors
// are [LockNoKeyUpdate] (see queueAnchor). Callers pass anchors in the global
// lock order (worker, jobs by id, queue, farm, usage pools by id, users by id),
// which [lockOrder.check] enforces. See "Store invariants" in
// docs/architecture.md for the full anchor-row table, the lock order and the
// races the anchors leave open.
//
// On SQLite LockAnchors does nothing: the store's single write connection
// (SetMaxOpenConns(1)) already serializes every write transaction, so a row
// lock would add nothing.

func jobAnchor(id string) Anchor { return Anchor{Table: "jobs", ID: id, Mode: LockUpdate} }

// The capacity anchors a lease takes (a capped queue and farm, each requested
// pool) are LockNoKeyUpdate rather than LockUpdate. They still serialize two
// leases on the same row (the mode conflicts with itself) and a lease against
// a cap change (an UPDATE of the row), but not against an insert that only
// references the row: a submission's job row holds FOR KEY SHARE on its queue
// and farm until it commits, and with LockUpdate every lease on a capped queue
// or farm would wait for the whole of a large submission into it.
func queueAnchor(id string) Anchor  { return Anchor{Table: "queues", ID: id, Mode: LockNoKeyUpdate} }
func farmAnchor(id string) Anchor   { return Anchor{Table: "farms", ID: id, Mode: LockNoKeyUpdate} }
func poolAnchor(id string) Anchor   { return Anchor{Table: "usage_pools", ID: id, Mode: LockNoKeyUpdate} }
func workerAnchor(id string) Anchor { return Anchor{Table: "workers", ID: id, Mode: LockUpdate} }
func userAnchor(id string) Anchor   { return Anchor{Table: "users", ID: id, Mode: LockUpdate} }

// sharedAnchor returns a with a shared lock: the row must not change, but
// other transactions may read-lock it too.
func sharedAnchor(a Anchor) Anchor { a.Mode = LockShare; return a }

// lockRank is each anchor table's place in the global lock order
// (docs/architecture.md, "Anchor rows").
var lockRank = map[string]int{"workers": 0, "jobs": 1, "queues": 2, "farms": 3, "usage_pools": 4, "users": 5}

// errLockOrder marks an anchor taken out of the global lock order. It is a
// programming error: two transactions that lock the same rows in opposite
// orders can deadlock on PostgreSQL, so the lock is refused before it is taken.
//
// It is refused on SQLite too, where an inversion cannot deadlock, so that a
// call site out of order fails the SQLite tests, which run everywhere, and not
// only the PostgreSQL ones.
var errLockOrder = errors.New("store: anchor out of the global lock order")

// lockOrder is where one transaction stands in the global lock order: the rank
// of the last anchor it locked (-1 before the first) and, for each table and
// mode, the last id it locked there.
type lockOrder struct {
	rank   int
	lastID map[lockKey]string
}

type lockKey struct {
	table string
	mode  LockMode
}

func newLockOrder() lockOrder { return lockOrder{rank: -1, lastID: map[lockKey]string{}} }

// check checks anchors, about to be locked in that order, against the global
// lock order and o, and advances o past them: no table ranked below one the
// transaction already locked, and the rows of one table and mode in id order,
// across every call the transaction makes. (ReleaseBlockedJob and
// CancelBlockedJob lock their job exclusively, then its upstream jobs shared:
// the two modes are ordered separately.) It runs on every dialect, so SQLite,
// whose LockAnchors takes no lock, still checks every caller.
func (o *lockOrder) check(anchors []Anchor) error {
	for _, a := range anchors {
		r, ok := lockRank[a.Table]
		if !ok {
			return fmt.Errorf("%w: %s is not an anchor table", errLockOrder, a.Table)
		}
		if r < o.rank {
			return fmt.Errorf("%w: %s locked after a higher-ranked row: %v", errLockOrder, a.Table, anchors)
		}
		k := lockKey{a.Table, a.Mode}
		if last, seen := o.lastID[k]; seen && last > a.ID {
			return fmt.Errorf("%w: %s %s locked after %s: %v", errLockOrder, a.Table, a.ID, last, anchors)
		}
		o.rank, o.lastID[k] = r, a.ID
	}
	return nil
}
