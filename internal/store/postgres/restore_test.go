// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres_test

import (
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
	src := storetest.Postgres(t)
	if _, err := src.CreateFarm(t.Context(), store.Farm{ID: "farm-restore", Name: "kept"}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "sqi.dump")
	if err := src.Backup(t.Context(), out); err != nil {
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

func TestPostgres_RequireMigratedDatabase(t *testing.T) {
	if err := postgres.RequireMigratedDatabase(t.Context(), storetest.PostgresEmptyDSN(t)); err == nil ||
		!strings.Contains(err.Error(), "no sqi schema") {
		t.Fatalf("RequireMigratedDatabase(empty) = %v, want a refusal", err)
	}
	if err := postgres.RequireMigratedDatabase(t.Context(), storetest.PostgresDSN(t)); err != nil {
		t.Fatalf("RequireMigratedDatabase(migrated) = %v", err)
	}
}
