// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // register "pgx" driver
	"github.com/pressly/goose/v3"
	"github.com/spf13/cobra"
	_ "modernc.org/sqlite" // register "sqlite" driver

	"github.com/uberware/sqi/internal/pgdsn"
	"github.com/uberware/sqi/internal/store/backend"
	"github.com/uberware/sqi/internal/store/migrations"
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
  down    Roll back the most recently applied migration.
  status  Show applied and pending migrations.

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
		p, db, err := openMigrator(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

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

var migrateDownCmd = &cobra.Command{
	Use:   "down",
	Short: "Roll back the last applied migration",
	Long:  `Roll back exactly one migration — the most recently applied one.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		p, db, err := openMigrator(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		r, err := p.Down(cmd.Context())
		if err != nil {
			return fmt.Errorf("migrate down: %w", err)
		}
		fmt.Fprintln(os.Stdout, r)
		return nil
	},
}

var migrateStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show applied and pending migrations",
	Long: `List every migration file with its current state: the time it was
applied (UTC, RFC 3339), or "pending".`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		p, db, err := openMigrator(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		statuses, err := p.Status(cmd.Context())
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
	// --db and --dsn on the parent so all three subcommands inherit them.
	migrateCmd.PersistentFlags().StringVar(
		&migrateDBPath,
		"db", "sqi.db",
		"path to SQLite database file (defaults to store.sqlite_path from config, then the deprecated SQI_SQLITE_PATH, then \"sqi.db\")",
	)
	migrateCmd.PersistentFlags().StringVar(
		&migrateDSN,
		"dsn", "",
		"PostgreSQL connection string (defaults to store.postgres.dsn when store.driver is postgres)",
	)

	migrateCmd.AddCommand(migrateUpCmd, migrateDownCmd, migrateStatusCmd)
}

// openMigrator opens the target database and returns a goose provider for it.
// SQLite gets the store's pragmas first, and its file is created if missing
// (that is migrate's job); PostgreSQL must already exist.
func openMigrator(cmd *cobra.Command) (*goose.Provider, *sql.DB, error) {
	tgt, err := resolveStoreTarget(cmd, migrateDBPath, migrateDSN)
	if err != nil {
		return nil, nil, err
	}
	var db *sql.DB
	driver := tgt.Driver
	if driver == backend.DriverPostgres {
		if db, err = sql.Open("pgx", tgt.DSN); err != nil {
			return nil, nil, fmt.Errorf("open %s: %w", pgdsn.Redact(tgt.DSN), err)
		}
	} else if db, err = openMigrateDB(tgt.SQLitePath); err != nil {
		return nil, nil, err
	}
	p, err := migrations.NewProvider(db, driver)
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
	if path == "" {
		return nil, errors.New("database path is empty; use --db, set store.sqlite_path, or set SQI_STORE_SQLITE_PATH")
	}

	db, err := sql.Open("sqlite", path)
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
