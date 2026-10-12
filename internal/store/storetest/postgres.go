// SPDX-License-Identifier: AGPL-3.0-or-later

package storetest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx"

	"github.com/uberware/sqi/internal/store/migrations"
	"github.com/uberware/sqi/internal/store/postgres"
)

// PostgresDSN returns the DSN of a fresh, migrated database, dropped when the
// test ends. SQI_TEST_POSTGRES_DSN names an administrative connection to a
// server the tests may create databases on (make test-postgres provides one).
// Without it the test is skipped, unless SQI_TEST_REQUIRE_POSTGRES=1, when it
// fails: a skip verifies nothing, and CI must not pass on one.
//
// Each database is cloned from a template migrated once per schema; the
// template's name carries a hash of the migration files, so a changed schema
// gets a new template. Creation is serialized across test processes with an
// advisory lock, because go test runs packages in parallel processes.
func PostgresDSN(t testing.TB) string {
	t.Helper()
	admin := os.Getenv("SQI_TEST_POSTGRES_DSN")
	if admin == "" {
		if os.Getenv("SQI_TEST_REQUIRE_POSTGRES") == "1" {
			t.Fatal("SQI_TEST_REQUIRE_POSTGRES=1 but SQI_TEST_POSTGRES_DSN is unset")
		}
		t.Skip("PostgreSQL not configured: set SQI_TEST_POSTGRES_DSN, or run make test-postgres")
	}
	ctx := context.Background()
	db, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("admin connection: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(7371692)`); err != nil {
		t.Fatalf("lock template creation: %v", err)
	}
	defer conn.ExecContext(ctx, `SELECT pg_advisory_unlock(7371692)`) //nolint:errcheck // released with the connection anyway

	tmpl := "sqi_tmpl_" + schemaHash(t)
	var exists bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, tmpl).Scan(&exists); err != nil {
		t.Fatalf("look up template: %v", err)
	}
	if !exists {
		if _, err := conn.ExecContext(ctx, `CREATE DATABASE `+tmpl); err != nil {
			t.Fatalf("create template: %v", err)
		}
		st, err := postgres.Open(ctx, withDatabase(t, admin, tmpl), postgres.DefaultOptions())
		if err != nil {
			// A half-migrated template would be cloned by every later run.
			_, derr := conn.ExecContext(ctx, `DROP DATABASE `+tmpl+` WITH (FORCE)`)
			t.Fatalf("migrate template: %v (dropping it: %v)", err, derr)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("close template: %v", err)
		}
	}
	name := "sqi_t_" + randomHex(t, 8)
	if _, err := conn.ExecContext(ctx, `CREATE DATABASE `+name+` TEMPLATE `+tmpl+` STRATEGY FILE_COPY`); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		d, err := sql.Open("pgx", admin)
		if err != nil {
			t.Errorf("drop %s: %v", name, err)
			return
		}
		defer d.Close()
		if _, err := d.ExecContext(context.Background(), `DROP DATABASE `+name+` WITH (FORCE)`); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	return withDatabase(t, admin, name)
}

// Postgres opens a store on a fresh database (see PostgresDSN), closed when
// the test ends. SQI_TEST_PG_FORCE_TX_RETRY=1 opens it with every transaction
// rehearsed once (postgres.Options.ForceTxRetry).
func Postgres(t testing.TB) *postgres.Store {
	t.Helper()
	return PostgresWith(t, postgres.DefaultOptions())
}

// PostgresWith is Postgres with explicit options.
func PostgresWith(t testing.TB, opts postgres.Options) *postgres.Store {
	t.Helper()
	dsn := PostgresDSN(t)
	if os.Getenv("SQI_TEST_PG_FORCE_TX_RETRY") == "1" {
		opts.ForceTxRetry = true
	}
	st, err := postgres.Open(context.Background(), dsn, opts)
	if err != nil {
		t.Fatalf("postgres.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close postgres store: %v", err)
		}
	})
	return st
}

// withDatabase returns dsn (URL form) pointed at database name.
func withDatabase(t testing.TB, dsn, name string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" {
		t.Fatalf("SQI_TEST_POSTGRES_DSN must be a postgres:// URL: %q", dsn)
	}
	u.Path = "/" + name
	return u.String()
}

func schemaHash(t testing.TB) string {
	t.Helper()
	h := sha256.New()
	err := fs.WalkDir(migrations.PostgresFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(migrations.PostgresFS, p)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\n%s\n", p, b)
		return nil
	})
	if err != nil {
		t.Fatalf("hash migrations: %v", err)
	}
	return strings.ToLower(hex.EncodeToString(h.Sum(nil))[:12])
}

func randomHex(t testing.TB, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
