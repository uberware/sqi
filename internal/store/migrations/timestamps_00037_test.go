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

// TestMigration00037_DownUpNormalizesRowsAnOlderBinaryWrote pins the recovery
// docs/architecture.md documents for a downgrade excursion: an older release
// writes trimmed timestamps into a database 00037 has already rewritten, and
// taking 00037 down (a no-op) and up again re-runs it over them.
func TestMigration00037_DownUpNormalizesRowsAnOlderBinaryWrote(t *testing.T) {
	path, s, _ := openSeedable(t)
	farm, err := s.CreateFarm(t.Context(), store.Farm{ID: uuid.NewString(), Name: uuid.NewString()})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db := rewindTo(t, path, 36)
	// What an older release writes after a downgrade.
	if _, err := db.ExecContext(t.Context(),
		`UPDATE farms SET created_at = '2026-10-10T12:00:05.5Z' WHERE id = ?`, farm.ID); err != nil {
		t.Fatalf("plant: %v", err)
	}
	if err := goose.UpTo(db, ".", 37); err != nil {
		t.Fatalf("UpTo(37): %v", err)
	}
	var created string
	if err := db.QueryRowContext(t.Context(), `SELECT created_at FROM farms WHERE id = ?`, farm.ID).Scan(&created); err != nil {
		t.Fatalf("read: %v", err)
	}
	if want := "2026-10-10T12:00:05.500000000Z"; created != want {
		t.Fatalf("created_at after down and up = %q, want %q", created, want)
	}
}

// TestMigrations_StampTimesInTheStoredForm keeps SQL that stamps a time from
// writing strftime's three fractional digits ('%fZ'), which sort wrong against
// the 30-character form every other write uses. Only the three repair
// migrations written before 00037 normalized their output may contain it.
func TestMigrations_StampTimesInTheStoredForm(t *testing.T) {
	allowed := []string{
		"00031_release_leaked_claims.sql",
		"00033_finalize_canceled_job_steps.sql",
		"00035_close_orphan_attempts.sql",
	}
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil || len(names) == 0 {
		t.Fatalf("list migrations = %v, %v", names, err)
	}
	for _, name := range names {
		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(body), "%fZ") && !slices.Contains(allowed, name) {
			t.Errorf("%s stamps a time with strftime's three digits ('%%fZ'); write "+
				"strftime('%%Y-%%m-%%dT%%H:%%M:%%f', 'now') || '000000Z', the 30-character stored form", name)
		}
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
	schema := queryStrings(t, openRaw(t, path), `
SELECT m.name || '.' || p.name
FROM   sqlite_master m JOIN pragma_table_info(m.name) p
WHERE  m.type = 'table' AND m.name NOT LIKE 'sqlite_%' AND m.name <> 'goose_db_version'
  AND  (p.name IN ('at', 'retry_after') OR p.name LIKE '%\_at' ESCAPE '\')`)
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
