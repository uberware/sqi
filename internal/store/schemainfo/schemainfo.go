// SPDX-License-Identifier: AGPL-3.0-or-later

// Package schemainfo reads the shape of a migrated sqi database — tables,
// columns, primary keys, foreign-key targets — from either backend's catalog,
// for tools that must follow the live schema rather than a hard-coded list
// (storecopy). TestSchemaParity cross-checks it against its own, independent
// queries on both backends.
package schemainfo

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// Column is one column of a table.
type Column struct {
	Name string
	// Type is lower-cased: SQLite's declared type ("text", "integer",
	// "timestamp"), or PostgreSQL's type as format_type renders it ("text",
	// "bigint", "boolean"). A PostgreSQL column of a domain reports the
	// domain's base type here and the domain in Domain.
	Type string
	// Domain is the PostgreSQL domain the column is declared with (sqi's
	// "ts"), or empty. Always empty on SQLite.
	Domain string
}

// Table is one table of a migrated sqi database.
type Table struct {
	Name       string
	Columns    []Column // in ordinal order
	PrimaryKey []string // in key order
	References []string // tables its foreign keys point at, sorted, without itself
}

// skipGoose is goose's bookkeeping table, which is not sqi data.
const skipGoose = "goose_db_version"

// SQLite reads a SQLite database's schema: every table but SQLite's own and
// goose's.
func SQLite(ctx context.Context, db *sql.DB) (map[string]Table, error) {
	names, err := strs(ctx, db,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name != ?`, skipGoose)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Table, len(names))
	for _, n := range names {
		t, err := sqliteTable(ctx, db, n)
		if err != nil {
			return nil, err
		}
		out[n] = t
	}
	return out, nil
}

func sqliteTable(ctx context.Context, db *sql.DB, name string) (Table, error) {
	t := Table{Name: name}
	rows, err := db.QueryContext(ctx, `SELECT name, type, pk FROM pragma_table_info(?) ORDER BY cid`, name)
	if err != nil {
		return Table{}, fmt.Errorf("schemainfo: columns of %s: %w", name, err)
	}
	defer rows.Close()
	pkAt := map[int]string{} // pk is the column's 1-based position in the key, 0 outside it
	for rows.Next() {
		var c Column
		var pk int
		if err := rows.Scan(&c.Name, &c.Type, &pk); err != nil {
			return Table{}, fmt.Errorf("schemainfo: columns of %s: %w", name, err)
		}
		c.Type = strings.ToLower(c.Type)
		t.Columns = append(t.Columns, c)
		if pk > 0 {
			pkAt[pk] = c.Name
		}
	}
	if err := rows.Err(); err != nil {
		return Table{}, fmt.Errorf("schemainfo: columns of %s: %w", name, err)
	}
	for i := 1; i <= len(pkAt); i++ {
		t.PrimaryKey = append(t.PrimaryKey, pkAt[i])
	}
	refs, err := strs(ctx, db, `SELECT DISTINCT "table" FROM pragma_foreign_key_list(?)`, name)
	if err != nil {
		return Table{}, err
	}
	t.References = withoutSelf(refs, name)
	return t, nil
}

// Postgres reads the schema of a PostgreSQL database: every ordinary or
// partitioned table in current_schema() (where sqi's migrations create them,
// "public" by default) but goose's. It reads pg_catalog rather than
// information_schema, which lists only the tables the connected role holds a
// privilege on and so could hide a table from the copier.
func Postgres(ctx context.Context, db *sql.DB) (map[string]Table, error) {
	names, err := strs(ctx, db, `SELECT c.relname FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = current_schema() AND c.relkind IN ('r', 'p')
		  AND NOT c.relispartition AND c.relname <> $1`, skipGoose)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Table, len(names))
	for _, n := range names {
		t, err := postgresTable(ctx, db, n)
		if err != nil {
			return nil, err
		}
		out[n] = t
	}
	return out, nil
}

// relation is the WHERE clause naming one table of current_schema() as c.
// current_schema() is a name, compared as one: to_regnamespace would re-parse
// it as an identifier and fold a mixed-case name to lower case.
const relation = `c.relnamespace = (SELECT oid FROM pg_namespace WHERE nspname = current_schema()) AND c.relname = $1`

func postgresTable(ctx context.Context, db *sql.DB, name string) (Table, error) {
	t := Table{Name: name}
	rows, err := db.QueryContext(ctx, `SELECT a.attname,
		       format_type(CASE WHEN ty.typtype = 'd' THEN ty.typbasetype ELSE a.atttypid END, NULL),
		       CASE WHEN ty.typtype = 'd' THEN ty.typname ELSE '' END
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_type ty ON ty.oid = a.atttypid
		WHERE `+relation+` AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum`, name)
	if err != nil {
		return Table{}, fmt.Errorf("schemainfo: columns of %s: %w", name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var c Column
		if err := rows.Scan(&c.Name, &c.Type, &c.Domain); err != nil {
			return Table{}, fmt.Errorf("schemainfo: columns of %s: %w", name, err)
		}
		c.Type = strings.ToLower(c.Type)
		t.Columns = append(t.Columns, c)
	}
	if err := rows.Err(); err != nil {
		return Table{}, fmt.Errorf("schemainfo: columns of %s: %w", name, err)
	}
	// conkey lists a constraint's columns in key order.
	if t.PrimaryKey, err = strs(ctx, db, `SELECT a.attname
		FROM pg_constraint k
		JOIN pg_class c ON c.oid = k.conrelid
		CROSS JOIN LATERAL unnest(k.conkey) WITH ORDINALITY AS key(attnum, ord)
		JOIN pg_attribute a ON a.attrelid = k.conrelid AND a.attnum = key.attnum
		WHERE `+relation+` AND k.contype = 'p'
		ORDER BY key.ord`, name); err != nil {
		return Table{}, err
	}
	refs, err := strs(ctx, db, `SELECT DISTINCT r.relname
		FROM pg_constraint k
		JOIN pg_class c ON c.oid = k.conrelid
		JOIN pg_class r ON r.oid = k.confrelid
		WHERE `+relation+` AND k.contype = 'f'`, name)
	if err != nil {
		return Table{}, err
	}
	t.References = withoutSelf(refs, name)
	return t, nil
}

// InsertOrder sorts tables so every table comes after the tables it
// references (Kahn's algorithm, ties broken by name for a stable order). A
// cycle, or a reference to a table not in tables, is an error.
func InsertOrder(tables map[string]Table) ([]string, error) {
	indeg := make(map[string]int, len(tables))
	children := map[string][]string{}
	for n, t := range tables {
		indeg[n] += 0
		for _, r := range t.References {
			if _, ok := tables[r]; !ok {
				return nil, fmt.Errorf("schemainfo: %s references %s, which is not among the tables read", n, r)
			}
			indeg[n]++
			children[r] = append(children[r], n)
		}
	}
	var ready, out []string
	for n, d := range indeg {
		if d == 0 {
			ready = append(ready, n)
		}
	}
	for len(ready) > 0 {
		slices.Sort(ready)
		n := ready[0]
		ready = ready[1:]
		out = append(out, n)
		for _, c := range children[n] {
			indeg[c]--
			if indeg[c] == 0 {
				ready = append(ready, c)
			}
		}
	}
	if len(out) != len(tables) {
		var stuck []string
		for n, d := range indeg {
			if d > 0 {
				stuck = append(stuck, n)
			}
		}
		slices.Sort(stuck)
		return nil, fmt.Errorf("schemainfo: foreign keys form a cycle among %s", strings.Join(stuck, ", "))
	}
	return out, nil
}

func strs(ctx context.Context, db *sql.DB, q string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("schemainfo: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("schemainfo: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schemainfo: %w", err)
	}
	return out, nil
}

func withoutSelf(refs []string, self string) []string {
	refs = slices.DeleteFunc(refs, func(r string) bool { return r == self })
	slices.Sort(refs)
	return refs
}
