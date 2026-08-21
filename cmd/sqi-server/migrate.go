// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/pressly/goose/v3"
	"github.com/spf13/cobra"
	_ "modernc.org/sqlite" // register "sqlite" driver

	"github.com/uberware/sqi/internal/store/migrations"
)

// migrateDBPath is the SQLite file path used by all migrate subcommands.
// It is set via --db, or resolved from configuration — see [resolveDBPath].
var migrateDBPath string

// migrateCmd groups SQLite schema migration subcommands.
var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Manage SQLite schema migrations",
	Long: `Manage SQLite schema migrations for the sqi-server state store.

Subcommands:
  up      Apply all pending migrations.
  down    Roll back the most recently applied migration.
  status  Show applied and pending migrations.

The database path defaults to store.sqlite_path from the resolved
configuration (the root -c/--config file and SQI_STORE_SQLITE_PATH), falling
back to the legacy SQI_SQLITE_PATH environment variable and then to "sqi.db"
in the working directory — the same path the server uses, so running
"migrate up" before "serve" is the standard deployment initialization step.
Pass --db to override it explicitly. Unlike backup and worker, migrate
creates the database file when it does not already exist — that is its job.`,
	// No RunE — bare "migrate" prints usage.
}

var migrateUpCmd = &cobra.Command{
	Use:   "up",
	Short: "Apply all pending migrations",
	Long:  `Apply every migration that has not yet been run against the target database.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		path, err := resolveDBPath(migrateDBPath, cmd.Flags().Changed("db"))
		if err != nil {
			return err
		}
		db, err := openMigrateDB(path)
		if err != nil {
			return err
		}
		defer db.Close()

		if err := goose.Up(db, "."); err != nil {
			return fmt.Errorf("migrate up: %w", err)
		}
		return nil
	},
}

var migrateDownCmd = &cobra.Command{
	Use:   "down",
	Short: "Roll back the last applied migration",
	Long:  `Roll back exactly one migration — the most recently applied one.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		path, err := resolveDBPath(migrateDBPath, cmd.Flags().Changed("db"))
		if err != nil {
			return err
		}
		db, err := openMigrateDB(path)
		if err != nil {
			return err
		}
		defer db.Close()

		if err := goose.Down(db, "."); err != nil {
			return fmt.Errorf("migrate down: %w", err)
		}
		return nil
	},
}

var migrateStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show applied and pending migrations",
	Long:  `List every migration file with its current state: applied (✓) or pending (○).`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		path, err := resolveDBPath(migrateDBPath, cmd.Flags().Changed("db"))
		if err != nil {
			return err
		}
		db, err := openMigrateDB(path)
		if err != nil {
			return err
		}
		defer db.Close()

		if err := goose.Status(db, "."); err != nil {
			return fmt.Errorf("migrate status: %w", err)
		}
		return nil
	},
}

func init() {
	// --db flag on the parent so all three subcommands inherit it.
	migrateCmd.PersistentFlags().StringVar(
		&migrateDBPath,
		"db", "sqi.db",
		"path to SQLite database file (defaults to store.sqlite_path from config, or SQI_SQLITE_PATH)",
	)

	migrateCmd.AddCommand(migrateUpCmd, migrateDownCmd, migrateStatusCmd)
}

// openMigrateDB opens the SQLite database at path, configures WAL mode and
// foreign-key enforcement, and wires goose to use the embedded migration FS.
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

	// Point goose at the embedded FS and set the SQLite dialect.
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set goose dialect: %w", err)
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
