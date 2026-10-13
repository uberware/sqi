// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres_test

import (
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/uberware/sqi/internal/store/migrations"
	"github.com/uberware/sqi/internal/store/postgres"
	"github.com/uberware/sqi/internal/store/schemainfo"
	"github.com/uberware/sqi/internal/store/storetest"
)

// boolColumns are the SQLite INTEGER columns PostgreSQL holds as BOOLEAN.
var boolColumns = map[string]bool{"queues.paused": true, "users.disabled": true, "workers.disabled": true}

type column struct {
	Name, Type string
	NotNull    bool
}

type index struct {
	Cols    string // comma-joined; a NOCASE or lower() key reads "lower(col)"
	Unique  bool
	Partial bool
}

type foreignKey struct{ From, To, OnDelete string }

type schema struct {
	Columns map[string][]column
	Indexes map[string][]index
	FKs     map[string][]foreignKey
}

// TestSchemaParity migrates a fresh SQLite and a fresh PostgreSQL database and
// compares them table by table, so a schema change made on one backend only
// fails here. goose's own table is left out.
func TestSchemaParity(t *testing.T) {
	pg := openRaw(t, storetest.PostgresDSN(t))

	lite, err := sql.Open("sqlite", t.TempDir()+"/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lite.Close() })
	p, err := migrations.NewProvider(lite, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatal(err)
	}

	sv, err := p.GetDBVersion(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	pv, err := postgresVersion(t, pg)
	if err != nil {
		t.Fatal(err)
	}
	if sv != pv {
		t.Fatalf("schema versions differ: sqlite %d, postgres %d; every migration ships as a pair", sv, pv)
	}

	ls, ps := sqliteSchema(t, lite), postgresSchema(t, pg)
	if a, b := slices.Sorted(maps.Keys(ls.Columns)), slices.Sorted(maps.Keys(ps.Columns)); !slices.Equal(a, b) {
		t.Fatalf("tables differ:\n sqlite   %v\n postgres %v", a, b)
	}
	for _, tbl := range slices.Sorted(maps.Keys(ls.Columns)) {
		if !slices.Equal(ls.Columns[tbl], ps.Columns[tbl]) {
			t.Errorf("%s columns differ:\n sqlite   %v\n postgres %v", tbl, ls.Columns[tbl], ps.Columns[tbl])
		}
		if a, b := sortedIdx(ls.Indexes[tbl]), sortedIdx(ps.Indexes[tbl]); !slices.Equal(a, b) {
			t.Errorf("%s indexes differ:\n sqlite   %v\n postgres %v", tbl, a, b)
		}
		if a, b := sortedFK(ls.FKs[tbl]), sortedFK(ps.FKs[tbl]); !slices.Equal(a, b) {
			t.Errorf("%s foreign keys differ:\n sqlite   %v\n postgres %v", tbl, a, b)
		}
	}

	// The copier (storecopy) reads the schema through schemainfo; it must see
	// what this test's independent queries see, on both backends.
	for name, db := range map[string]*sql.DB{"sqlite": lite, "postgres": pg} {
		var info map[string]schemainfo.Table
		var err error
		want := ls
		if name == "sqlite" {
			info, err = schemainfo.SQLite(t.Context(), db)
		} else {
			info, err = schemainfo.Postgres(t.Context(), db)
			want = ps
		}
		if err != nil {
			t.Fatal(err)
		}
		if a, b := slices.Sorted(maps.Keys(info)), slices.Sorted(maps.Keys(want.Columns)); !slices.Equal(a, b) {
			t.Fatalf("%s: schemainfo tables %v, parity tables %v", name, a, b)
		}
		for tbl, ti := range info {
			var got []string
			for _, c := range ti.Columns {
				got = append(got, c.Name)
			}
			var exp []string
			for _, c := range want.Columns[tbl] {
				exp = append(exp, c.Name)
			}
			if !slices.Equal(got, exp) {
				t.Errorf("%s.%s: schemainfo columns %v, parity columns %v", name, tbl, got, exp)
			}
			if a, b := ti.References, referencedTables(want.FKs[tbl], tbl); !slices.Equal(a, b) {
				t.Errorf("%s.%s: schemainfo references %v, parity foreign keys point at %v", name, tbl, a, b)
			}
		}
	}
}

// referencedTables is the sorted set of tables fks point at, without self.
func referencedTables(fks []foreignKey, self string) []string {
	var out []string
	for _, f := range fks {
		parent, _, _ := strings.Cut(f.To, ".")
		if parent != self && !slices.Contains(out, parent) {
			out = append(out, parent)
		}
	}
	slices.Sort(out)
	return out
}

func postgresVersion(t *testing.T, db *sql.DB) (int64, error) {
	t.Helper()
	return postgres.SchemaVersion(t.Context(), db)
}

func sortedIdx(in []index) []string     { return sortedFmt(in) }
func sortedFK(in []foreignKey) []string { return sortedFmt(in) }

func sortedFmt[T any](in []T) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, fmt.Sprintf("%+v", v))
	}
	slices.Sort(out)
	return out
}

// collect runs query and returns what scan reads from each row.
func collect[T any](t *testing.T, db *sql.DB, scan func(*sql.Rows) (T, error), query string, args ...any) []T {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			t.Fatalf("%s: scan: %v", query, err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return out
}

func scanName(r *sql.Rows) (string, error) {
	var s string
	return s, r.Scan(&s)
}

// fkRule maps a delete rule onto the one spelling both backends share: the
// default (NO ACTION) is the empty string.
func fkRule(rule string) string {
	if rule == "NO ACTION" {
		return ""
	}
	return rule
}

func sqliteSchema(t *testing.T, db *sql.DB) schema {
	t.Helper()
	s := schema{Columns: map[string][]column{}, Indexes: map[string][]index{}, FKs: map[string][]foreignKey{}}
	for _, tbl := range collect(t, db, scanName,
		`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name != 'goose_db_version'`) {
		s.Columns[tbl] = sqliteColumns(t, db, tbl)
		s.Indexes[tbl] = sqliteIndexes(t, db, tbl)
		s.FKs[tbl] = sqliteFKs(t, db, tbl)
	}
	return s
}

func sqliteColumns(t *testing.T, db *sql.DB, tbl string) []column {
	t.Helper()
	return collect(t, db, func(r *sql.Rows) (column, error) {
		var (
			c       column
			typ     string
			pk      int
			notNull bool
		)
		if err := r.Scan(&c.Name, &typ, &notNull, &pk); err != nil {
			return column{}, err
		}
		c.Type = strings.ToUpper(typ)
		switch {
		case c.Type == "TIMESTAMP":
			c.Type = "TEXT"
		case c.Type == "INTEGER" && boolColumns[tbl+"."+c.Name]:
			c.Type = "BOOLEAN"
		}
		// A SQLite TEXT primary key admits NULL for legacy reasons; PostgreSQL never does.
		c.NotNull = notNull || pk > 0
		return c, nil
	}, `SELECT name, type, "notnull", pk FROM pragma_table_info(?) ORDER BY cid`, tbl)
}

func sqliteIndexes(t *testing.T, db *sql.DB, tbl string) []index {
	t.Helper()
	type listed struct {
		name            string
		unique, partial bool
	}
	lst := collect(t, db, func(r *sql.Rows) (listed, error) {
		var l listed
		return l, r.Scan(&l.name, &l.unique, &l.partial)
	}, `SELECT name, "unique", partial FROM pragma_index_list(?)`, tbl)

	out := make([]index, 0, len(lst))
	for _, l := range lst {
		// Key columns only: index_xinfo also lists the rowid, with key = 0.
		cols := collect(t, db, func(r *sql.Rows) (string, error) {
			var name sql.NullString
			var coll string
			if err := r.Scan(&name, &coll); err != nil {
				return "", err
			}
			// COLLATE NOCASE folds ASCII as lower() does under COLLATE "C".
			if coll == "NOCASE" {
				return "lower(" + name.String + ")", nil
			}
			return name.String, nil
		}, `SELECT name, coll FROM pragma_index_xinfo(?) WHERE key = 1 ORDER BY seqno`, l.name)
		out = append(out, index{Cols: strings.Join(cols, ","), Unique: l.unique, Partial: l.partial})
	}
	return out
}

func sqliteFKs(t *testing.T, db *sql.DB, tbl string) []foreignKey {
	t.Helper()
	return collect(t, db, func(r *sql.Rows) (foreignKey, error) {
		var f foreignKey
		var parent string
		var to sql.NullString
		if err := r.Scan(&f.From, &parent, &to, &f.OnDelete); err != nil {
			return foreignKey{}, err
		}
		f.To = parent + "." + to.String
		f.OnDelete = fkRule(f.OnDelete)
		return f, nil
	}, `SELECT "from", "table", "to", on_delete FROM pragma_foreign_key_list(?)`, tbl)
}

func postgresSchema(t *testing.T, db *sql.DB) schema {
	t.Helper()
	s := schema{Columns: map[string][]column{}, Indexes: map[string][]index{}, FKs: map[string][]foreignKey{}}
	for _, tbl := range collect(t, db, scanName,
		`SELECT table_name FROM information_schema.tables
		 WHERE table_schema = 'public' AND table_type = 'BASE TABLE' AND table_name != 'goose_db_version'`) {
		s.Columns[tbl] = postgresColumns(t, db, tbl)
		s.Indexes[tbl] = postgresIndexes(t, db, tbl)
		s.FKs[tbl] = postgresFKs(t, db, tbl)
	}
	return s
}

func postgresColumns(t *testing.T, db *sql.DB, tbl string) []column {
	t.Helper()
	return collect(t, db, func(r *sql.Rows) (column, error) {
		var c column
		var typ string
		if err := r.Scan(&c.Name, &typ, &c.NotNull); err != nil {
			return column{}, err
		}
		// A column of the ts domain reports data_type 'text' (its domain is domain_name).
		// Any other type, integer included, is left as reported so the comparison fails.
		switch typ {
		case "text":
			c.Type = "TEXT"
		case "bigint":
			c.Type = "INTEGER"
		case "boolean":
			c.Type = "BOOLEAN"
		default:
			c.Type = typ
		}
		return c, nil
	}, `SELECT column_name, data_type, is_nullable = 'NO'
	    FROM information_schema.columns
	    WHERE table_schema = 'public' AND table_name = $1
	    ORDER BY ordinal_position`, tbl)
}

func postgresIndexes(t *testing.T, db *sql.DB, tbl string) []index {
	t.Helper()
	// pg_get_indexdef numbers key columns from 1 (0 is the whole definition),
	// and renders an expression key as written, so lower(name) reads as such.
	return collect(t, db, func(r *sql.Rows) (index, error) {
		var x index
		return x, r.Scan(&x.Unique, &x.Partial, &x.Cols)
	}, `SELECT i.indisunique, i.indpred IS NOT NULL,
	           array_to_string(ARRAY(SELECT pg_get_indexdef(i.indexrelid, k, true)
	                                 FROM generate_series(1, i.indnkeyatts::int) k ORDER BY k), ',')
	    FROM pg_index i JOIN pg_class c ON c.oid = i.indrelid
	    WHERE c.relname = $1 AND c.relnamespace = 'public'::regnamespace`, tbl)
}

func postgresFKs(t *testing.T, db *sql.DB, tbl string) []foreignKey {
	t.Helper()
	return collect(t, db, func(r *sql.Rows) (foreignKey, error) {
		var f foreignKey
		if err := r.Scan(&f.From, &f.To, &f.OnDelete); err != nil {
			return foreignKey{}, err
		}
		f.OnDelete = fkRule(f.OnDelete)
		return f, nil
	}, `SELECT kcu.column_name, ccu.table_name || '.' || ccu.column_name, rc.delete_rule
	    FROM information_schema.referential_constraints rc
	    JOIN information_schema.key_column_usage kcu ON kcu.constraint_name = rc.constraint_name
	    JOIN information_schema.constraint_column_usage ccu ON ccu.constraint_name = rc.constraint_name
	    WHERE kcu.table_name = $1`, tbl)
}
