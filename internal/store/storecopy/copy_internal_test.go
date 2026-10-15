// SPDX-License-Identifier: AGPL-3.0-or-later

package storecopy

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	msqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/uberware/sqi/internal/store/schemainfo"
)

func TestRowsPerInsert_StaysUnderTheBindParameterLimit(t *testing.T) {
	for _, tc := range []struct{ cols, want int }{
		{1, 500}, {23, 500}, {131, 500}, {132, 496}, {1000, 65}, {70000, 1},
	} {
		got := rowsPerInsert(tc.cols)
		if got != tc.want {
			t.Errorf("rowsPerInsert(%d) = %d, want %d", tc.cols, got, tc.want)
		}
		if tc.cols <= maxBindParams && got*tc.cols > maxBindParams {
			t.Errorf("rowsPerInsert(%d) = %d: %d parameters, over PostgreSQL's %d", tc.cols, got, got*tc.cols, maxBindParams)
		}
	}
}

func TestSameShape_NamesEveryDifference(t *testing.T) {
	cols := func(names ...string) []schemainfo.Column {
		var out []schemainfo.Column
		for _, n := range names {
			out = append(out, schemainfo.Column{Name: n, Type: "text"})
		}
		return out
	}
	src := map[string]schemainfo.Table{
		"farms": {Name: "farms", Columns: cols("id", "name", "old")},
		"jobs":  {Name: "jobs", Columns: cols("id")},
	}
	dst := map[string]schemainfo.Table{
		"farms": {Name: "farms", Columns: cols("id", "name", "extra")},
		"notes": {Name: "notes", Columns: cols("id")},
	}
	err := sameShape(src, dst)
	if err == nil {
		t.Fatal("sameShape accepted different schemas")
	}
	for _, want := range []string{
		"table notes exists only in the PostgreSQL database",
		"table jobs exists only in the SQLite database",
		"column farms.extra exists only in the PostgreSQL database",
		"column farms.old exists only in the SQLite database",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
	if err := sameShape(src, src); err != nil {
		t.Errorf("sameShape of a schema with itself = %v", err)
	}
}

func TestConvertValue(t *testing.T) {
	for _, tc := range []struct {
		name    string
		k       kind
		in      any
		want    any
		wantErr string
	}{
		{"null boolean", kindBool, nil, nil, ""},
		{"zero is false", kindBool, int64(0), false, ""},
		{"one is true", kindBool, int64(1), true, ""},
		{"two is no boolean", kindBool, int64(2), nil, "not 0 or 1"},
		{"text is no boolean", kindBool, "1", nil, "not 0 or 1"},
		{"integer", kindInt, int64(-7), int64(-7), ""},
		{"real is no integer", kindInt, 1.5, nil, "not an integer"},
		{"text", kindText, "héllo\n", "héllo\n", ""},
		{"bytes are text", kindText, []byte("x"), "x", ""},
		{"NUL", kindText, "a\x00b", nil, "NUL"},
		{"invalid UTF-8", kindText, "\xff", nil, "UTF-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := convertValue(tc.k, tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("convertValue = %v, %v; want an error with %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("convertValue = %#v, %v; want %#v", got, err, tc.want)
			}
		})
	}
}

func TestKindOf_RefusesAnUntaughtType(t *testing.T) {
	for _, typ := range []string{"bytea", "bpchar", "double precision", "numeric", "jsonb", "timestamp with time zone"} {
		if _, err := kindOf(typ); err == nil || !strings.Contains(err.Error(), typ) {
			t.Errorf("kindOf(%q) = %v, want a refusal naming it", typ, err)
		}
	}
}

func TestQuoteIdent(t *testing.T) {
	if got := quoteIdent(`a"b`); got != `"a""b"` {
		t.Fatalf("quoteIdent = %s", got)
	}
}

// TestOpenSource_ReadsAPendingWALWithoutWritingIt opens, through openSource,
// a WAL database whose committed rows exist only in its -wal file — with and
// without the -shm index beside it — and checks the rows are read and that
// closing the connection leaves the database file and the WAL as they were.
// It needs no PostgreSQL, so it runs on every OS the unit tests run on.
func TestOpenSource_ReadsAPendingWALWithoutWritingIt(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live.db")
	w, err := sql.Open("sqlite", live+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.SetMaxOpenConns(1)
	for _, q := range []string{`CREATE TABLE x (v TEXT)`, `INSERT INTO x VALUES ('only in the WAL')`} {
		if _, err := w.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	name := "crashed #1 %x"
	if runtime.GOOS != "windows" {
		name += "?q" // a name Windows does not allow
	}
	for _, withSHM := range []bool{false, true} {
		t.Run(fmt.Sprintf("shm=%v", withSHM), func(t *testing.T) {
			src := filepath.Join(t.TempDir(), name+".db")
			sidecars := []string{"", "-wal"}
			if withSHM {
				sidecars = append(sidecars, "-shm")
			}
			for _, s := range sidecars {
				b, err := os.ReadFile(live + s)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(src+s, b, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			dbBefore, walBefore := hashFile(t, src), hashFile(t, src+"-wal")
			db, err := openSourceDB(t, src)
			if err != nil {
				t.Fatal(err)
			}
			var n int
			if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM x`).Scan(&n); err != nil || n != 1 {
				t.Fatalf("rows read = %d, %v; want the WAL's 1", n, err)
			}
			if _, err := db.ExecContext(t.Context(), `INSERT INTO x VALUES ('no')`); err == nil {
				t.Error("a write through the source connection succeeded")
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if hashFile(t, src) != dbBefore {
				t.Error("closing the source rewrote the database file (a WAL checkpoint)")
			}
			if hashFile(t, src+"-wal") != walBefore {
				t.Error("closing the source changed or removed the WAL")
			}
		})
	}
}

// openSourceDB is openSource without its schema checks: the fixture is not an
// sqi database.
func openSourceDB(t *testing.T, path string) (*sql.DB, error) {
	t.Helper()
	dsn, err := readOnlyDSN(path)
	if err != nil {
		return nil, err
	}
	if before, _, found := strings.Cut(dsn, "?mode=ro"); !found || strings.Contains(before, "?") {
		t.Errorf("readOnlyDSN = %s: want a mode=ro URI with no unescaped '?' in the path", dsn)
	}
	return sql.Open("sqlite", dsn)
}

func hashFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return "absent: " + err.Error()
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// journalMagic opens every rollback journal header SQLite has synced.
var journalMagic = []byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7}

// TestOpenSource_RefusesAHotJournalWithoutRollingItBack opens, through
// openSource, what a server that crashed mid-transaction in rollback-journal
// mode leaves behind: the database file and a hot -journal holding the
// pre-transaction pages. Rolling that journal back writes the database file
// and deletes the journal, which a read-only reader may not do, so the open
// must fail with the operator's way out — and must leave both files exactly
// as it found them, since store copy never writes its source. The fixture is
// a copy taken while a writer's transaction is open and uncommitted, which is
// byte for byte what a crash at that instant leaves. It needs no PostgreSQL.
func TestOpenSource_RefusesAHotJournalWithoutRollingItBack(t *testing.T) {
	ctx := t.Context()
	live := filepath.Join(t.TempDir(), "live.db")
	w, err := sql.Open("sqlite", live+"?_pragma=journal_mode(DELETE)")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	conn, err := w.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, q := range []string{
		`CREATE TABLE x (v TEXT)`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 2000)
			INSERT INTO x SELECT printf('%0500d', i) FROM n`,
		// Until the pager first writes the database file it leaves the
		// journal's magic zeroed, and a journal starting with a zero byte is
		// not hot (nothing needs undoing). A cache far smaller than the
		// rewrite makes it spill mid-transaction: sync the journal, magic and
		// all, then overwrite database pages — the state a crash must undo.
		`PRAGMA cache_size = 10`,
		`BEGIN IMMEDIATE`,
		`UPDATE x SET v = v || ' rewritten'`,
	} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	src := filepath.Join(t.TempDir(), "crashed.db")
	for _, s := range []string{"", "-journal"} {
		b, err := os.ReadFile(live + s)
		if err != nil {
			t.Fatalf("the writer's open transaction left no %s: %v", live+s, err)
		}
		if s == "-journal" && !bytes.HasPrefix(b, journalMagic) {
			t.Fatalf("the journal does not start with SQLite's magic (% x...): it is not hot", b[:min(len(b), 8)])
		}
		if err := os.WriteFile(src+s, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
		t.Fatal(err)
	}

	dbBefore, journalBefore := hashFile(t, src), hashFile(t, src+"-journal")
	db, err := openSource(ctx, src)
	if err == nil {
		db.Close()
		t.Fatal("openSource opened a database with a hot journal")
	}
	for _, want := range []string{
		"cannot be read read-only",
		"start sqi-server on it once and stop it cleanly",
		`copy from a "sqi-server backup" of it`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
	if se, ok := errors.AsType[*msqlite.Error](err); !ok || se.Code() != sqlite3.SQLITE_READONLY_ROLLBACK {
		t.Errorf("error %q is not SQLite's SQLITE_READONLY_ROLLBACK: refused for some other reason than the hot journal", err)
	}
	if hashFile(t, src) != dbBefore {
		t.Error("openSource rewrote the database file (a journal rollback)")
	}
	if hashFile(t, src+"-journal") != journalBefore {
		t.Error("openSource changed or removed the hot journal")
	}
}
