// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/uberware/sqi/internal/config"
)

// resolveDBPath applies the database-path precedence shared by backup,
// migrate, and worker, highest priority first:
//
//  1. explicit is used as-is when explicitChanged is true (the operator
//     passed --db on the command line).
//  2. The config layer: the root -c/--config file and SQI_STORE_SQLITE_PATH,
//     i.e. whatever [config.Load] resolves store.sqlite_path to, when that
//     differs from the built-in default.
//  3. The legacy SQI_SQLITE_PATH environment variable, kept working as an
//     alias. A deprecation notice naming SQI_STORE_SQLITE_PATH — the
//     variable sqi-server itself reads — is printed to stderr when this is
//     the layer that decided the path.
//  4. The built-in default ("sqi.db").
//
// The config layer is always loaded, even when an explicit --db makes its
// result moot, so a malformed --config file is reported as an error rather
// than silently ignored — the operator asked for a specific config.
func resolveDBPath(explicit string, explicitChanged bool) (string, error) {
	cfg, err := config.Load(persistentFlags.ConfigFile, config.FlagOverrides{})
	if err != nil {
		return "", fmt.Errorf("load config: %w", err)
	}

	if explicitChanged {
		return explicit, nil
	}

	if def := config.DefaultConfig().Store.SQLitePath; cfg.Store.SQLitePath != def {
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
