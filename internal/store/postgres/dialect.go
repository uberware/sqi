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

// anchorTables are the tables an anchor may name. lockSQL interpolates the
// table name, so it must come from here.
var anchorTables = map[string]bool{
	"jobs": true, "queues": true, "farms": true, "usage_pools": true, "workers": true, "users": true,
}

// lockSQL returns the statement that locks one anchor row.
func lockSQL(a sqlstore.Anchor) (string, error) {
	if !anchorTables[a.Table] {
		return "", fmt.Errorf("postgres: anchor table %q is not allowed", a.Table)
	}
	mode := "FOR UPDATE"
	if a.Mode == sqlstore.LockShare {
		mode = "FOR SHARE"
	}
	return "SELECT 1 FROM " + a.Table + " WHERE id = $1 " + mode, nil
}

// LockAnchors locks each anchor row in the order given. A row that does not
// exist locks nothing and is not an error: the operation's own guarded
// statements decide what its absence means, as they do on SQLite.
func (pgDialect) LockAnchors(ctx context.Context, tx *sql.Tx, anchors ...sqlstore.Anchor) error {
	for _, a := range anchors {
		if err := lockRow(ctx, tx, a); err != nil {
			return err
		}
	}
	return nil
}

// lockRow locks one anchor row. A lock wait that ends in a deadlock surfaces
// from the query, from reading the result, or from closing it, so all three
// are checked; each is wrapped with %w, so the *pgconn.PgError stays reachable
// for Classify.
func lockRow(ctx context.Context, tx *sql.Tx, a sqlstore.Anchor) (err error) {
	q, err := lockSQL(a)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, q, a.ID)
	if err != nil {
		return fmt.Errorf("postgres: lock %s %s: %w", a.Table, a.ID, err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("postgres: lock %s %s: %w", a.Table, a.ID, cerr)
		}
	}()
	for rows.Next() { // id is unique: at most the one row, locked once fetched
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("postgres: lock %s %s: %w", a.Table, a.ID, err)
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
