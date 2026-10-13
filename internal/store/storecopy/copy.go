// SPDX-License-Identifier: AGPL-3.0-or-later

// Package storecopy copies a stopped farm's SQLite database into an empty
// PostgreSQL database, verbatim, in one PostgreSQL transaction, and verifies
// every table by row count and digest before committing. See
// docs/operations.md, Moving to PostgreSQL.
//
// The schema is read from the live catalogs (schemainfo), not from a list kept
// here, so a new table or column is copied without a change to this package,
// and a column type it does not know how to copy is refused by name. SQL is
// built by concatenating identifiers read from those catalogs, each one
// double-quoted (quoteIdent doubles any embedded quote); every value travels
// as a bind parameter.
package storecopy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx"
	_ "modernc.org/sqlite"             // registers "sqlite"

	"github.com/uberware/sqi/internal/pgdsn"
	"github.com/uberware/sqi/internal/store/migrations"
	"github.com/uberware/sqi/internal/store/postgres"
	"github.com/uberware/sqi/internal/store/schemainfo"
	"github.com/uberware/sqi/internal/store/sqlite"
)

// Options says what to copy where.
type Options struct {
	SQLitePath  string // the source; never written
	PostgresDSN string // the target; empty, or migrated by sqi with no rows in its tables
	Logger      *slog.Logger
}

// TableResult is one table's copied and verified row count.
type TableResult struct {
	Table string
	Rows  int64
}

// Result is what a successful copy did, tables in the order they were copied.
type Result struct {
	Tables  []TableResult
	Elapsed time.Duration
}

const (
	// batchRows is the most rows one INSERT carries, as CreateJobSubmission.
	batchRows = 500
	// maxBindParams is PostgreSQL's limit on one statement's parameters; a
	// wide table gets fewer rows per INSERT so it stays below it.
	maxBindParams = 65535
)

// Copy checks every precondition of the copy (H5 design §7.2), then copies
// every table in one PostgreSQL transaction and verifies it before
// committing. Any failure after the preconditions rolls the transaction back,
// committing no sqi rows (a schema Copy migrated into an empty target stays).
func Copy(ctx context.Context, o Options) (Result, error) {
	start := time.Now()
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.PostgresDSN == "" {
		return Result{}, errors.New("store copy: no PostgreSQL DSN; use --to or set store.postgres.dsn")
	}
	// Checked before the lock, which would create <path>.lock beside a path
	// that holds no database.
	if err := requireExistingFile(o.SQLitePath); err != nil {
		return Result{}, err
	}
	srcLock, err := sqlite.AcquireLock(o.SQLitePath)
	if err != nil {
		return Result{}, fmt.Errorf("store copy: source: %w", err)
	}
	defer srcLock.Release() //nolint:errcheck // released on exit; the OS drops it with the process anyway
	dstLock, err := postgres.LockDatabase(ctx, o.PostgresDSN, o.Logger)
	if err != nil {
		return Result{}, fmt.Errorf("store copy: target %s: %w", pgdsn.Redact(o.PostgresDSN), err)
	}
	defer dstLock.Release() //nolint:errcheck // released on exit; PostgreSQL drops it with the session anyway

	src, err := openSource(ctx, o.SQLitePath)
	if err != nil {
		return Result{}, err
	}
	defer src.Close()
	dst, err := openTarget(ctx, o.PostgresDSN)
	if err != nil {
		return Result{}, err
	}
	defer dst.Close()

	plans, err := plan(ctx, src, dst)
	if err != nil {
		return Result{}, err
	}
	res := Result{}
	err = copyAll(ctx, src, dst, plans, o.Logger, &res)
	res.Elapsed = time.Since(start)
	return res, err
}

func requireExistingFile(path string) error {
	if path == "" {
		return errors.New("store copy: no SQLite database named; use --from or set store.sqlite_path")
	}
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("store copy: no SQLite database at %s; point --from or store.sqlite_path at the farm's database", path)
	}
	if err != nil {
		return fmt.Errorf("store copy: source %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("store copy: %s is not a file; point --from at the farm's SQLite database", path)
	}
	return nil
}

// openSource opens the SQLite database read-only and checks it is intact and
// at this binary's schema version.
//
// Read-only means the file is opened read-only (a mode=ro URI), not merely a
// connection that refuses SQL writes: SQLite writes the database file on its
// own account too — closing the last connection to a WAL database checkpoints
// the WAL into it, and opening one with a hot rollback journal rolls the
// journal back — and a crashed server leaves exactly such a database. A
// read-only connection reads the WAL's committed rows without moving them. It
// may create the -shm wal-index beside a WAL that has none (an index of the
// WAL, never data); a database it cannot read without writing is refused.
func openSource(ctx context.Context, path string) (*sql.DB, error) {
	dsn, err := readOnlyDSN(path)
	if err != nil {
		return nil, fmt.Errorf("store copy: source %s: %w", path, err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store copy: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := checkSource(ctx, db, path); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
}

// readOnlyDSN is a file: URI opening path read-only, escaped by
// [sqlite.FileURI] so any character in the name stays part of it.
func readOnlyDSN(path string) (string, error) {
	return sqlite.FileURI(path, true)
}

func checkSource(ctx context.Context, db *sql.DB, path string) error {
	var ok string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&ok); err != nil {
		return fmt.Errorf("store copy: %s cannot be read read-only: %w; it is not a SQLite database, or it needs "+
			"recovery a read-only reader cannot do (a crashed server's hot journal, or a -wal file whose directory "+
			"store copy cannot create the -shm index in): start sqi-server on it once and stop it cleanly, or "+
			"copy from a \"sqi-server backup\" of it, then run store copy again", path, err)
	}
	if ok != "ok" {
		return fmt.Errorf("store copy: %s failed SQLite's integrity check (%s); restore it from a backup first", path, ok)
	}
	v, err := sqlite.SchemaVersion(ctx, db)
	if err != nil {
		return fmt.Errorf("store copy: %w", err)
	}
	maxV, err := migrations.MaxVersion("sqlite")
	if err != nil {
		return err
	}
	switch {
	case v == 0:
		return fmt.Errorf("store copy: %s has no sqi schema; it is not an sqi database", path)
	case v > maxV:
		return fmt.Errorf("store copy: the SQLite database: %w", migrations.CheckVersion("sqlite", v))
	case v < maxV:
		return fmt.Errorf("store copy: the SQLite database is at schema %d and this sqi-server is at %d; "+
			"run \"sqi-server migrate up --db %s\" first", v, maxV, path)
	}
	return nil
}

// openTarget connects to the PostgreSQL database and brings it to this
// binary's schema: an empty database is migrated, one at this version is
// kept, and any other is refused. A database with no sqi schema that holds
// tables is refused before the migration writes anything into it.
func openTarget(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("store copy: target %s: %w", pgdsn.Redact(dsn), err)
	}
	db.SetMaxOpenConns(2)
	if err := prepareTarget(ctx, db); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
}

func prepareTarget(ctx context.Context, db *sql.DB) error {
	v, err := postgres.SchemaVersion(ctx, db)
	if err != nil {
		return fmt.Errorf("store copy: %w", err)
	}
	maxV, err := migrations.MaxVersion("postgres")
	if err != nil {
		return err
	}
	switch {
	case v == 0:
		tables, err := schemainfo.Postgres(ctx, db)
		if err != nil {
			return fmt.Errorf("store copy: %w", err)
		}
		if len(tables) > 0 {
			return fmt.Errorf("store copy: the PostgreSQL database has no sqi schema but holds tables sqi does not own (%s); "+
				"use an empty database", strings.Join(sortedKeys(tables), ", "))
		}
		p, err := migrations.NewProvider(db, "postgres")
		if err != nil {
			return fmt.Errorf("store copy: migrations: %w", err)
		}
		if _, err := p.Up(ctx); err != nil {
			return fmt.Errorf("store copy: migrate the PostgreSQL database: %w", err)
		}
	case v > maxV:
		return fmt.Errorf("store copy: the PostgreSQL database: %w", migrations.CheckVersion("postgres", v))
	case v < maxV:
		return fmt.Errorf("store copy: the PostgreSQL database is at schema %d, not %d; use an empty database "+
			"(if an earlier store copy's migration of it stopped partway, drop and recreate the database)", v, maxV)
	}
	return nil
}

// kind is how a column's values are carried from SQLite to PostgreSQL.
type kind int

const (
	kindText kind = iota // read through CAST(... AS TEXT), inserted as text
	kindInt              // read and inserted as int64
	kindBool             // read as SQLite's 0/1, inserted as a boolean
)

// tablePlan is everything the copy needs to move one table.
type tablePlan struct {
	name    string
	cols    []string // the target's column names, in its order
	kinds   []kind
	domains []string // each column's PostgreSQL domain, or ""
	pk      []int    // indexes into cols of the primary key, in key order

	srcSelect, dstSelect, insertPrefix string
	perInsert                          int // rows per INSERT
}

// plan reads both schemas, refuses any difference between them and any
// target row, and returns the tables in foreign-key order.
func plan(ctx context.Context, src, dst *sql.DB) ([]*tablePlan, error) {
	srcInfo, err := schemainfo.SQLite(ctx, src)
	if err != nil {
		return nil, fmt.Errorf("store copy: %w", err)
	}
	dstInfo, err := schemainfo.Postgres(ctx, dst)
	if err != nil {
		return nil, fmt.Errorf("store copy: %w", err)
	}
	if err := sameShape(srcInfo, dstInfo); err != nil {
		return nil, err
	}
	if err := requireEmpty(ctx, dst, dstInfo); err != nil {
		return nil, err
	}
	order, err := schemainfo.InsertOrder(dstInfo)
	if err != nil {
		return nil, fmt.Errorf("store copy: %w", err)
	}
	plans := make([]*tablePlan, 0, len(order))
	for _, n := range order {
		p, err := newTablePlan(dstInfo[n])
		if err != nil {
			return nil, err
		}
		plans = append(plans, p)
	}
	return plans, nil
}

// sameShape refuses unless both databases hold the same tables with the same
// column names. A table or column on one side only is named: a table sqi does
// not own in the target would otherwise sit beside the copy.
func sameShape(src, dst map[string]schemainfo.Table) error {
	var diffs []string
	for _, n := range sortedKeys(dst) {
		if _, ok := src[n]; !ok {
			diffs = append(diffs, "table "+n+" exists only in the PostgreSQL database")
		}
	}
	for _, n := range sortedKeys(src) {
		d, ok := dst[n]
		if !ok {
			diffs = append(diffs, "table "+n+" exists only in the SQLite database")
			continue
		}
		diffs = append(diffs, columnDiffs(n, src[n].Columns, d.Columns)...)
	}
	if len(diffs) > 0 {
		return fmt.Errorf("store copy: the two databases' schemas differ: %s; store copy needs the PostgreSQL "+
			"database to hold sqi's tables alone, at this binary's schema: use an empty database", strings.Join(diffs, "; "))
	}
	return nil
}

func columnDiffs(table string, src, dst []schemainfo.Column) []string {
	names := func(cs []schemainfo.Column) []string {
		out := make([]string, 0, len(cs))
		for _, c := range cs {
			out = append(out, c.Name)
		}
		return out
	}
	s, d := names(src), names(dst)
	var diffs []string
	for _, c := range d {
		if !slices.Contains(s, c) {
			diffs = append(diffs, "column "+table+"."+c+" exists only in the PostgreSQL database")
		}
	}
	for _, c := range s {
		if !slices.Contains(d, c) {
			diffs = append(diffs, "column "+table+"."+c+" exists only in the SQLite database")
		}
	}
	return diffs
}

// requireEmpty refuses a target any of whose tables holds a row. sameShape
// has already refused a table sqi does not own, so these are sqi's tables.
func requireEmpty(ctx context.Context, db *sql.DB, info map[string]schemainfo.Table) error {
	for _, n := range sortedKeys(info) {
		var rows int64
		q := `SELECT count(*) FROM ` + quoteIdent(n)
		if err := db.QueryRowContext(ctx, q).Scan(&rows); err != nil {
			return fmt.Errorf("store copy: count the rows of %s: %w", n, err)
		}
		if rows > 0 {
			return fmt.Errorf("store copy: the PostgreSQL database is not empty (%s has %d rows); use an empty database", n, rows)
		}
	}
	return nil
}

func newTablePlan(t schemainfo.Table) (*tablePlan, error) {
	if len(t.PrimaryKey) == 0 {
		return nil, fmt.Errorf("store copy: table %s has no primary key, so its rows have no order to verify in", t.Name)
	}
	p := &tablePlan{name: t.Name}
	var srcCols, dstCols []string
	for _, c := range t.Columns {
		k, err := kindOf(c.Type)
		if err != nil {
			return nil, fmt.Errorf("store copy: column %s.%s: %w", t.Name, c.Name, err)
		}
		p.cols = append(p.cols, c.Name)
		p.kinds = append(p.kinds, k)
		p.domains = append(p.domains, c.Domain)
		q := quoteIdent(c.Name)
		dstCols = append(dstCols, q)
		if k == kindText {
			// The SQLite driver decodes a TIMESTAMP column into a time.Time,
			// which would not round-trip byte for byte.
			q = "CAST(" + q + " AS TEXT)"
		}
		srcCols = append(srcCols, q)
	}
	var srcOrder, dstOrder []string
	for _, k := range t.PrimaryKey {
		i := slices.Index(p.cols, k)
		if i < 0 {
			return nil, fmt.Errorf("store copy: table %s's primary key names %s, which it does not hold", t.Name, k)
		}
		p.pk = append(p.pk, i)
		srcOrder, dstOrder = append(srcOrder, quoteIdent(k)), append(dstOrder, quoteIdent(k))
		if p.kinds[i] == kindText {
			// Byte order on both sides, whatever the column's declared collation.
			srcOrder[len(srcOrder)-1] += " COLLATE BINARY"
			dstOrder[len(dstOrder)-1] += ` COLLATE "C"`
		}
	}
	tbl := quoteIdent(t.Name)
	p.srcSelect = "SELECT " + strings.Join(srcCols, ", ") + " FROM " + tbl + " ORDER BY " + strings.Join(srcOrder, ", ")
	p.dstSelect = "SELECT " + strings.Join(dstCols, ", ") + " FROM " + tbl + " ORDER BY " + strings.Join(dstOrder, ", ")
	p.insertPrefix = "INSERT INTO " + tbl + " (" + strings.Join(dstCols, ", ") + ") VALUES "
	p.perInsert = rowsPerInsert(len(p.cols))
	return p, nil
}

// rowsPerInsert is how many rows of ncols columns one INSERT carries: 500,
// or fewer when that many would pass PostgreSQL's bind-parameter limit.
func rowsPerInsert(ncols int) int {
	return max(1, min(batchRows, maxBindParams/max(ncols, 1)))
}

// kindOf maps a target column's type to how its values are carried. A type
// this package has not been taught is refused, so a migration that adds one
// fails the copy tests rather than copying values approximately.
func kindOf(typ string) (kind, error) {
	switch typ {
	case "text", "character varying":
		return kindText, nil
	case "bigint", "integer", "smallint":
		return kindInt, nil
	case "boolean":
		return kindBool, nil
	}
	return 0, fmt.Errorf("its PostgreSQL type %q is one store copy does not know how to copy", typ)
}

// copyAll copies and verifies every table inside one read transaction on the
// source and one write transaction on the target, committing only when every
// table verified.
func copyAll(ctx context.Context, src, dst *sql.DB, plans []*tablePlan, log *slog.Logger, res *Result) error {
	srcTx, err := src.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("store copy: begin reading the SQLite database: %w", err)
	}
	defer srcTx.Rollback() //nolint:errcheck // read-only; nothing to undo
	dstTx, err := dst.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store copy: begin the PostgreSQL transaction: %w", err)
	}
	defer dstTx.Rollback() //nolint:errcheck // a no-op after Commit; otherwise the rollback is the point
	for _, p := range plans {
		n, err := copyAndVerify(ctx, srcTx, dstTx, p)
		if err != nil {
			return fmt.Errorf("%w; no sqi rows were committed (a schema store copy created stays, empty)", err)
		}
		res.Tables = append(res.Tables, TableResult{Table: p.name, Rows: n})
		log.InfoContext(ctx, "store copy: copied table", slog.String("table", p.name), slog.Int64("rows", n))
	}
	if err := dstTx.Commit(); err != nil {
		return fmt.Errorf("store copy: commit: %w; no sqi rows were committed (a schema store copy created stays, empty)", err)
	}
	return nil
}

func copyAndVerify(ctx context.Context, srcTx, dstTx *sql.Tx, p *tablePlan) (int64, error) {
	n, srcSum, err := copyTable(ctx, srcTx, dstTx, p)
	if err != nil {
		return 0, err
	}
	dstN, dstSum, err := digestTable(ctx, dstTx, p)
	if err != nil {
		return 0, err
	}
	if n != dstN || srcSum != dstSum {
		return 0, fmt.Errorf("store copy: %s did not verify (source %d rows, digest %s; copy %d rows, digest %s)",
			p.name, n, srcSum, dstN, dstSum)
	}
	return n, nil
}

// copyTable streams the table from the source, in primary-key order, into
// multi-row INSERTs, and returns the source's row count and digest.
func copyTable(ctx context.Context, srcTx, dstTx *sql.Tx, p *tablePlan) (n int64, sum string, err error) {
	rows, err := srcTx.QueryContext(ctx, p.srcSelect)
	if err != nil {
		return 0, "", fmt.Errorf("store copy: read %s: %w", p.name, err)
	}
	defer rows.Close()
	d := newDigest()
	w := &writer{tx: dstTx, p: p, rows: make([][]any, 0, p.perInsert)}
	for rows.Next() {
		vals, err := scanRow(rows, len(p.cols))
		if err != nil {
			return 0, "", fmt.Errorf("store copy: read %s: %w", p.name, err)
		}
		d.row(vals)
		out, err := p.convert(vals)
		if err != nil {
			return 0, "", err
		}
		if err := w.add(ctx, out); err != nil {
			return 0, "", err
		}
	}
	if err := rows.Err(); err != nil {
		return 0, "", fmt.Errorf("store copy: read %s: %w", p.name, err)
	}
	if err := w.flush(ctx); err != nil {
		return 0, "", err
	}
	return d.rows, d.sum(), nil
}

// digestTable reads the table back from the target, in primary-key order,
// and returns its row count and digest.
func digestTable(ctx context.Context, tx *sql.Tx, p *tablePlan) (n int64, sum string, err error) {
	rows, err := tx.QueryContext(ctx, p.dstSelect)
	if err != nil {
		return 0, "", fmt.Errorf("store copy: read back %s: %w", p.name, err)
	}
	defer rows.Close()
	d := newDigest()
	for rows.Next() {
		vals, err := scanRow(rows, len(p.cols))
		if err != nil {
			return 0, "", fmt.Errorf("store copy: read back %s: %w", p.name, err)
		}
		d.row(vals)
	}
	if err := rows.Err(); err != nil {
		return 0, "", fmt.Errorf("store copy: read back %s: %w", p.name, err)
	}
	return d.rows, d.sum(), nil
}

func scanRow(rows *sql.Rows, n int) ([]any, error) {
	vals := make([]any, n)
	ptrs := make([]any, n)
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	return vals, nil
}

// convert turns one source row into the values the target takes, refusing
// any value the target column could not hold as it is: the only conversion is
// a boolean column's 0/1.
func (p *tablePlan) convert(vals []any) ([]any, error) {
	out := make([]any, len(vals))
	for i, v := range vals {
		c, err := convertValue(p.kinds[i], v)
		if err != nil {
			return nil, fmt.Errorf("store copy: %s row (%s) column %s: %w", p.name, p.key(vals), p.cols[i], err)
		}
		out[i] = c
	}
	return out, nil
}

func convertValue(k kind, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch k {
	case kindBool:
		if n, ok := v.(int64); ok && (n == 0 || n == 1) {
			return n == 1, nil
		}
		return nil, fmt.Errorf("%#v is not 0 or 1, and the PostgreSQL column is a boolean", v)
	case kindInt:
		if n, ok := v.(int64); ok {
			return n, nil
		}
		return nil, fmt.Errorf("%#v is not an integer", v)
	default:
		s, ok := v.(string)
		if b, isBytes := v.([]byte); isBytes {
			s, ok = string(b), true
		}
		switch {
		case !ok:
			return nil, fmt.Errorf("%#v is not text", v)
		case !utf8.ValidString(s):
			return nil, errors.New("the value is not valid UTF-8, which PostgreSQL text cannot hold")
		case strings.ContainsRune(s, 0):
			return nil, errors.New("the value contains a NUL byte, which PostgreSQL text cannot hold")
		}
		return s, nil
	}
}

// key renders a row's primary key for a message: "id=task-1".
func (p *tablePlan) key(vals []any) string {
	parts := make([]string, 0, len(p.pk))
	for _, i := range p.pk {
		parts = append(parts, fmt.Sprintf("%s=%v", p.cols[i], vals[i]))
	}
	return strings.Join(parts, ", ")
}

// writer buffers converted rows and inserts them a batch at a time. Each
// batch runs under a savepoint so that, when PostgreSQL refuses one, the
// writer can roll the batch back and find the row (and, where it can, the
// column) to name.
type writer struct {
	tx   *sql.Tx
	p    *tablePlan
	rows [][]any
}

func (w *writer) add(ctx context.Context, row []any) error {
	w.rows = append(w.rows, row)
	if len(w.rows) < w.p.perInsert {
		return nil
	}
	return w.flush(ctx)
}

func (w *writer) flush(ctx context.Context) error {
	if len(w.rows) == 0 {
		return nil
	}
	if err := w.exec(ctx, "SAVEPOINT storecopy_batch"); err != nil {
		return err
	}
	if err := w.insert(ctx, w.rows); err != nil {
		return w.locate(ctx, err)
	}
	if err := w.exec(ctx, "RELEASE SAVEPOINT storecopy_batch"); err != nil {
		return err
	}
	w.rows = w.rows[:0]
	return nil
}

func (w *writer) insert(ctx context.Context, rows [][]any) error {
	ncols := len(w.p.cols)
	var b strings.Builder
	b.WriteString(w.p.insertPrefix)
	args := make([]any, 0, len(rows)*ncols)
	for r, row := range rows {
		if r > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('(')
		for c := range ncols {
			if c > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(len(args) + 1))
			args = append(args, row[c])
		}
		b.WriteByte(')')
	}
	_, err := w.tx.ExecContext(ctx, b.String(), args...)
	return err
}

func (w *writer) exec(ctx context.Context, q string) error {
	if _, err := w.tx.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("store copy: %s: %s: %w", w.p.name, q, err)
	}
	return nil
}

// locate rolls back the refused batch and inserts its rows one at a time to
// find the one PostgreSQL refuses, and names it.
func (w *writer) locate(ctx context.Context, batchErr error) error {
	if err := w.exec(ctx, "ROLLBACK TO SAVEPOINT storecopy_batch"); err != nil {
		return w.refused(ctx, nil, errors.Join(batchErr, err))
	}
	for _, row := range w.rows {
		if err := w.exec(ctx, "SAVEPOINT storecopy_row"); err != nil {
			return w.refused(ctx, nil, errors.Join(batchErr, err))
		}
		if err := w.insert(ctx, [][]any{row}); err != nil {
			if rbErr := w.exec(ctx, "ROLLBACK TO SAVEPOINT storecopy_row"); rbErr != nil {
				return w.refused(ctx, row, errors.Join(err, rbErr))
			}
			return w.refused(ctx, row, err)
		}
	}
	// The batch failed but no row alone did: name the batch's first row.
	return w.refused(ctx, nil, batchErr)
}

// refused builds the error for a value PostgreSQL refused: the table, the
// row's primary key and the column, as far as they can be told.
func (w *writer) refused(ctx context.Context, row []any, err error) error {
	where := w.p.name
	if row != nil {
		where += " row (" + w.p.key(row) + ")"
		if col := w.column(ctx, row, err); col != "" {
			where += " column " + col
		}
	} else if len(w.rows) > 0 {
		where += " (in the batch starting at row " + w.p.key(w.rows[0]) + ")"
	}
	if pe, ok := errors.AsType[*pgconn.PgError](err); ok && pe.Detail != "" {
		return fmt.Errorf("store copy: PostgreSQL refused %s: %w (%s)", where, err, pe.Detail)
	}
	return fmt.Errorf("store copy: PostgreSQL refused %s: %w", where, err)
}

// column names the column PostgreSQL's error is about: the one it reports,
// or, for a domain's CHECK (which reports the domain, not the column), each
// column of that domain whose value the domain refuses on its own.
func (w *writer) column(ctx context.Context, row []any, err error) string {
	pe, ok := errors.AsType[*pgconn.PgError](err)
	if !ok {
		return ""
	}
	if pe.ColumnName != "" {
		return pe.ColumnName
	}
	if pe.DataTypeName == "" {
		return ""
	}
	var cols []string
	for i, d := range w.p.domains {
		if d != pe.DataTypeName || row[i] == nil {
			continue
		}
		if w.domainRefuses(ctx, d, row[i]) {
			cols = append(cols, w.p.cols[i])
		}
	}
	return strings.Join(cols, ", ")
}

func (w *writer) domainRefuses(ctx context.Context, domain string, v any) bool {
	if _, err := w.tx.ExecContext(ctx, "SAVEPOINT storecopy_probe"); err != nil {
		return false
	}
	_, err := w.tx.ExecContext(ctx, "SELECT CAST($1::text AS "+quoteIdent(domain)+")", v) //nolint:gosec // G202: the domain name comes from the catalog and is quoted; the value is a parameter
	if err != nil {
		_, _ = w.tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT storecopy_probe") //nolint:errcheck // diagnosis only; the copy is failing anyway
		return true
	}
	_, _ = w.tx.ExecContext(ctx, "RELEASE SAVEPOINT storecopy_probe") //nolint:errcheck // diagnosis only
	return false
}

// quoteIdent double-quotes an identifier read from a catalog, doubling any
// quote inside it, so it is always read as a name.
func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
