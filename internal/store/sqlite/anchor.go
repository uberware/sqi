// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
)

// anchor names one row that an invariant-guarding transaction must hold before
// it reads the other rows its decision depends on: exclusively, except the
// upstream job rows of the blocked-job operations and of a submission's
// dependency check, which only need a shared lock. anchor carries no lock mode
// yet; those call sites say so in a comment. See "Store invariants" in
// docs/architecture.md for the full anchor-row table.
type anchor struct {
	table string
	id    string
}

func jobAnchor(id string) anchor    { return anchor{table: "jobs", id: id} }
func queueAnchor(id string) anchor  { return anchor{table: "queues", id: id} }
func farmAnchor(id string) anchor   { return anchor{table: "farms", id: id} }
func poolAnchor(id string) anchor   { return anchor{table: "usage_pools", id: id} }
func workerAnchor(id string) anchor { return anchor{table: "workers", id: id} }
func userAnchor(id string) anchor   { return anchor{table: "users", id: id} }

// lockAnchors is the hook point for invariants I3, I4 and I5. It deliberately
// does nothing on SQLite: the store's single write connection
// (SetMaxOpenConns(1)) already serializes every write transaction, so a row
// lock would add nothing. The PostgreSQL store (H4c) implements the same call
// as SELECT ... FOR UPDATE on each anchor, in the order given (FOR SHARE for
// the upstream job rows, which needs a lock mode this type does not carry
// yet). Callers pass anchors in the order the anchor-row table specifies, so
// the two backends agree on lock order. The table is not yet the whole Postgres
// story: docs/architecture.md lists the gaps it leaves open.
func lockAnchors(_ context.Context, _ *sql.Tx, _ ...anchor) error {
	return nil
}
