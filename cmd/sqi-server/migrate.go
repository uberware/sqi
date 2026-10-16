// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // register "pgx" driver
	"github.com/pressly/goose/v3"
	"github.com/spf13/cobra"
	_ "modernc.org/sqlite" // register "sqlite" driver

	"github.com/uberware/sqi/internal/pgdsn"
	"github.com/uberware/sqi/internal/store/backend"
	"github.com/uberware/sqi/internal/store/migrations"
	"github.com/uberware/sqi/internal/store/postgres"
	"github.com/uberware/sqi/internal/store/sqlite"
)

// migrateDBPath is the SQLite file path used by all migrate subcommands.
// It is set via --db, or resolved from configuration — see [resolveStoreTarget].
var migrateDBPath string

// migrateDSN is the PostgreSQL connection string used by all migrate
// subcommands, set via --dsn — see [resolveStoreTarget].
var migrateDSN string

// migrateCmd groups state store schema migration subcommands.
var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Manage state store schema migrations (SQLite or PostgreSQL)",
	Long: `Manage state store schema migrations (SQLite or PostgreSQL) for the
sqi-server state store.

Subcommands:
  up      Apply all pending migrations.
  status  Show applied and pending migrations.

Migrations are forward-only. There is no "migrate down": to go back to an older
sqi-server, restore the backup taken before the upgrade (sqi-server restore).

The store is chosen as follows: --dsn names a PostgreSQL database; --db names
a SQLite file; otherwise store.driver from the resolved configuration (the
root -c/--config file and SQI_STORE_DRIVER) decides. For PostgreSQL the
connection string is store.postgres.dsn (SQI_STORE_POSTGRES_DSN), and the
database itself must already exist. For SQLite the path is
store.sqlite_path (SQI_STORE_SQLITE_PATH), falling back to the legacy
SQI_SQLITE_PATH environment variable and then to "sqi.db" in the working
directory — the same store the server uses, so running "migrate up" before
"serve" is the standard deployment initialization step. Unlike backup and
worker, migrate creates the SQLite file when it does not already exist — that
is its job.`,
	// No RunE — bare "migrate" prints usage.
}

var migrateUpCmd = &cobra.Command{
	Use:   "up",
	Short: "Apply all pending migrations",
	Long:  `Apply every migration that has not yet been run against the target database.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		tgt, err := resolveStoreTarget(cmd, migrateDBPath, migrateDSN)
		if err != nil {
			return fmt.Errorf("migrate up: %w", err)
		}
		// Migrating beside a running server would race its own AutoMigrate and
		// change the schema under it, so take the single-server lock first.
		release, err := backend.LockStore(cmd.Context(), tgt.options())
		if err != nil {
			return fmt.Errorf("migrate up: %w", err)
		}
		defer func() { _ = release() }() //nolint:errcheck // releasing on exit; the OS drops it anyway

		p, db, err := openMigrator(tgt)
		if err != nil {
			return err
		}
		defer db.Close()

		// Refuse a schema newer than this binary.
		cur, err := p.GetDBVersion(cmd.Context())
		if err != nil {
			return fmt.Errorf("migrate up: read schema version: %w", err)
		}
		if err := migrations.CheckVersion(tgt.Driver, cur); err != nil {
			return fmt.Errorf("migrate up: %w", err)
		}

		results, err := p.Up(cmd.Context())
		if err != nil {
			return fmt.Errorf("migrate up: %w", err)
		}
		if len(results) == 0 {
			fmt.Fprintln(os.Stdout, "no pending migrations")
		}
		for _, r := range results {
			fmt.Fprintln(os.Stdout, r)
		}
		return nil
	},
}

var migrateStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show applied and pending migrations",
	Long: `List every migration file with its current state: the time it was
applied (UTC, RFC 3339), or "pending".

It writes nothing: a database never migrated lists every migration pending
and is left empty. On SQLite it only reads the file, so it is safe to run on a
backup; a missing file lists every migration pending and is not created.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		statuses, err := migrationStatuses(cmd)
		if err != nil {
			return fmt.Errorf("migrate status: %w", err)
		}
		for _, m := range statuses {
			applied := "pending"
			if m.State == goose.StateApplied {
				applied = m.AppliedAt.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(os.Stdout, "%-25s %s\n", applied, m.Source.Path)
		}
		return nil
	},
}

func init() {
	// --db and --dsn on the parent so both subcommands inherit them.
	addStoreFlags(migrateCmd.PersistentFlags(), &migrateDBPath, &migrateDSN, "path to SQLite database file")

	migrateCmd.AddCommand(migrateUpCmd, migrateStatusCmd)
}

// migrationStatuses lists every migration with its state, writing nothing.
// goose's Status creates its version table, so a database with no schema is
// listed as all pending without calling it (on PostgreSQL that table would
// make restore refuse the database as not empty). On SQLite a missing file is
// not opened, and an existing one is read over a read-only connection, because
// the read-write open migrate up uses rewrites the file.
func migrationStatuses(cmd *cobra.Command) ([]*goose.MigrationStatus, error) {
	tgt, err := resolveStoreTarget(cmd, migrateDBPath, migrateDSN)
	if err != nil {
		return nil, err
	}
	if tgt.Driver == backend.DriverPostgres {
		p, db, err := openMigrator(tgt)
		if err != nil {
			return nil, err
		}
		defer db.Close()
		v, err := postgres.SchemaVersion(cmd.Context(), db)
		if err != nil {
			return nil, err
		}
		if v == 0 {
			return allPending(p), nil
		}
		return p.Status(cmd.Context())
	}

	dsn := ":memory:" // never connected to: it only lets the provider list the migrations
	if _, err := os.Stat(tgt.SQLitePath); errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "no database at %s yet: every migration is pending\n", tgt.SQLitePath)
	} else if err != nil {
		return nil, err
	} else if dsn, err = sqlite.FileURI(tgt.SQLitePath, true); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", tgt.SQLitePath, err)
	}
	defer db.Close()
	p, err := migrations.NewProvider(db, "sqlite")
	if err != nil {
		return nil, err
	}
	v := int64(0)
	if dsn != ":memory:" {
		if v, err = sqlite.SchemaVersion(cmd.Context(), db); err != nil {
			return nil, err
		}
	}
	if v == 0 {
		return allPending(p), nil
	}
	return p.Status(cmd.Context())
}

// allPending is every migration p knows, each pending.
func allPending(p *goose.Provider) []*goose.MigrationStatus {
	sources := p.ListSources()
	out := make([]*goose.MigrationStatus, 0, len(sources))
	for _, src := range sources {
		out = append(out, &goose.MigrationStatus{Source: src, State: goose.StatePending})
	}
	return out
}

// openMigrator opens tgt's database and returns a goose provider for it.
// SQLite gets the store's pragmas first, and its file is created if missing
// (that is migrate's job); PostgreSQL must already exist.
func openMigrator(tgt storeTarget) (*goose.Provider, *sql.DB, error) {
	var db *sql.DB
	var err error
	if tgt.Driver == backend.DriverPostgres {
		if db, err = sql.Open("pgx", tgt.DSN); err != nil {
			return nil, nil, fmt.Errorf("open %s: %w", pgdsn.Redact(tgt.DSN), err)
		}
	} else if db, err = openMigrateDB(tgt.SQLitePath); err != nil {
		return nil, nil, err
	}
	p, err := migrations.NewProvider(db, tgt.Driver)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return p, db, nil
}

// openMigrateDB opens the SQLite database at path (creating the file when it
// is missing) and configures WAL mode, foreign-key enforcement and the busy
// timeout the store uses.
func openMigrateDB(path string) (*sql.DB, error) {
	dsn, err := sqlite.DSN(path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}

	// Apply SQLite pragmas that match what the store uses.
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	}
	for _, p := range pragmas {
		if _, err := db.ExecContext(context.Background(), p); err != nil {
			db.Close()
			return nil, fmt.Errorf("set pragma %q: %w", p, err)
		}
	}

	return db, nil
}

// envOr returns the value of the environment variable key if set and non-empty,
// otherwise it returns fallback.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
