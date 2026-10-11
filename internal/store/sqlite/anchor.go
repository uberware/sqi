// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

// The anchor constructors name the rows an invariant-guarding transaction must
// hold before it reads the other rows its decision depends on, and pass them
// to [Dialect.LockAnchors]. Every anchor is exclusive ([LockUpdate]) except
// the upstream job rows of the blocked-job operations and of a submission's
// dependency check, which only need a shared lock ([sharedAnchor]). Callers
// pass anchors in the order the anchor-row table specifies, so every backend
// agrees on lock order. See "Store invariants" in docs/architecture.md for the
// full anchor-row table and the gaps it leaves open.
//
// On SQLite LockAnchors does nothing: the store's single write connection
// (SetMaxOpenConns(1)) already serializes every write transaction, so a row
// lock would add nothing.

func jobAnchor(id string) Anchor    { return Anchor{Table: "jobs", ID: id, Mode: LockUpdate} }
func queueAnchor(id string) Anchor  { return Anchor{Table: "queues", ID: id, Mode: LockUpdate} }
func farmAnchor(id string) Anchor   { return Anchor{Table: "farms", ID: id, Mode: LockUpdate} }
func poolAnchor(id string) Anchor   { return Anchor{Table: "usage_pools", ID: id, Mode: LockUpdate} }
func workerAnchor(id string) Anchor { return Anchor{Table: "workers", ID: id, Mode: LockUpdate} }
func userAnchor(id string) Anchor   { return Anchor{Table: "users", ID: id, Mode: LockUpdate} }

// sharedAnchor returns a with a shared lock: the row must not change, but
// other transactions may read-lock it too.
func sharedAnchor(a Anchor) Anchor { a.Mode = LockShare; return a }
