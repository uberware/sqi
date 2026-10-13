// SPDX-License-Identifier: AGPL-3.0-or-later

// Package postgres is the PostgreSQL state store: the shared SQL store in
// internal/store/sqlstore over pgx, with the locking and retry a database
// with concurrent writers needs.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/uberware/sqi/internal/store/sqlstore"
)

// pgDialect is the PostgreSQL dialect. Timestamps bind as the store's
// 30-character text (the columns are TEXT, see migrations/postgres) and
// booleans as booleans.
type pgDialect struct{}

var _ sqlstore.Dialect = pgDialect{}

// Rebind rewrites each "?" as $1, $2, …. No SQL constant puts a "?" inside a
// quoted literal or a comment (TestSQLConstants_NoQuestionMarkInLiteralsOrComments).
func (pgDialect) Rebind(query string) string {
	if !strings.Contains(query, "?") {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Classify maps SQLSTATE codes: unique and foreign-key violations are
// conflicts; serialization failures and deadlocks mean the transaction was
// aborted for a concurrent writer and is safe to run again.
func (pgDialect) Classify(err error) sqlstore.ErrClass {
	if errors.Is(err, sql.ErrNoRows) {
		return sqlstore.ErrClassNotFound
	}
	if pe, ok := errors.AsType[*pgconn.PgError](err); ok {
		switch pe.Code {
		case "23505", "23503":
			return sqlstore.ErrClassConflict
		case "40001", "40P01":
			return sqlstore.ErrClassRetry
		}
	}
	return sqlstore.ErrClassOther
}

// taskInsertBatchRows is how many tasks a submission inserts per statement. A
// task per statement costs a round trip each, which dominates a large
// submission; 500 rows bind 7,500 parameters, well inside PostgreSQL's 65,535
// per statement.
const taskInsertBatchRows = 500

// anchorTables are the tables an anchor may name. lockSQL interpolates the
// table name, so it must come from here.
var anchorTables = map[string]bool{
	"jobs": true, "queues": true, "farms": true, "usage_pools": true, "workers": true, "users": true,
}

// lockSQL returns the statement that locks a's row or, with many set, the rows
// of a's table and mode whose ids are in the array bound to $1, in id order.
func lockSQL(a sqlstore.Anchor, many bool) (string, error) {
	if !anchorTables[a.Table] {
		return "", fmt.Errorf("postgres: anchor table %q is not allowed", a.Table)
	}
	mode := "FOR UPDATE"
	switch a.Mode {
	case sqlstore.LockShare:
		mode = "FOR SHARE"
	case sqlstore.LockNoKeyUpdate:
		mode = "FOR NO KEY UPDATE"
	}
	if many {
		return "SELECT 1 FROM " + a.Table + " WHERE id = ANY($1) ORDER BY id " + mode, nil
	}
	return "SELECT 1 FROM " + a.Table + " WHERE id = $1 " + mode, nil
}

// LockAnchors locks each anchor row in the order given. A row that does not
// exist locks nothing and is not an error: the operation's own guarded
// statements decide what its absence means, as they do on SQLite.
//
// A run of consecutive anchors on one table in one mode is locked by one
// statement rather than one per row: the store passes such a run in id order
// (the global lock order), and the statement's ORDER BY id feeds its lock step,
// so the rows are locked in that same order, in one round trip.
func (pgDialect) LockAnchors(ctx context.Context, tx *sql.Tx, anchors ...sqlstore.Anchor) error {
	for len(anchors) > 0 {
		n := 1
		for n < len(anchors) && anchors[n].Table == anchors[0].Table && anchors[n].Mode == anchors[0].Mode {
			n++
		}
		if err := lockRows(ctx, tx, anchors[:n]); err != nil {
			return err
		}
		anchors = anchors[n:]
	}
	return nil
}

// lockRows locks run, anchors on one table in one mode, with one statement. A
// lock wait that ends in a deadlock surfaces from the query, from reading the
// result, or from closing it, so all three are checked; each is wrapped with
// %w, so the *pgconn.PgError stays reachable for Classify.
func lockRows(ctx context.Context, tx *sql.Tx, run []sqlstore.Anchor) (err error) {
	a := run[0]
	q, err := lockSQL(a, len(run) > 1)
	if err != nil {
		return err
	}
	var arg any = a.ID
	what := a.ID
	if len(run) > 1 {
		ids := make([]string, len(run))
		for i, r := range run {
			ids[i] = r.ID
		}
		arg, what = ids, strings.Join(ids, ",")
	}
	rows, err := tx.QueryContext(ctx, q, arg)
	if err != nil {
		return fmt.Errorf("postgres: lock %s %s: %w", a.Table, what, err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("postgres: lock %s %s: %w", a.Table, what, cerr)
		}
	}()
	for rows.Next() { // each row is locked once fetched
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("postgres: lock %s %s: %w", a.Table, what, err)
	}
	return nil
}

// BindTime binds the 30-character text the timestamp columns hold.
func (pgDialect) BindTime(t time.Time) any { return sqlstore.TimeText(t) }

// BindBool binds a boolean.
func (pgDialect) BindBool(b bool) any { return b }

// CIEqual compares lower() to lower(). The columns are COLLATE "C", so lower()
// folds ASCII only, as SQLite's NOCASE does, and the lower(col) unique
// indexes serve it.
func (pgDialect) CIEqual(col string) string { return "lower(" + col + ") = lower(?)" }

// CILike matches case-insensitively with backslash taken literally: SQLite's
// LIKE has no escape character, and a search for a Windows path must match.
func (pgDialect) CILike(col string) string { return col + " ILIKE ? ESCAPE ''" }

// ForUpdate locks the rows a SELECT reads until the transaction ends.
func (pgDialect) ForUpdate() string { return " FOR UPDATE" }
