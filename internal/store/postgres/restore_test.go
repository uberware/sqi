// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/migrations"
	"github.com/uberware/sqi/internal/store/postgres"
	"github.com/uberware/sqi/internal/store/storetest"
)

// TestPostgres_RestoreRoundTrip backs a database up, reads the schema version
// off the archive, restores it into an empty database with postgres.Restore,
// and reads the row back.
func TestPostgres_RestoreRoundTrip(t *testing.T) {
	storetest.RequirePostgresTool(t, "pg_dump")
	storetest.RequirePostgresTool(t, "pg_restore")
	src, srcDSN := storetest.PostgresAndDSN(t)
	if _, err := src.CreateFarm(t.Context(), store.Farm{ID: "farm-restore", Name: "kept"}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "sqi.dump")
	if err := postgres.BackupDSN(t.Context(), srcDSN, out, nil); err != nil {
		t.Fatal(err)
	}
	v, err := postgres.ArchiveSchemaVersion(t.Context(), out)
	maxV, mErr := migrations.MaxVersion("postgres")
	if mErr != nil {
		t.Fatal(mErr)
	}
	if err != nil || v != maxV {
		t.Fatalf("ArchiveSchemaVersion = %d, %v; want %d", v, err, maxV)
	}

	dst := storetest.PostgresEmptyDSN(t)
	if err := postgres.RequireEmptyDatabase(t.Context(), dst); err != nil {
		t.Fatalf("empty database refused: %v", err)
	}
	if err := postgres.Restore(t.Context(), dst, out, nil); err != nil {
		t.Fatal(err)
	}
	err = postgres.RequireEmptyDatabase(t.Context(), dst)
	if err == nil || !strings.Contains(err.Error(), "needs an empty database") {
		t.Fatalf("RequireEmptyDatabase of a restored database = %v, want a refusal", err)
	}
	got, err := postgres.Open(t.Context(), dst, postgres.Options{MaxConns: 2})
	if err != nil {
		t.Fatalf("open the restored database: %v", err)
	}
	defer got.Close()
	if f, err := got.GetFarm(t.Context(), "farm-restore"); err != nil || f.Name != "kept" {
		t.Fatalf("restored farm = %+v, %v", f, err)
	}
}

func TestPostgres_RestoreOfACorruptArchiveLeavesTheDatabaseEmpty(t *testing.T) {
	storetest.RequirePostgresTool(t, "pg_restore")
	bad := filepath.Join(t.TempDir(), "bad.dump")
	if err := os.WriteFile(bad, []byte("PGDMP garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.ArchiveSchemaVersion(t.Context(), bad); err == nil {
		t.Fatal("ArchiveSchemaVersion accepted garbage")
	}
	dst := storetest.PostgresEmptyDSN(t)
	if err := postgres.Restore(t.Context(), dst, bad, nil); err == nil {
		t.Fatal("Restore accepted garbage")
	}
	if err := postgres.RequireEmptyDatabase(t.Context(), dst); err != nil {
		t.Fatalf("database not empty after a failed restore: %v", err)
	}
}

// tempRole creates a cluster-wide role named for the test (roles outlive the
// test's database) and drops it, and anything it owns or was granted in db's
// database, when the test ends.
func tempRole(t *testing.T, db *sql.DB, attrs string) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	name := "sqi_t_role_" + hex.EncodeToString(b)
	if _, err := db.ExecContext(t.Context(), `CREATE ROLE `+name+` `+attrs); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		var exists bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, name).
			Scan(&exists); err != nil || !exists {
			return
		}
		if _, err := db.ExecContext(ctx, `DROP OWNED BY `+name); err != nil {
			t.Errorf("drop what %s owns: %v", name, err)
		}
		if _, err := db.ExecContext(ctx, `DROP ROLE `+name); err != nil {
			t.Errorf("drop role %s: %v", name, err)
		}
	})
	return name
}

// An archive taken while a table was granted to a role the target cluster
// lacks (a reporting role, say) must still restore: grants are not restored,
// and under --exit-on-error a GRANT to a missing role would fail the whole
// load.
func TestPostgres_RestoreSkipsGrantsToAbsentRoles(t *testing.T) {
	storetest.RequirePostgresTool(t, "pg_dump")
	storetest.RequirePostgresTool(t, "pg_restore")
	dsn := storetest.PostgresDSN(t)
	db := openRaw(t, dsn)
	role := tempRole(t, db, "NOLOGIN")
	if _, err := db.ExecContext(t.Context(), `GRANT SELECT ON farms TO `+role); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "sqi.dump")
	if err := postgres.BackupDSN(t.Context(), dsn, out, nil); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DROP OWNED BY ` + role, `DROP ROLE ` + role} {
		if _, err := db.ExecContext(t.Context(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	dst := storetest.PostgresEmptyDSN(t)
	if err := postgres.Restore(t.Context(), dst, out, nil); err != nil {
		t.Fatalf("Restore of an archive granting to a role the target lacks: %v", err)
	}
	got, err := postgres.Open(t.Context(), dst, postgres.Options{MaxConns: 2})
	if err != nil {
		t.Fatalf("open the restored database: %v", err)
	}
	defer got.Close()
}

// RequireEmptyDatabase must see every table in the search path, including one
// owned by another role and granted to nobody, which information_schema hides
// from a role that cannot access it.
func TestPostgres_RequireEmptyDatabaseSeesTablesTheRoleCannotAccess(t *testing.T) {
	dsn := storetest.PostgresEmptyDSN(t)
	db := openRaw(t, dsn)
	owner := tempRole(t, db, "NOLOGIN")
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	password := hex.EncodeToString(b)
	restorer := tempRole(t, db, "LOGIN PASSWORD '"+password+"'")
	for _, q := range []string{
		`CREATE TABLE someone_elses (id int)`,
		`ALTER TABLE someone_elses OWNER TO ` + owner,
	} {
		if _, err := db.ExecContext(t.Context(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(restorer, password)

	err = postgres.RequireEmptyDatabase(t.Context(), u.String())
	if err == nil || !strings.Contains(err.Error(), "needs an empty database") ||
		!strings.Contains(err.Error(), "someone_elses") {
		t.Fatalf("RequireEmptyDatabase as a role that cannot see the table = %v, want a refusal naming it", err)
	}
}

// A file pg_restore cannot read is an unreadable archive; a canceled read or a
// pg_restore too old for the archive's format is not, and must not be reported
// as one (restore would call it "not an sqi backup").
func TestPostgres_ArchiveSchemaVersionReportsToolProblemsAsSuch(t *testing.T) {
	storetest.RequirePostgresTool(t, "pg_dump")
	storetest.RequirePostgresTool(t, "pg_restore")
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk.dump")
	if err := os.WriteFile(junk, []byte("not an archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.ArchiveSchemaVersion(t.Context(), junk); !errors.Is(err, postgres.ErrUnreadableArchive) {
		t.Fatalf("ArchiveSchemaVersion(junk) = %v, want ErrUnreadableArchive", err)
	}

	archive := filepath.Join(dir, "sqi.dump")
	if err := postgres.BackupDSN(t.Context(), storetest.PostgresDSN(t), archive, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := postgres.ArchiveSchemaVersion(ctx, archive)
	if !errors.Is(err, context.Canceled) || errors.Is(err, postgres.ErrUnreadableArchive) {
		t.Fatalf("ArchiveSchemaVersion(canceled) = %v, want context.Canceled and not ErrUnreadableArchive", err)
	}

	// The header's format version, raised past any pg_restore's: what an
	// archive from a newer pg_dump looks like to an older pg_restore.
	b, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if string(b[:5]) != "PGDMP" {
		t.Fatalf("archive header = %q, want PGDMP", b[:5])
	}
	b[6] = 99 // minor format version
	newer := filepath.Join(dir, "newer.dump")
	if err := os.WriteFile(newer, b, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = postgres.ArchiveSchemaVersion(t.Context(), newer)
	if err == nil || errors.Is(err, postgres.ErrUnreadableArchive) || !strings.Contains(err.Error(), "newer pg_dump") {
		t.Fatalf("ArchiveSchemaVersion(newer format) = %v, want a tool error naming a newer pg_dump", err)
	}
}

func TestArchiveSchemaVersion_AMissingToolIsNotAnUnreadableArchive(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := postgres.ArchiveSchemaVersion(t.Context(), filepath.Join(t.TempDir(), "sqi.dump"))
	if err == nil || errors.Is(err, postgres.ErrUnreadableArchive) || !strings.Contains(err.Error(), "not found on PATH") {
		t.Fatalf("ArchiveSchemaVersion without pg_restore = %v, want a missing-tool error", err)
	}
}

func TestPostgres_SchemaVersionDSN(t *testing.T) {
	if v, err := postgres.SchemaVersionDSN(t.Context(), storetest.PostgresEmptyDSN(t)); err != nil || v != 0 {
		t.Fatalf("SchemaVersionDSN(empty) = %d, %v; want 0", v, err)
	}
	maxV, err := migrations.MaxVersion("postgres")
	if err != nil {
		t.Fatal(err)
	}
	if v, err := postgres.SchemaVersionDSN(t.Context(), storetest.PostgresDSN(t)); err != nil || v != maxV {
		t.Fatalf("SchemaVersionDSN(migrated) = %d, %v; want %d", v, err, maxV)
	}
}
