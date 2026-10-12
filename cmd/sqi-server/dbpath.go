// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib" // register "pgx" driver
	"github.com/spf13/cobra"
	_ "modernc.org/sqlite" // register "sqlite" driver

	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/pgdsn"
	"github.com/uberware/sqi/internal/store/backend"
	"github.com/uberware/sqi/internal/store/postgres"
)

// resolveDBPath applies the database-path precedence shared by backup,
// migrate, and worker, highest priority first:
//
//  1. explicit is used as-is when explicitChanged is true (the operator
//     passed --db on the command line).
//  2. The config layer: the root -c/--config file and SQI_STORE_SQLITE_PATH,
//     i.e. whatever [config.LoadWithSources] resolves store.sqlite_path to,
//     when [config.Sources.StoreSQLitePath] reports the file or env var
//     actually set it — NOT when the resolved value merely differs from the
//     built-in default, which a config file that restates the default
//     value (as config/sqi-server.example.yaml does) would defeat.
//  3. The legacy SQI_SQLITE_PATH environment variable, kept working as an
//     alias. A deprecation notice naming SQI_STORE_SQLITE_PATH — the
//     variable sqi-server itself reads — is printed to stderr when this is
//     the layer that decided the path.
//  4. The built-in default ("sqi.db").
//
// The config layer is always loaded, even when an explicit --db makes its
// result moot, so a malformed --config file is reported as an error rather
// than silently ignored when the operator asked for a specific one (root
// -c/--config was passed explicitly). Without an explicit -c, a config-load
// failure — an auto-discovered but broken /etc/sqi/sqi-server.yaml, or an
// unrelated malformed SQI_* env var with nothing to do with the database
// path — is NOT a hard failure: backup, migrate, and worker are the tools
// reached for when something is already broken, so a warning goes to
// stderr and resolution falls through to the legacy env var and default as
// if the config layer had decided nothing.
func resolveDBPath(explicit string, explicitChanged bool) (string, error) {
	cfg, src, err := config.LoadWithSources(persistentFlags.ConfigFile, config.FlagOverrides{})
	if err != nil {
		if persistentFlags.ConfigFile != "" {
			return "", fmt.Errorf("load config: %w", err)
		}
		fmt.Fprintf(
			os.Stderr,
			"warning: could not load configuration (%v); falling back to SQI_SQLITE_PATH or the built-in default\n",
			err,
		)
		cfg = config.DefaultConfig()
		src = config.Sources{}
	}

	if explicitChanged {
		return explicit, nil
	}

	if src.StoreSQLitePath {
		return cfg.Store.SQLitePath, nil
	}

	if legacy := envOr("SQI_SQLITE_PATH", ""); legacy != "" {
		fmt.Fprintln(os.Stderr,
			"warning: SQI_SQLITE_PATH is deprecated; set SQI_STORE_SQLITE_PATH instead, "+
				"which is the variable sqi-server itself reads")
		return legacy, nil
	}

	return cfg.Store.SQLitePath, nil
}

// requireExistingDB stats path and returns an actionable error naming it and
// how to point elsewhere when no file exists there. Used by backup and
// worker, which must never create a database — unlike migrate, whose job is
// to create one.
func requireExistingDB(path string) error {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf(
				"no database at %s; point --db, store.sqlite_path, or SQI_STORE_SQLITE_PATH at the right file, "+
					"or run \"sqi-server migrate up\" to create one there",
				path,
			)
		}
		return fmt.Errorf("stat %s: %w", path, err)
	}
	return nil
}

// requireMigratedDB catches the case requireExistingDB's stat check cannot
// see: a file that exists at path but was never migrated (empty, or created
// by something other than "migrate up" or the server's own AutoMigrate).
// sqlite.Open with AutoMigrate: false succeeds against such a file — SQLite
// opens an empty database happily — so the first real query would otherwise
// fail with a raw driver error ("no such table: ..."). Checked directly
// against sqlite_master rather than through the store, so it runs before
// any store method that would surface that error unremediated.
func requireMigratedDB(path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer db.Close()

	var count int
	err = db.QueryRowContext(
		context.Background(),
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`,
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("check schema at %s: %w", path, err)
	}
	if count == 0 {
		return fmt.Errorf("the database at %s has no schema; run \"sqi-server migrate up\" first", path)
	}
	return nil
}

// storeTarget is the store a CLI command reads and writes.
type storeTarget struct{ Driver, SQLitePath, DSN string }

// resolveStoreTarget decides which store a CLI command uses. In order: an
// explicit --dsn (PostgreSQL); an explicit --db (SQLite at that path); the
// configured store.driver, with store.postgres.dsn for PostgreSQL and
// resolveDBPath's layering for SQLite. Configuration-load failures follow
// resolveDBPath's rules.
func resolveStoreTarget(cmd *cobra.Command, dbFlag, dsnFlag string) (storeTarget, error) {
	if flagChanged(cmd, "dsn") {
		if dsnFlag == "" {
			return storeTarget{}, errors.New("--dsn is empty")
		}
		return storeTarget{Driver: backend.DriverPostgres, DSN: dsnFlag}, nil
	}
	if !flagChanged(cmd, "db") {
		cfg, _, err := config.LoadWithSources(persistentFlags.ConfigFile, config.FlagOverrides{})
		if err != nil && persistentFlags.ConfigFile != "" {
			return storeTarget{}, fmt.Errorf("load config: %w", err)
		}
		if err == nil && cfg.Store.Driver == config.StoreDriverPostgres {
			if cfg.Store.Postgres.DSN == "" {
				return storeTarget{}, errors.New("store.driver is postgres but no DSN is set; " +
					"use --dsn, set store.postgres.dsn, or set SQI_STORE_POSTGRES_DSN")
			}
			return storeTarget{Driver: backend.DriverPostgres, DSN: cfg.Store.Postgres.DSN}, nil
		}
	}
	path, err := resolveDBPath(dbFlag, flagChanged(cmd, "db"))
	if err != nil {
		return storeTarget{}, err
	}
	return storeTarget{Driver: backend.DriverSQLite, SQLitePath: path}, nil
}

// flagChanged reports whether the operator passed the flag name to cmd. A nil
// cmd (a direct, non-cobra caller) passed nothing.
func flagChanged(cmd *cobra.Command, name string) bool {
	if cmd == nil {
		return false
	}
	f := cmd.Flags().Lookup(name)
	return f != nil && f.Changed
}

// requireMigratedPostgres fails, with the same advice as requireMigratedDB,
// when the database has no schema.
func requireMigratedPostgres(ctx context.Context, dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open %s: %w", pgdsn.Redact(dsn), err)
	}
	defer db.Close()
	v, err := postgres.SchemaVersion(ctx, db)
	if err != nil {
		return fmt.Errorf("check schema at %s: %w", pgdsn.Redact(dsn), err)
	}
	if v == 0 {
		return fmt.Errorf("the database at %s has no schema; run \"sqi-server migrate up\" first", pgdsn.Redact(dsn))
	}
	return nil
}

// openExistingStore opens tgt's store without migrating it, after checking it
// exists and has a schema. These commands write into an existing schema; they
// never create one.
func openExistingStore(ctx context.Context, tgt storeTarget) (backend.Backend, error) {
	switch tgt.Driver {
	case backend.DriverPostgres:
		if err := requireMigratedPostgres(ctx, tgt.DSN); err != nil {
			return nil, err
		}
		st, err := backend.Open(ctx, backend.Options{
			Driver: backend.DriverPostgres, PostgresDSN: tgt.DSN, MaxConns: 2, AutoMigrate: false,
		})
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", pgdsn.Redact(tgt.DSN), err)
		}
		return st, nil
	default:
		if tgt.SQLitePath == "" {
			return nil, errors.New("database path is empty; use --db, set store.sqlite_path, or set SQI_STORE_SQLITE_PATH")
		}
		if err := requireExistingDB(tgt.SQLitePath); err != nil {
			return nil, err
		}
		if err := requireMigratedDB(tgt.SQLitePath); err != nil {
			return nil, err
		}
		st, err := backend.Open(ctx, backend.Options{SQLitePath: tgt.SQLitePath, AutoMigrate: false})
		if err != nil {
			return nil, fmt.Errorf("open database: %w", err)
		}
		return st, nil
	}
}
