// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

// The anchor constructors name the rows an invariant-guarding transaction must
// hold before it reads the other rows its decision depends on, and pass them
// to [Dialect.LockAnchors]. Job, worker and user anchors are exclusive
// ([LockUpdate]) except the lease's worker and job rows and the upstream job
// rows of the blocked-job operations and of a submission's dependency check,
// which only need a shared lock ([sharedAnchor]). Queue, farm and pool anchors
// are [LockNoKeyUpdate] (see queueAnchor). Callers pass anchors in the global lock order (worker,
// jobs by id, queue, farm, usage pools by id, users by id). See "Store
// invariants" in docs/architecture.md for the full anchor-row table, the lock
// order and the races the anchors leave open.
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
