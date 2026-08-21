// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	_ "modernc.org/sqlite" // register "sqlite" driver

	"github.com/uberware/sqi/internal/config"
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
