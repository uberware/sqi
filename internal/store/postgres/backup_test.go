// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres_test

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/uberware/sqi/internal/pgdsn"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/postgres"
	"github.com/uberware/sqi/internal/store/storetest"
)

// requirePgRestore skips the test without the PostgreSQL client tools, or
// fails it when SQI_TEST_REQUIRE_POSTGRES=1: a skip verifies nothing.
func requirePgRestore(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("pg_restore"); err != nil {
		if os.Getenv("SQI_TEST_REQUIRE_POSTGRES") == "1" {
			t.Fatal("pg_restore not on PATH; install the PostgreSQL client tools")
		}
		t.Skip("pg_restore not on PATH")
	}
}

// TestPostgres_BackupRestoreRoundTrip backs a database up with Backup,
// restores it into an empty database with pg_restore, and reads a row back.
func TestPostgres_BackupRestoreRoundTrip(t *testing.T) {
	requirePgRestore(t)
	src := storetest.Postgres(t)
	if _, err := src.CreateFarm(t.Context(), store.Farm{ID: "farm-backup", Name: "kept"}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "sqi.dump")
	if err := src.Backup(t.Context(), out); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	// Backup creates the archive owner-only and pg_dump writes into it: the
	// dump holds every account's password hash.
	if fi, err := os.Stat(out); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Fatalf("archive = %v, %v; want mode 0600", fi, err)
	}
	if err := src.Backup(t.Context(), out); err == nil {
		t.Fatal("Backup overwrote an existing archive")
	}

	dst := storetest.PostgresEmptyDSN(t)
	restore := exec.CommandContext(t.Context(), "pg_restore", "--no-owner", "--dbname="+dst, out)
	if b, err := restore.CombinedOutput(); err != nil {
		t.Fatalf("pg_restore: %v\n%s", err, b)
	}
	got, err := postgres.Open(t.Context(), dst, postgres.Options{MaxConns: 2})
	if err != nil {
		t.Fatalf("open the restored database: %v", err)
	}
	defer got.Close()
	if f, err := got.GetFarm(t.Context(), "farm-backup"); err != nil || f.Name != "kept" {
		t.Fatalf("restored farm = %+v, %v", f, err)
	}
}

// TestPostgres_BackupDSNForms runs Backup with the password in each place a
// DSN can carry it, so the real pg_dump (libpq) reads the DSN the password was
// taken out of, and authenticates with the password from PGPASSWORD alone.
func TestPostgres_BackupDSNForms(t *testing.T) {
	requirePgRestore(t)
	t.Setenv("PGPASSWORD", "")
	t.Setenv("PGPASSFILE", filepath.Join(t.TempDir(), "no-pgpass"))
	base := storetest.PostgresDSN(t)
	cfg, err := pgconn.ParseConfig(base)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Password == "" {
		t.Skip("SQI_TEST_POSTGRES_DSN carries no password; nothing to keep off the command line")
	}
	user, pw := url.QueryEscape(cfg.User), url.QueryEscape(cfg.Password)
	hostPort := net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port)))
	forms := map[string]string{
		"url userinfo": fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", user, pw, hostPort, cfg.Database),
		"url query": fmt.Sprintf("postgresql://%s@%s/%s?password=%s&sslmode=disable&application_name=sqi%%20backup",
			user, hostPort, cfg.Database, pw),
		"keyword/value": fmt.Sprintf("host=%s port=%d user=%s password='%s' dbname=%s sslmode=disable application_name='sqi backup'",
			cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Database),
	}
	// Without PGPASSWORD the same password-free DSN must fail, or the passes
	// below would not show that PGPASSWORD is what authenticated.
	rest, ok := pgdsn.WithoutPassword(forms["url userinfo"])
	if !ok {
		t.Fatal("WithoutPassword refused the test DSN")
	}
	probe := exec.CommandContext(t.Context(), "pg_dump", "--no-password", "--schema-only",
		"--file="+filepath.Join(t.TempDir(), "probe.sql"), "--dbname="+rest)
	if b, err := probe.CombinedOutput(); err == nil {
		t.Logf("the server accepts %s with no password; this test cannot show PGPASSWORD authenticating", rest)
	} else {
		t.Logf("without PGPASSWORD pg_dump fails as it should: %s", b)
	}
	for name, dsn := range forms {
		t.Run(name, func(t *testing.T) {
			st, err := postgres.Open(t.Context(), dsn, postgres.Options{MaxConns: 2})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer st.Close()
			out := filepath.Join(t.TempDir(), "sqi.dump")
			if err := st.Backup(t.Context(), out); err != nil {
				t.Fatalf("Backup: %v", err)
			}
			if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
				t.Fatalf("archive: %v, %v", fi, err)
			}
		})
	}
}
