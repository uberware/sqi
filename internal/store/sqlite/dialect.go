// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

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
	// CILike returns "<col> LIKE ?" matched case-insensitively.
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
// from changing (an upstream job a dependent waits on).
const (
	LockUpdate LockMode = iota
	LockShare
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

// dbPool runs statements on a connection pool through the dialect.
type dbPool struct {
	db *sql.DB
	d  Dialect
}

func (p *dbPool) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	res, err := p.db.ExecContext(ctx, p.d.Rebind(q), bindArgs(p.d, args)...)
	return res, classifyErr(p.d, err)
}

func (p *dbPool) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	rows, err := p.db.QueryContext(ctx, p.d.Rebind(q), bindArgs(p.d, args)...)
	return rows, classifyErr(p.d, err)
}

func (p *dbPool) QueryRowContext(ctx context.Context, q string, args ...any) *dbRow {
	return &dbRow{row: p.db.QueryRowContext(ctx, p.d.Rebind(q), bindArgs(p.d, args)...), d: p.d}
}

// dbTx runs statements in a transaction through the dialect.
type dbTx struct {
	tx *sql.Tx
	d  Dialect
}

func (t *dbTx) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	res, err := t.tx.ExecContext(ctx, t.d.Rebind(q), bindArgs(t.d, args)...)
	return res, classifyErr(t.d, err)
}

func (t *dbTx) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	rows, err := t.tx.QueryContext(ctx, t.d.Rebind(q), bindArgs(t.d, args)...)
	return rows, classifyErr(t.d, err)
}

func (t *dbTx) QueryRowContext(ctx context.Context, q string, args ...any) *dbRow {
	return &dbRow{row: t.tx.QueryRowContext(ctx, t.d.Rebind(q), bindArgs(t.d, args)...), d: t.d}
}

// Stmt returns stmt bound to this transaction.
func (t *dbTx) Stmt(ctx context.Context, stmt *dbStmt) *dbStmt {
	return &dbStmt{st: t.tx.StmtContext(ctx, stmt.st), d: t.d}
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
