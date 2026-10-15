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
	"github.com/spf13/pflag"
	_ "modernc.org/sqlite" // register "sqlite" driver

	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/pgdsn"
	"github.com/uberware/sqi/internal/store/backend"
	"github.com/uberware/sqi/internal/store/postgres"
	"github.com/uberware/sqi/internal/store/sqlite"
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
	c, err := loadCLIConfig()
	if err != nil {
		return "", err
	}
	return c.dbPath(explicit, explicitChanged), nil
}

// cliConfig is the configuration a CLI command resolves its store from, loaded
// once by [loadCLIConfig]. When loading failed without an explicit -c, cfg is
// the built-in default, src is empty and loadErr holds the failure, which the
// lenient fallbacks report.
type cliConfig struct {
	cfg     config.Config
	src     config.Sources
	loadErr error
}

// loadCLIConfig loads the configuration for a CLI command. A failure is an
// error only when -c was passed explicitly (see [resolveDBPath]).
func loadCLIConfig() (cliConfig, error) {
	cfg, src, err := config.LoadWithSources(persistentFlags.ConfigFile, config.FlagOverrides{})
	if err != nil {
		if persistentFlags.ConfigFile != "" {
			return cliConfig{}, fmt.Errorf("load config: %w", err)
		}
		return cliConfig{cfg: config.DefaultConfig(), loadErr: err}, nil
	}
	return cliConfig{cfg: cfg, src: src}, nil
}

// dbPath applies [resolveDBPath]'s precedence to c.
func (c cliConfig) dbPath(explicit string, explicitChanged bool) string {
	if c.loadErr != nil {
		fmt.Fprintf(
			os.Stderr,
			"warning: could not load configuration (%v); falling back to SQI_SQLITE_PATH or the built-in default\n",
			c.loadErr,
		)
	}

	if explicitChanged {
		return explicit
	}

	if c.src.StoreSQLitePath {
		return c.cfg.Store.SQLitePath
	}

	if legacy := envOr("SQI_SQLITE_PATH", ""); legacy != "" {
		fmt.Fprintln(os.Stderr,
			"warning: SQI_SQLITE_PATH is deprecated; set SQI_STORE_SQLITE_PATH instead, "+
				"which is the variable sqi-server itself reads")
		return legacy
	}

	return c.cfg.Store.SQLitePath
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
	dsn, err := sqlite.DSN(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	db, err := sql.Open("sqlite", dsn)
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
// resolveDBPath's layering for SQLite.
//
// Configuration-load failures follow resolveDBPath's rules, with two
// tightenings. An explicit -c must load whichever flag decides the store, so
// "-c broken.yaml --dsn ..." fails as "-c broken.yaml --db ..." does. And the
// no -c lenience (fall back to SQLite) stops where SQI_STORE_DRIVER names
// another driver, because the failed load has discarded that setting and
// SQLite is then the wrong store, not a safe default. A driver the server
// would reject is an error, never SQLite.
func resolveStoreTarget(cmd *cobra.Command, dbFlag, dsnFlag string) (storeTarget, error) {
	if flagChanged(cmd, "dsn") && dsnFlag == "" {
		return storeTarget{}, errors.New("--dsn is empty")
	}
	c, err := loadCLIConfig()
	if err != nil {
		return storeTarget{}, err
	}
	if flagChanged(cmd, "dsn") {
		return storeTarget{Driver: backend.DriverPostgres, DSN: dsnFlag}, nil
	}
	if !flagChanged(cmd, "db") {
		tgt, decided, err := c.configuredStoreTarget()
		if err != nil || decided {
			return tgt, err
		}
	}
	return storeTarget{Driver: backend.DriverSQLite, SQLitePath: c.dbPath(dbFlag, flagChanged(cmd, "db"))}, nil
}

// configuredStoreTarget resolves the store from configuration alone. It
// reports decided only for PostgreSQL; SQLite (and the lenient fallback after
// a failed load without -c) is left to resolveDBPath's layering.
func (c cliConfig) configuredStoreTarget() (tgt storeTarget, decided bool, err error) {
	if c.loadErr != nil {
		if d := os.Getenv("SQI_STORE_DRIVER"); d != "" && d != config.StoreDriverSQLite {
			return storeTarget{}, false, fmt.Errorf(
				"load config: %w (SQI_STORE_DRIVER is %q, so falling back to SQLite would use the wrong store)", c.loadErr, d,
			)
		}
		return storeTarget{}, false, nil
	}
	cfg := c.cfg
	switch cfg.Store.Driver {
	case "", config.StoreDriverSQLite:
		return storeTarget{}, false, nil
	case config.StoreDriverPostgres:
		if cfg.Store.Postgres.DSN == "" {
			return storeTarget{}, false, errors.New("store.driver is postgres but no DSN is set; " +
				"use --dsn, set store.postgres.dsn, or set SQI_STORE_POSTGRES_DSN")
		}
		return storeTarget{Driver: backend.DriverPostgres, DSN: cfg.Store.Postgres.DSN}, true, nil
	default:
		return storeTarget{}, false, fmt.Errorf(
			"store.driver must be sqlite or postgres, got %q; set SQI_STORE_DRIVER or store.driver", cfg.Store.Driver,
		)
	}
}

// addStoreFlags registers the --db and --dsn flags that [resolveStoreTarget]
// reads, on fs. dbWhat is the start of --db's help text.
func addStoreFlags(fs *pflag.FlagSet, db, dsn *string, dbWhat string) {
	fs.StringVar(db, "db", "sqi.db", dbWhat+
		" (defaults to store.sqlite_path from config, then the deprecated SQI_SQLITE_PATH, then \"sqi.db\")")
	fs.StringVar(dsn, "dsn", "",
		"PostgreSQL connection string (defaults to store.postgres.dsn when store.driver is postgres); "+
			"leave the password out, since the process list shows it, and supply it via PGPASSWORD or a password file")
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

// requireExistingStore checks that tgt names a database that exists and
// carries a schema, without opening the store. These commands write into an
// existing schema; they never create one.
func requireExistingStore(ctx context.Context, tgt storeTarget) error {
	if tgt.Driver == backend.DriverPostgres {
		return requireMigratedPostgres(ctx, tgt.DSN)
	}
	if tgt.SQLitePath == "" {
		return errors.New("database path is empty; use --db, set store.sqlite_path, or set SQI_STORE_SQLITE_PATH")
	}
	if err := requireExistingDB(tgt.SQLitePath); err != nil {
		return err
	}
	return requireMigratedDB(tgt.SQLitePath)
}

// openExistingStore opens tgt's store without migrating it, after checking it
// exists and has a schema (requireExistingStore). The store's own Open refuses
// a schema newer than this binary.
func openExistingStore(ctx context.Context, tgt storeTarget) (backend.Backend, error) {
	if err := requireExistingStore(ctx, tgt); err != nil {
		return nil, err
	}
	if tgt.Driver == backend.DriverPostgres {
		st, err := backend.Open(ctx, backend.Options{
			Driver: backend.DriverPostgres, PostgresDSN: tgt.DSN, MaxConns: 2, AutoMigrate: false,
		})
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", pgdsn.Redact(tgt.DSN), err)
		}
		return st, nil
	}
	st, err := backend.Open(ctx, backend.Options{SQLitePath: tgt.SQLitePath, AutoMigrate: false})
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	return st, nil
}
