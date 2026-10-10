// SPDX-License-Identifier: AGPL-3.0-or-later

package migrations_test

import (
	"database/sql"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/migrations"
)

const migration00037 = "00037_fixed_width_timestamps.sql"

// TestMigration00037_RewritesEveryStoredForm plants each form a timestamp has
// been stored in, and values of no recognized shape, in a database rewound to
// 36, then migrates. Recognized forms become the 30-character form at the same
// instant; everything else, NULL included, is left byte-for-byte.
func TestMigration00037_RewritesEveryStoredForm(t *testing.T) {
	cases := []struct{ name, stored, want string }{
		{"trimmed whole second", "2026-10-10T12:00:05Z", "2026-10-10T12:00:05.000000000Z"},
		{"trimmed one digit", "2026-10-10T12:00:05.5Z", "2026-10-10T12:00:05.500000000Z"},
		{"trimmed eight digits", "2026-10-10T12:00:05.12345678Z", "2026-10-10T12:00:05.123456780Z"},
		{"three digits from strftime", "2026-10-10T12:00:05.500Z", "2026-10-10T12:00:05.500000000Z"},
		{"already nine digits", "2026-10-10T12:00:05.123456789Z", "2026-10-10T12:00:05.123456789Z"},
		{"not a timestamp", "t", "t"},
		{"time.Time String form", "2026-10-10 12:00:05 +0000 UTC", "2026-10-10 12:00:05 +0000 UTC"},
		{"empty fraction", "2026-10-10T12:00:05.Z", "2026-10-10T12:00:05.Z"},
		{"non-digit fraction", "2026-10-10T12:00:05.5x5Z", "2026-10-10T12:00:05.5x5Z"},
		{"doubled zone", "2026-10-10T12:00:05.5ZZ", "2026-10-10T12:00:05.5ZZ"},
		{"numeric offset", "2026-10-10T12:00:05.5+01:00", "2026-10-10T12:00:05.5+01:00"},
	}
	path, s, _ := openSeedable(t)
	farmIDs := make([]string, len(cases))
	for i := range cases {
		f, err := s.CreateFarm(t.Context(), store.Farm{ID: uuid.NewString(), Name: uuid.NewString()})
		if err != nil {
			t.Fatalf("CreateFarm: %v", err)
		}
		farmIDs[i] = f.ID
	}
	// A nullable timestamp holding NULL: a worker that has never heartbeated.
	if _, _, err := s.RegisterWorker(t.Context(), store.Worker{
		ID: "w-null", FarmID: farmIDs[0], Hostname: "node", Status: store.WorkerStatusOnline,
	}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db := rewindTo(t, path, 36)
	for i, c := range cases {
		if _, err := db.ExecContext(t.Context(),
			`UPDATE farms SET created_at = ?, updated_at = ? WHERE id = ?`, c.stored, c.stored, farmIDs[i]); err != nil {
			t.Fatalf("plant %s: %v", c.name, err)
		}
	}
	if err := goose.Up(db, "."); err != nil {
		t.Fatalf("goose.Up: %v", err)
	}

	for i, c := range cases {
		var created, updated string
		if err := db.QueryRowContext(t.Context(),
			`SELECT created_at, updated_at FROM farms WHERE id = ?`, farmIDs[i]).Scan(&created, &updated); err != nil {
			t.Fatalf("read %s: %v", c.name, err)
		}
		if created != c.want || updated != c.want {
			t.Errorf("%s: %q became (%q, %q), want %q", c.name, c.stored, created, updated, c.want)
		}
	}
	var heartbeat sql.NullString
	if err := db.QueryRowContext(t.Context(),
		`SELECT last_heartbeat_at FROM workers WHERE id = 'w-null'`).Scan(&heartbeat); err != nil {
		t.Fatalf("read heartbeat: %v", err)
	}
	if heartbeat.Valid {
		t.Errorf("NULL heartbeat became %q, want NULL", heartbeat.String)
	}

	// Applying it again changes nothing: its down is a no-op and every row it
	// recognized is now 30 characters, which it skips.
	before := dumpTables(t, db)
	if err := goose.DownTo(db, ".", 36); err != nil {
		t.Fatalf("DownTo(36): %v", err)
	}
	if err := goose.UpTo(db, ".", 37); err != nil {
		t.Fatalf("UpTo(37): %v", err)
	}
	if after := dumpTables(t, db); after != before {
		t.Fatalf("re-applying 00037 changed the database:\n--- before\n%s--- after\n%s", before, after)
	}
}

var (
	updateTableLine = regexp.MustCompile(`^UPDATE (\w+) SET$`)
	caseColumnLine  = regexp.MustCompile(`^    (\w+) = CASE$`)
)

// TestMigration00037_CoversEveryTimestampColumn fails, by name, on a timestamp
// column the schema has and 00037 does not rewrite, or one 00037 rewrites that
// the schema does not have. A timestamp column is one named at, retry_after or
// *_at. It guards 00037's completeness only: a column a later migration adds is
// written fixed-width by timeToText from the start and needs no rewrite.
func TestMigration00037_CoversEveryTimestampColumn(t *testing.T) {
	path, s, _ := openSeedable(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	schema := schemaTimestampColumns(t, db)
	rewritten := migrationColumns(t)
	for _, c := range schema {
		if !slices.Contains(rewritten, c) {
			t.Errorf("timestamp column %s is not rewritten by %s", c, migration00037)
		}
	}
	for _, c := range rewritten {
		if !slices.Contains(schema, c) {
			t.Errorf("%s rewrites %s, which the schema does not have", migration00037, c)
		}
	}
	if len(schema) == 0 {
		t.Fatal("found no timestamp columns; the comparison would be vacuous")
	}
}

func schemaTimestampColumns(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `
SELECT m.name, p.name
FROM   sqlite_master m JOIN pragma_table_info(m.name) p
WHERE  m.type = 'table' AND m.name NOT LIKE 'sqlite_%' AND m.name <> 'goose_db_version'`)
	if err != nil {
		t.Fatalf("list columns: %v", err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var table, col string
		if err := rows.Scan(&table, &col); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		if col == "at" || col == "retry_after" || strings.HasSuffix(col, "_at") {
			cols = append(cols, table+"."+col)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list columns: %v", err)
	}
	return cols
}

// migrationColumns reads table.column for every CASE in 00037's Up section.
func migrationColumns(t *testing.T) []string {
	t.Helper()
	body, err := fs.ReadFile(migrations.FS, migration00037)
	if err != nil {
		t.Fatalf("read %s: %v", migration00037, err)
	}
	up, _, found := strings.Cut(string(body), "-- +goose Down")
	if !found {
		t.Fatalf("%s has no -- +goose Down section", migration00037)
	}
	var cols []string
	table := ""
	for line := range strings.SplitSeq(up, "\n") {
		if m := updateTableLine.FindStringSubmatch(line); m != nil {
			table = m[1]
		} else if m := caseColumnLine.FindStringSubmatch(line); m != nil {
			cols = append(cols, table+"."+m[1])
		}
	}
	return cols
}
