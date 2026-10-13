// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/uberware/sqi/internal/store/migrations"
)

// withConfigFile points persistentFlags.ConfigFile at path for the duration
// of the test and restores the previous value on cleanup. resolveDBPath
// reads persistentFlags.ConfigFile directly (the same global the "-c" root
// flag populates), so tests exercise it without going through cobra parsing.
func withConfigFile(t *testing.T, path string) {
	t.Helper()
	orig := persistentFlags.ConfigFile
	persistentFlags.ConfigFile = path
	t.Cleanup(func() { persistentFlags.ConfigFile = orig })
}

// writeStoreConfigFile writes a minimal config file setting store.sqlite_path
// and returns its path.
func writeStoreConfigFile(t *testing.T, sqlitePath string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sqi-server.yaml")
	content := "store:\n  sqlite_path: " + sqlitePath + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
	return path
}

// writeEmptyServerConfigFile writes an empty config file and returns its
// path, for tests that mean "no config file decided anything". Passing this
// instead of "" for persistentFlags.ConfigFile keeps config.Load's default
// search from falling through to $HOME/.sqi/sqi-server.yaml or
// /etc/sqi/sqi-server.yaml — real paths that could exist on the machine
// running the test, which would silently make the test depend on that
// machine's state.
func writeEmptyServerConfigFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "empty-sqi-server.yaml")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write empty config file: %v", err)
	}
	return path
}

// unsetStoreSQLitePathEnv neutralizes both env vars resolveDBPath consults,
// so a subtest whose meaning is "env decided nothing" (or "only the legacy
// var decided something") is not at the mercy of whatever the machine
// running the test happens to have exported. t.Setenv to "" is treated as
// unset by both config.applyEnv (SQI_STORE_SQLITE_PATH) and resolveDBPath's
// own envOr check (SQI_SQLITE_PATH).
func unsetStoreSQLitePathEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SQI_STORE_SQLITE_PATH", "")
	t.Setenv("SQI_SQLITE_PATH", "")
}

// TestResolveDBPath_Precedence walks the four-layer precedence order:
// explicit flag > config layer (file/SQI_STORE_SQLITE_PATH) > legacy
// SQI_SQLITE_PATH > built-in default. Every subtest explicitly neutralizes
// both env vars first and only sets the ones its own scenario needs, so
// none of them silently depends on whatever the test machine happens to
// have exported.
func TestResolveDBPath_Precedence(t *testing.T) {
	t.Run("explicit flag beats everything else", func(t *testing.T) {
		unsetStoreSQLitePathEnv(t)
		withConfigFile(t, writeStoreConfigFile(t, "from-config.db"))
		t.Setenv("SQI_STORE_SQLITE_PATH", "from-store-env.db")
		t.Setenv("SQI_SQLITE_PATH", "from-legacy-env.db")

		got, err := resolveDBPath("from-flag.db", true)
		if err != nil {
			t.Fatalf("resolveDBPath: %v", err)
		}
		if got != "from-flag.db" {
			t.Errorf("resolveDBPath = %q; want %q", got, "from-flag.db")
		}
	})

	t.Run("config file beats legacy env when flag not passed", func(t *testing.T) {
		unsetStoreSQLitePathEnv(t)
		withConfigFile(t, writeStoreConfigFile(t, "from-config.db"))
		t.Setenv("SQI_SQLITE_PATH", "from-legacy-env.db")

		got, err := resolveDBPath("sqi.db", false)
		if err != nil {
			t.Fatalf("resolveDBPath: %v", err)
		}
		if got != "from-config.db" {
			t.Errorf("resolveDBPath = %q; want %q", got, "from-config.db")
		}
	})

	t.Run("SQI_STORE_SQLITE_PATH env beats legacy env when flag not passed", func(t *testing.T) {
		unsetStoreSQLitePathEnv(t)
		withConfigFile(t, writeEmptyServerConfigFile(t))
		t.Setenv("SQI_STORE_SQLITE_PATH", "from-store-env.db")
		t.Setenv("SQI_SQLITE_PATH", "from-legacy-env.db")

		got, err := resolveDBPath("sqi.db", false)
		if err != nil {
			t.Fatalf("resolveDBPath: %v", err)
		}
		if got != "from-store-env.db" {
			t.Errorf("resolveDBPath = %q; want %q", got, "from-store-env.db")
		}
	})

	t.Run("legacy env used and warned about when nothing else set", func(t *testing.T) {
		unsetStoreSQLitePathEnv(t)
		withConfigFile(t, writeEmptyServerConfigFile(t))
		t.Setenv("SQI_SQLITE_PATH", "from-legacy-env.db")

		var got string
		var err error
		stderr := captureStderr(t, func() {
			got, err = resolveDBPath("sqi.db", false)
		})
		if err != nil {
			t.Fatalf("resolveDBPath: %v", err)
		}
		if got != "from-legacy-env.db" {
			t.Errorf("resolveDBPath = %q; want %q", got, "from-legacy-env.db")
		}
		if !strings.Contains(stderr, "SQI_STORE_SQLITE_PATH") {
			t.Errorf("expected a deprecation notice naming SQI_STORE_SQLITE_PATH on stderr; got:\n%s", stderr)
		}
		if !strings.Contains(stderr, "deprecated") {
			t.Errorf("expected the word 'deprecated' on stderr; got:\n%s", stderr)
		}
	})

	t.Run("built-in default when nothing set", func(t *testing.T) {
		unsetStoreSQLitePathEnv(t)
		withConfigFile(t, writeEmptyServerConfigFile(t))

		var got string
		var err error
		stderr := captureStderr(t, func() {
			got, err = resolveDBPath("sqi.db", false)
		})
		if err != nil {
			t.Fatalf("resolveDBPath: %v", err)
		}
		if got != "sqi.db" {
			t.Errorf("resolveDBPath = %q; want %q", got, "sqi.db")
		}
		if strings.Contains(stderr, "deprecated") {
			t.Errorf("no deprecation notice expected when nothing legacy is set; got:\n%s", stderr)
		}
	})
}

// TestResolveDBPath_MalformedConfigIsAnError verifies that a malformed
// --config file is surfaced as an error rather than silently falling back —
// even when an explicit --db is also passed, since the operator asked for a
// specific config file (persistentFlags.ConfigFile is non-empty here, i.e.
// -c was explicitly passed).
func TestResolveDBPath_MalformedConfigIsAnError(t *testing.T) {
	unsetStoreSQLitePathEnv(t)
	badPath := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(badPath, []byte("not: [valid: yaml"), 0o600); err != nil {
		t.Fatalf("write bad config: %v", err)
	}
	withConfigFile(t, badPath)

	if _, err := resolveDBPath("explicit.db", true); err == nil {
		t.Fatal("expected an error for a malformed config file even with an explicit --db, got nil")
	}
	if _, err := resolveDBPath("sqi.db", false); err == nil {
		t.Fatal("expected an error for a malformed config file, got nil")
	}
}

// TestResolveDBPath_ConfigLoadErrorWithoutExplicitConfigIsLenient verifies
// that a config-load failure having nothing to do with the database path —
// an unrelated malformed SQI_* variable, standing in for what an
// auto-discovered but broken /etc/sqi/sqi-server.yaml would also trigger —
// does not block backup/migrate/worker when -c was not explicitly passed.
// These are the tools reached for when something is already broken; hard
// failure is reserved for when the operator explicitly named a config file
// (see TestResolveDBPath_MalformedConfigIsAnError).
func TestResolveDBPath_ConfigLoadErrorWithoutExplicitConfigIsLenient(t *testing.T) {
	unsetStoreSQLitePathEnv(t)
	withConfigFile(t, "") // no -c passed
	t.Setenv("SQI_SCHEDULER_TICK_INTERVAL", "not-a-duration")

	var got string
	var err error
	stderr := captureStderr(t, func() {
		got, err = resolveDBPath("sqi.db", false)
	})
	if err != nil {
		t.Fatalf("resolveDBPath: unexpected error: %v", err)
	}
	if got != "sqi.db" {
		t.Errorf("resolveDBPath = %q; want the built-in default %q", got, "sqi.db")
	}
	if !strings.Contains(stderr, "could not load configuration") {
		t.Errorf("expected a warning about the failed config load on stderr; got:\n%s", stderr)
	}
}

// TestResolveDBPath_ConfigLoadErrorWithExplicitConfigIsHardFailure verifies
// the other half of the same rule: a config-load failure IS a hard failure
// once -c was explicitly passed, malformed-env-var or not.
func TestResolveDBPath_ConfigLoadErrorWithExplicitConfigIsHardFailure(t *testing.T) {
	unsetStoreSQLitePathEnv(t)
	withConfigFile(t, filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	t.Setenv("SQI_SCHEDULER_TICK_INTERVAL", "not-a-duration")

	if _, err := resolveDBPath("sqi.db", false); err == nil {
		t.Fatal("expected an error: -c named a config file that does not exist")
	}
}

// TestRequireExistingDB verifies the existence guard shared by backup and
// worker: a missing file is an actionable error, an existing one is fine.
func TestRequireExistingDB(t *testing.T) {
	t.Run("missing file is an actionable error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "does-not-exist.db")
		err := requireExistingDB(path)
		if err == nil {
			t.Fatal("expected an error for a missing database, got nil")
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("error should name the resolved path %q; got: %v", path, err)
		}
		if !strings.Contains(err.Error(), "migrate up") {
			t.Errorf("error should point at \"migrate up\" as the remediation; got: %v", err)
		}
	})

	t.Run("existing file passes", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "exists.db")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("write file: %v", err)
		}
		if err := requireExistingDB(path); err != nil {
			t.Errorf("requireExistingDB(%q) = %v; want nil", path, err)
		}
	})
}

// TestRequireMigratedDB verifies the schema guard that catches what
// requireExistingDB's stat check cannot: a file that exists but has no
// tables, because it was created (e.g. by a plain sqlite.Open with
// AutoMigrate: false, or "touch") rather than migrated.
func TestRequireMigratedDB(t *testing.T) {
	t.Run("empty file has no schema", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty.db")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("write empty file: %v", err)
		}
		err := requireMigratedDB(path)
		if err == nil {
			t.Fatal("expected an error for an unmigrated database, got nil")
		}
		if !strings.Contains(err.Error(), "migrate up") {
			t.Errorf("error should point at \"migrate up\" as the remediation; got: %v", err)
		}
	})

	t.Run("migrated database passes", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "migrated.db")
		db, err := openMigrateDB(path)
		if err != nil {
			t.Fatalf("openMigrateDB: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		p, err := migrations.NewProvider(db, "sqlite")
		if err != nil {
			t.Fatalf("migrations.NewProvider: %v", err)
		}
		if _, err := p.Up(t.Context()); err != nil {
			t.Fatalf("migrate up: %v", err)
		}

		if err := requireMigratedDB(path); err != nil {
			t.Errorf("requireMigratedDB(%q) = %v; want nil", path, err)
		}
	})
}

// TestResolveStoreTarget walks resolveStoreTarget's precedence: an explicit
// --dsn, then an explicit --db, then the configured store.driver.
func TestResolveStoreTarget(t *testing.T) {
	unsetStoreSQLitePathEnv(t)
	withConfigFile(t, writeEmptyServerConfigFile(t))
	t.Setenv("SQI_STORE_DRIVER", "")
	t.Setenv("SQI_STORE_POSTGRES_DSN", "")

	cmd := &cobra.Command{}
	cmd.Flags().String("db", "sqi.db", "")
	cmd.Flags().String("dsn", "", "")

	got, err := resolveStoreTarget(cmd, "sqi.db", "")
	if err != nil || got.Driver != "sqlite" {
		t.Fatalf("default = %+v, %v; want sqlite", got, err)
	}

	t.Setenv("SQI_STORE_DRIVER", "postgres")
	if _, err := resolveStoreTarget(cmd, "sqi.db", ""); err == nil || !strings.Contains(err.Error(), "SQI_STORE_POSTGRES_DSN") {
		t.Fatalf("postgres without a dsn = %v", err)
	}
	t.Setenv("SQI_STORE_POSTGRES_DSN", "postgres://x@db/sqi")
	got, err = resolveStoreTarget(cmd, "sqi.db", "")
	if err != nil || got.Driver != "postgres" || got.DSN != "postgres://x@db/sqi" {
		t.Fatalf("postgres from config = %+v, %v", got, err)
	}

	// A driver the server would reject must not resolve to SQLite: that would
	// let "migrate up" create ./sqi.db and exit 0 for a config the server
	// refuses to start on.
	t.Setenv("SQI_STORE_DRIVER", "postgresql")
	if got, err := resolveStoreTarget(cmd, "sqi.db", ""); err == nil ||
		!strings.Contains(err.Error(), `store.driver must be sqlite or postgres, got "postgresql"`) {
		t.Fatalf("unknown driver = %+v, %v; want a store.driver error", got, err)
	}
	t.Setenv("SQI_STORE_DRIVER", "postgres")

	if err := cmd.Flags().Set("db", "other.db"); err != nil {
		t.Fatal(err)
	}
	got, err = resolveStoreTarget(cmd, "other.db", "")
	if err != nil || got.Driver != "sqlite" || got.SQLitePath != "other.db" {
		t.Fatalf("--db wins over a postgres config = %+v, %v", got, err)
	}
	if err := cmd.Flags().Set("dsn", "postgres://flag@db/sqi"); err != nil {
		t.Fatal(err)
	}
	got, err = resolveStoreTarget(cmd, "other.db", "postgres://flag@db/sqi")
	if err != nil || got.Driver != "postgres" || got.DSN != "postgres://flag@db/sqi" {
		t.Fatalf("--dsn wins = %+v, %v", got, err)
	}
}

// isolateConfigDiscovery makes config.Load's default search find nothing a
// developer has on disk: the working directory (for ./config/sqi-server.yaml)
// and the home directory (for ~/.sqi/sqi-server.yaml) both become empty temp
// directories, and no -c is set.
func isolateConfigDiscovery(t *testing.T) {
	t.Helper()
	withConfigFile(t, "")
	t.Chdir(t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

// TestResolveStoreTarget_UnloadableConfigWithoutExplicitConfig pins the limit
// of the no -c lenience. A failed load returns a zero Config, losing every
// env setting, SQI_STORE_DRIVER included; falling back to SQLite is fine when
// the operator asked for SQLite (or nothing), but not when SQI_STORE_DRIVER
// names another driver, where it would quietly open (or, for migrate, create)
// the wrong store.
func TestResolveStoreTarget_UnloadableConfigWithoutExplicitConfig(t *testing.T) {
	for _, tc := range []struct {
		driver  string
		wantErr bool
	}{
		{"postgres", true},
		{"postgresql", true},
		{"sqlite", false},
		{"", false},
	} {
		t.Run("SQI_STORE_DRIVER="+tc.driver, func(t *testing.T) {
			unsetStoreSQLitePathEnv(t)
			isolateConfigDiscovery(t)
			t.Setenv("SQI_SCHEDULER_TICK_INTERVAL", "not-a-duration")
			t.Setenv("SQI_STORE_DRIVER", tc.driver)
			t.Setenv("SQI_STORE_POSTGRES_DSN", "postgres://x@db/sqi")

			cmd := &cobra.Command{}
			cmd.Flags().String("db", "sqi.db", "")
			cmd.Flags().String("dsn", "", "")

			var got storeTarget
			var err error
			_ = captureStderr(t, func() { got, err = resolveStoreTarget(cmd, "sqi.db", "") })
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "load config") ||
					!strings.Contains(err.Error(), "SQI_SCHEDULER_TICK_INTERVAL") {
					t.Fatalf("resolveStoreTarget = %+v, %v; want the load error", got, err)
				}
				return
			}
			if err != nil || got.Driver != "sqlite" || got.SQLitePath != "sqi.db" {
				t.Fatalf("resolveStoreTarget = %+v, %v; want the lenient SQLite default", got, err)
			}
		})
	}
}

// TestResolveStoreTarget_ExplicitConfigMustLoadWithDSN pins that an explicit
// -c is loaded, and its failure reported, even when --dsn means the config
// decides nothing, exactly as it is with --db.
func TestResolveStoreTarget_ExplicitConfigMustLoadWithDSN(t *testing.T) {
	unsetStoreSQLitePathEnv(t)
	t.Setenv("SQI_STORE_DRIVER", "")
	t.Setenv("SQI_STORE_POSTGRES_DSN", "")

	cmd := &cobra.Command{}
	cmd.Flags().String("db", "sqi.db", "")
	cmd.Flags().String("dsn", "", "")
	if err := cmd.Flags().Set("dsn", "postgres://flag@db/sqi"); err != nil {
		t.Fatal(err)
	}

	withConfigFile(t, filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if got, err := resolveStoreTarget(cmd, "sqi.db", "postgres://flag@db/sqi"); err == nil ||
		!strings.Contains(err.Error(), "load config") {
		t.Fatalf("-c missing.yaml --dsn = %+v, %v; want the load error", got, err)
	}

	withConfigFile(t, writeEmptyServerConfigFile(t))
	got, err := resolveStoreTarget(cmd, "sqi.db", "postgres://flag@db/sqi")
	if err != nil || got.Driver != "postgres" || got.DSN != "postgres://flag@db/sqi" {
		t.Fatalf("-c good.yaml --dsn = %+v, %v", got, err)
	}
}
