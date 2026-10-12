// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Dialect is what differs between the SQL databases the store runs on. The
// store writes every statement with "?" placeholders and binds Go values; the
// dialect rewrites, converts and classifies at the boundary.
type Dialect interface {
	// Rebind rewrites "?" placeholders into the driver's form.
	Rebind(query string) string
	// Classify reports what an error from the driver means.
	Classify(err error) ErrClass
	// LockAnchors locks each anchor row, in the order given, in tx.
	LockAnchors(ctx context.Context, tx *sql.Tx, anchors ...Anchor) error
	// BindTime and BindBool return the value a Go time or bool is bound as.
	BindTime(t time.Time) any
	BindBool(b bool) any
	// CIEqual returns "<col> = ?" compared case-insensitively.
	CIEqual(col string) string
	// CILike returns "<col> LIKE ?" matched case-insensitively, in the dialect's
	// own form (PostgreSQL: "<col> ILIKE ? ESCAPE ''"). Like CIEqual it may carry
	// only the column, a "?" and literals.
	CILike(col string) string
}

// ErrClass is what a driver error means to the store.
type ErrClass int

// The error classes. ErrClassRetry marks a transaction the database aborted
// for a concurrent writer; it is safe to run again.
const (
	ErrClassOther ErrClass = iota
	ErrClassNotFound
	ErrClassConflict
	ErrClassRetry
)

// LockMode is how LockAnchors locks a row.
type LockMode int

// LockUpdate excludes other writers of the row; LockShare only keeps the row
// from changing (an upstream job a dependent waits on). LockNoKeyUpdate
// (PostgreSQL's FOR NO KEY UPDATE) excludes other writers too: it conflicts
// with itself, with LockUpdate and LockShare, and with an UPDATE or DELETE of
// the row. It does not conflict with the FOR KEY SHARE lock a foreign-key
// check takes on a referenced row, so inserting a row that references the
// anchor (a job referencing its queue and farm, a claim referencing its pool)
// does not wait for it, nor it for the insert. SQLite ignores every mode.
const (
	LockUpdate LockMode = iota
	LockShare
	LockNoKeyUpdate
)

// Anchor is a row a transaction locks before it writes, per the anchor-row
// table in docs/architecture.md.
type Anchor struct {
	Table string
	ID    string
	Mode  LockMode
}

// classifiedError carries the class the dialect gave a driver error.
type classifiedError struct {
	class ErrClass
	err   error
}

func (e *classifiedError) Error() string { return e.err.Error() }
func (e *classifiedError) Unwrap() error { return e.err }

// classifyErr tags err with its class; nil stays nil.
func classifyErr(d Dialect, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*classifiedError](err); ok {
		return err
	}
	return &classifiedError{class: d.Classify(err), err: err}
}

// errClass returns the class a wrapper gave err, or ErrClassOther.
func errClass(err error) ErrClass {
	if ce, ok := errors.AsType[*classifiedError](err); ok {
		return ce.class
	}
	return ErrClassOther
}

// bindArgs converts Go time and bool arguments through the dialect.
func bindArgs(d Dialect, args []any) []any {
	out := make([]any, len(args))
	for i, a := range args {
		switch v := a.(type) {
		case time.Time:
			out[i] = d.BindTime(v)
		case *time.Time:
			if v == nil {
				out[i] = nil
			} else {
				out[i] = d.BindTime(*v)
			}
		case bool:
			out[i] = d.BindBool(v)
		default:
			out[i] = a
		}
	}
	return out
}

// dbRow is a single-row result whose Scan error is classified.
type dbRow struct {
	row *sql.Row
	d   Dialect
}

func (r *dbRow) Scan(dest ...any) error { return classifyErr(r.d, r.row.Scan(dest...)) }

// querier is the statement half of *sql.DB and *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// dbConn runs statements on a pool or in a transaction through the dialect:
// every query is rebound, every time and bool argument converted, every error
// classified.
type dbConn struct {
	q querier
	d Dialect
}

func (c *dbConn) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	res, err := c.q.ExecContext(ctx, c.d.Rebind(q), bindArgs(c.d, args)...)
	return res, classifyErr(c.d, err)
}

func (c *dbConn) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	rows, err := c.q.QueryContext(ctx, c.d.Rebind(q), bindArgs(c.d, args)...)
	return rows, classifyErr(c.d, err)
}

func (c *dbConn) QueryRowContext(ctx context.Context, q string, args ...any) *dbRow {
	return &dbRow{row: c.q.QueryRowContext(ctx, c.d.Rebind(q), bindArgs(c.d, args)...), d: c.d}
}

// dbPool runs statements on a connection pool through the dialect. db is the
// raw pool, for what the wrapper does not cover: Prepare, BeginTx and Close.
type dbPool struct {
	dbConn

	db *sql.DB
}

func newDBPool(db *sql.DB, d Dialect) *dbPool {
	return &dbPool{dbConn: dbConn{q: db, d: d}, db: db}
}

// dbTx runs statements in a transaction through the dialect. tx is the raw
// transaction, for Stmt and the anchor locks.
type dbTx struct {
	dbConn

	tx *sql.Tx
}

func newDBTx(tx *sql.Tx, d Dialect) *dbTx {
	return &dbTx{dbConn: dbConn{q: tx, d: d}, tx: tx}
}

// Stmt returns stmt bound to this transaction.
func (t *dbTx) Stmt(ctx context.Context, stmt *dbStmt) *dbStmt {
	return &dbStmt{st: t.tx.StmtContext(ctx, stmt.st), d: t.d}
}

// lockAnchors locks each anchor row, in the order given, in this transaction.
func (t *dbTx) lockAnchors(ctx context.Context, anchors ...Anchor) error {
	return t.d.LockAnchors(ctx, t.tx, anchors...)
}

// dbStmt is a prepared statement whose arguments and errors go through the
// dialect. Its query was rebound when it was prepared.
type dbStmt struct {
	st *sql.Stmt
	d  Dialect
}

func (s *dbStmt) ExecContext(ctx context.Context, args ...any) (sql.Result, error) {
	res, err := s.st.ExecContext(ctx, bindArgs(s.d, args)...)
	return res, classifyErr(s.d, err)
}

func (s *dbStmt) QueryContext(ctx context.Context, args ...any) (*sql.Rows, error) {
	rows, err := s.st.QueryContext(ctx, bindArgs(s.d, args)...)
	return rows, classifyErr(s.d, err)
}

func (s *dbStmt) QueryRowContext(ctx context.Context, args ...any) *dbRow {
	return &dbRow{row: s.st.QueryRowContext(ctx, bindArgs(s.d, args)...), d: s.d}
}

// Close closes the statement.
func (s *dbStmt) Close() error { return s.st.Close() }
