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
	"runtime"
	"strings"
	"sync"
	"testing"
	"weak"

	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx"

	"github.com/uberware/sqi/internal/store/migrations"
	"github.com/uberware/sqi/internal/store/postgres"
	"github.com/uberware/sqi/internal/store/sqlstore"
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
	return postgresDatabase(t, true)
}

// PostgresEmptyDSN is [PostgresDSN] for an empty database: created with a
// plain CREATE DATABASE, no sqi template and no migrations, for a test that
// fills it some other way (a pg_restore, say). It is dropped when the test
// ends, and skips or fails without SQI_TEST_POSTGRES_DSN as PostgresDSN does.
func PostgresEmptyDSN(t testing.TB) string {
	t.Helper()
	return postgresDatabase(t, false)
}

// postgresDatabase creates a database for the test, cloned from the migrated
// template when migrated is set and empty otherwise, under the advisory lock
// that serializes creation across test processes, and returns its DSN.
func postgresDatabase(t testing.TB, migrated bool) string {
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

	name := "sqi_t_" + randomHex(t, 8)
	if migrated {
		tmpl := migratedTemplate(ctx, t, conn, admin)
		_, err = conn.ExecContext(ctx, `CREATE DATABASE `+name+` TEMPLATE `+tmpl+` STRATEGY FILE_COPY`)
	} else {
		_, err = conn.ExecContext(ctx, `CREATE DATABASE `+name)
	}
	if err != nil {
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

// migratedTemplate returns the name of the template database migrated to the
// current schema, creating it if this schema has none yet. The caller holds
// the creation lock.
func migratedTemplate(ctx context.Context, t testing.TB, conn *sql.Conn, admin string) string {
	t.Helper()
	tmpl := "sqi_tmpl_" + schemaHash(t)
	var exists bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, tmpl).Scan(&exists); err != nil {
		t.Fatalf("look up template: %v", err)
	}
	if exists {
		return tmpl
	}
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
	return tmpl
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
	// The lock-order checker sits beneath any wrapper the caller supplied, so
	// every transaction of every PostgreSQL test store is checked.
	check := newLockOrderDialect(t, nil)
	outer := opts.WrapDialect
	opts.WrapDialect = func(d sqlstore.Dialect) sqlstore.Dialect {
		check.Dialect = d
		var out sqlstore.Dialect = check
		if outer != nil {
			out = outer(out)
		}
		return out
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

// lockRank is each anchor table's place in the global lock order
// (docs/architecture.md, "Anchor rows").
var lockRank = map[string]int{"workers": 0, "jobs": 1, "queues": 2, "farms": 3, "usage_pools": 4, "users": 5}

// lockOrderDialect fails the test when a transaction takes an anchor out of
// the global lock order: a table ranked below one this transaction already
// locked, or rows of one table and mode out of id order within one call.
// (ReleaseBlockedJob and CancelBlockedJob lock their job exclusively, then its
// upstream jobs shared: the two modes are checked separately.)
//
// It remembers, per transaction, the rank of the last table locked. It never
// sees a commit or a rollback, so the entry cannot be dropped when its
// transaction ends; it is keyed by a weak pointer to the *sql.Tx instead, and
// dropped by a cleanup once that transaction is garbage. A strong key would
// keep every transaction of a long test alive, and clearing an entry on some
// other signal (such as a new transaction's first anchor) would either drop a
// live transaction's rank, missing an inversion, or never fire.
type lockOrderDialect struct {
	sqlstore.Dialect

	t    testing.TB
	mu   sync.Mutex
	last map[weak.Pointer[sql.Tx]]int
}

// newLockOrderDialect returns a lockOrderDialect over d reporting to t.
func newLockOrderDialect(t testing.TB, d sqlstore.Dialect) *lockOrderDialect {
	return &lockOrderDialect{Dialect: d, t: t, last: map[weak.Pointer[sql.Tx]]int{}}
}

// tracked returns how many transactions d holds a rank for.
func (d *lockOrderDialect) tracked() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.last)
}

func (d *lockOrderDialect) LockAnchors(ctx context.Context, tx *sql.Tx, anchors ...sqlstore.Anchor) error {
	key := weak.Make(tx)
	d.mu.Lock()
	prev, seen := d.last[key]
	if !seen {
		runtime.AddCleanup(tx, d.forget, key)
	}
	var before *sqlstore.Anchor // the anchor before a in this call
	for _, a := range anchors {
		r := lockRank[a.Table]
		if seen && r < prev {
			d.t.Errorf("lock order: %s locked after a higher-ranked row in the same transaction: %v", a.Table, anchors)
		}
		if before != nil && before.Table == a.Table && before.Mode == a.Mode && before.ID > a.ID {
			d.t.Errorf("lock order: %s rows not in id order: %v", a.Table, anchors)
		}
		prev, seen, before = r, true, &a
	}
	d.last[key] = prev
	d.mu.Unlock()
	return d.Dialect.LockAnchors(ctx, tx, anchors...)
}

// forget drops a transaction's rank once the transaction is garbage.
func (d *lockOrderDialect) forget(key weak.Pointer[sql.Tx]) {
	d.mu.Lock()
	delete(d.last, key)
	d.mu.Unlock()
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
