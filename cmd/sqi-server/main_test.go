// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/migrations"
	"github.com/uberware/sqi/internal/store/postgres"
	"github.com/uberware/sqi/internal/store/sqlite"
	"github.com/uberware/sqi/internal/store/storetest"
)

// captureStdout redirects os.Stdout to a pipe for the duration of fn, then
// returns everything written to it.  Required because the cobra commands in
// this binary write directly to os.Stdout via fmt.Printf / fmt.Fprint rather
// than through cmd.OutOrStdout().
//
// Must NOT be called from parallel sub-tests — the redirect is process-wide.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w

	fn()

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("io.Copy from stdout pipe: %v", err)
	}
	r.Close()
	return buf.String()
}

// captureStderr redirects os.Stderr to a pipe for the duration of fn, then
// returns everything written to it. Needed alongside captureStdout for
// commands (backup's resolveDBPath deprecation notice, keygen's warnings)
// that deliberately separate their stderr diagnostics from stdout output.
//
// Must NOT be called from parallel sub-tests — the redirect is process-wide,
// same caveat as captureStdout.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w

	fn()

	w.Close()
	os.Stderr = old

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("io.Copy from stderr pipe: %v", err)
	}
	r.Close()
	return buf.String()
}

// withFlagUnchanged resets a flag's Changed state to false for the duration
// of the test, then restores it.
//
// pflag.Flag.Changed is sticky: it flips to true the first time a flag is
// parsed from the command line and the library never resets it. Flag
// objects in this binary are package-level singletons registered once in
// init(), so within a single test process a later test asserting "the flag
// was omitted" would otherwise observe a stale true left by an earlier test
// that passed it explicitly, purely as an artifact of test ordering.
func withFlagUnchanged(t *testing.T, fs *pflag.FlagSet, name string) {
	t.Helper()
	f := fs.Lookup(name)
	if f == nil {
		t.Fatalf("no such flag: %q", name)
	}
	f.Changed = false
	// Reset to false, not the saved original: restoring a stale true would
	// leave exactly the leftover this helper exists to prevent, for
	// whichever test runs next.
	t.Cleanup(func() { f.Changed = false })
}

// resetFlagsChanged clears pflag.Flag.Changed for every flag in cmd's own
// FlagSet and PersistentFlags, and recurses into every subcommand. Called
// from prepareRoot before every Execute() so a stale Changed=true left by an
// earlier test's cobra parse — Changed is sticky, pflag never resets it, and
// every command in this binary is a package-level singleton reused across
// the whole test process — cannot leak into a later test that means "this
// flag was not passed". This is the general form of withFlagUnchanged: every
// flag in the tree, before every Execute(), so no individual test needs to
// know which flags an earlier one touched.
func resetFlagsChanged(cmd *cobra.Command) {
	cmd.Flags().VisitAll(func(f *pflag.Flag) { f.Changed = false })
	cmd.PersistentFlags().VisitAll(func(f *pflag.Flag) { f.Changed = false })
	for _, c := range cmd.Commands() {
		resetFlagsChanged(c)
	}
}

// resetFlagValues puts every subcommand flag's bound variable back to its
// default, recursing into every subcommand. Changed is not the only sticky
// state: the variable a flag writes keeps an earlier test's value too, so
// "worker token issue" without --ttl would otherwise run with whatever --ttl
// the last test passed.
//
// The root command's persistent flags (-c/--config and the serve overrides)
// are left alone: tests set persistentFlags directly (withConfigFile) before
// prepareRoot, and resetting them here would undo that. A subcommand's
// Flags() also holds those inherited flags once cobra has merged them, so
// they are skipped by identity, not just on rootCmd.
//
// A default that does not parse back is a programming error in the flag's
// registration, so it panics rather than taking a *testing.T that
// prepareRoot does not have.
func resetFlagValues(cmd *cobra.Command) {
	reset := func(f *pflag.Flag) {
		if rootCmd.PersistentFlags().Lookup(f.Name) == f {
			return
		}
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			if err := sv.Replace(nil); err != nil {
				panic(fmt.Sprintf("reset flag --%s: %v", f.Name, err))
			}
			return
		}
		if err := f.Value.Set(f.DefValue); err != nil {
			panic(fmt.Sprintf("reset flag --%s to %q: %v", f.Name, f.DefValue, err))
		}
	}
	cmd.Flags().VisitAll(reset)
	cmd.PersistentFlags().VisitAll(reset)
	for _, c := range cmd.Commands() {
		resetFlagValues(c)
	}
}

// prepareRoot sets the args that rootCmd will parse on the next Execute() call
// and redirects cobra's own output writers (help, usage, error messages) to a
// discard buffer so test output stays clean.
func prepareRoot(args []string) {
	resetFlagValues(rootCmd)
	resetFlagsChanged(rootCmd)
	rootCmd.SetArgs(args)
	var sink bytes.Buffer
	rootCmd.SetOut(&sink)
	rootCmd.SetErr(&sink)
}

// TestEnvOr verifies the envOr helper returns the env value when set and the
// fallback when not.
func TestEnvOr(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		envVal   string
		fallback string
		want     string
	}{
		{
			name:     "returns env value when set",
			key:      "_SQI_TEST_ENV_OR_SET",
			envVal:   "from-env",
			fallback: "fallback",
			want:     "from-env",
		},
		{
			name:     "returns fallback when env is not set",
			key:      "_SQI_TEST_ENV_OR_UNSET",
			envVal:   "",
			fallback: "default-val",
			want:     "default-val",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envVal != "" {
				t.Setenv(tt.key, tt.envVal)
			}
			got := envOr(tt.key, tt.fallback)
			if got != tt.want {
				t.Errorf("envOr(%q, %q) = %q; want %q", tt.key, tt.fallback, got, tt.want)
			}
		})
	}
}

// TestExitOnErr_Nil verifies that exitOnErr is a no-op when passed nil, i.e.,
// it does not call os.Exit.
func TestExitOnErr_Nil(t *testing.T) {
	t.Run("nil error is no-op", func(_ *testing.T) {
		exitOnErr(nil) // must not call os.Exit
	})
}

// TestOpenMigrateDB_ErrorPaths verifies the validation guard and the success
// path of openMigrateDB without needing goose migration files.
func TestOpenMigrateDB_ErrorPaths(t *testing.T) {
	t.Run("empty path returns validation error", func(t *testing.T) {
		db, err := openMigrateDB("")
		if err == nil {
			db.Close()
			t.Fatal("expected error for empty path, got nil")
		}
		if !strings.Contains(err.Error(), "empty") {
			t.Errorf("error should mention 'empty'; got: %v", err)
		}
	})
}

// TestOpenMigrateDB_ValidPath verifies that openMigrateDB opens an SQLite
// database and applies pragmas when given a real path.
func TestOpenMigrateDB_ValidPath(t *testing.T) {
	t.Run("valid path succeeds and returns open db", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "test-migrate.db")
		db, err := openMigrateDB(dbPath)
		if err != nil {
			t.Fatalf("openMigrateDB(%q) error = %v", dbPath, err)
		}
		if db == nil {
			t.Fatal("openMigrateDB returned nil db without error")
		}
		db.Close()
	})
}

// TestPersistentFlagOverrides confirms that persistentFlagOverrides returns
// zero-value overrides when no persistent flags are set, and reflects the
// flag values when they are explicitly provided.
func TestPersistentFlagOverrides(t *testing.T) {
	t.Run("zero overrides when no flags changed", func(t *testing.T) {
		// Run version so cobra re-parses without explicit flag values.
		prepareRoot([]string{"version"})
		_ = captureStdout(t, func() {
			if err := Execute(); err != nil {
				t.Errorf("Execute(version) error = %v", err)
			}
		})

		ov := persistentFlagOverrides()
		if ov.LogLevel != "" {
			t.Errorf("LogLevel: got %q; want empty", ov.LogLevel)
		}
		if ov.LogFormat != "" {
			t.Errorf("LogFormat: got %q; want empty", ov.LogFormat)
		}
	})

	t.Run("log-level override set when flag provided", func(t *testing.T) {
		prepareRoot([]string{"--log-level", "debug", "version"})
		_ = captureStdout(t, func() {
			if err := Execute(); err != nil {
				t.Errorf("Execute(version) error = %v", err)
			}
		})

		ov := persistentFlagOverrides()
		if ov.LogLevel != "debug" {
			t.Errorf("LogLevel: got %q; want %q", ov.LogLevel, "debug")
		}
	})

	t.Run("log-format override set when flag provided", func(t *testing.T) {
		prepareRoot([]string{"--log-format", "text", "version"})
		_ = captureStdout(t, func() {
			if err := Execute(); err != nil {
				t.Errorf("Execute(version) error = %v", err)
			}
		})

		ov := persistentFlagOverrides()
		if ov.LogFormat != "text" {
			t.Errorf("LogFormat: got %q; want %q", ov.LogFormat, "text")
		}
	})
}

// TestRootCmd_SubcommandTree exercises the cobra command tree without starting
// the server, binding sockets, or touching databases.
func TestRootCmd_SubcommandTree(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{
			name:    "no args prints usage and exits cleanly",
			args:    []string{},
			wantErr: false,
		},
		{
			name:    "--help exits cleanly",
			args:    []string{"--help"},
			wantErr: false,
		},
		{
			name:    "unknown subcommand returns error",
			args:    []string{"bogus-subcommand"},
			wantErr: true,
		},
		{
			name:    "bad flag returns error",
			args:    []string{"--no-such-flag"},
			wantErr: true,
		},
		{
			name:    "bare config exits cleanly (prints usage)",
			args:    []string{"config"},
			wantErr: false,
		},
		{
			name:    "config --help exits cleanly",
			args:    []string{"config", "--help"},
			wantErr: false,
		},
		{
			name:    "bare migrate exits cleanly (prints usage)",
			args:    []string{"migrate"},
			wantErr: false,
		},
		{
			name:    "migrate --help exits cleanly",
			args:    []string{"migrate", "--help"},
			wantErr: false,
		},
		{
			name:    "restore --help exits cleanly",
			args:    []string{"restore", "--help"},
			wantErr: false,
		},
		{
			name:    "bare store exits cleanly (prints usage)",
			args:    []string{"store"},
			wantErr: false,
		},
		{
			name:    "store copy --help exits cleanly",
			args:    []string{"store", "copy", "--help"},
			wantErr: false,
		},
		{
			name:    "store copy takes no arguments",
			args:    []string{"store", "copy", "extra"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prepareRoot(tt.args)
			err := Execute()
			if (err != nil) != tt.wantErr {
				t.Errorf("Execute(%v) error = %v; wantErr %v", tt.args, err, tt.wantErr)
			}
		})
	}
}

// TestRootCmd_MigrateSubcommands verifies migrations are forward-only: the
// migrate command offers status and up, and no down.
func TestRootCmd_MigrateSubcommands(t *testing.T) {
	var got []string
	for _, c := range migrateCmd.Commands() {
		got = append(got, c.Name())
	}
	sort.Strings(got)
	if want := []string{"status", "up"}; !slices.Equal(got, want) {
		t.Errorf("migrate subcommands = %v; want %v", got, want)
	}
}

// TestMigrateDown_IsGone verifies "migrate down" is removed (rollback is
// restoring the pre-upgrade backup). Cobra prints usage for an unknown
// subcommand of a command with no RunE and returns nil, so the assertion is on
// the schema version rather than on the error.
func TestMigrateDown_IsGone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	prepareRoot([]string{"migrate", "up", "--db", path})
	_ = captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("migrate up: %v", err)
		}
	})
	before := schemaVersionOf(t, path)
	if before == 0 {
		t.Fatal("migrate up left the schema at version 0")
	}

	prepareRoot([]string{"migrate", "down", "--db", path})
	var downErr error
	_ = captureStdout(t, func() { downErr = Execute() })
	t.Logf("migrate down returned: %v", downErr)

	if after := schemaVersionOf(t, path); after != before {
		t.Fatalf("schema version %d -> %d after migrate down; it is removed", before, after)
	}
}

// schemaVersionOf reads the highest applied goose version of the SQLite
// database at path.
func schemaVersionOf(t *testing.T, path string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	v, err := sqlite.SchemaVersion(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestVersionCmd verifies that the version subcommand prints the expected
// output label keys.  Values vary by build; only label presence is checked.
func TestVersionCmd(t *testing.T) {
	t.Run("prints version metadata labels to stdout", func(t *testing.T) {
		prepareRoot([]string{"version"})

		out := captureStdout(t, func() {
			if err := Execute(); err != nil {
				t.Errorf("Execute(version) unexpected error: %v", err)
			}
		})

		for _, want := range []string{"sqi-server", "commit:", "built:", "go:"} {
			if !strings.Contains(out, want) {
				t.Errorf("version output missing %q\ngot:\n%s", want, out)
			}
		}
	})
}

// TestConfigPrintCmd verifies that "config print" produces non-empty YAML
// containing the top-level "http:" key from the server configuration.
func TestConfigPrintCmd(t *testing.T) {
	t.Run("prints non-empty YAML with http key", func(t *testing.T) {
		prepareRoot([]string{"config", "print"})

		out := captureStdout(t, func() {
			if err := Execute(); err != nil {
				t.Errorf("Execute(config print) unexpected error: %v", err)
			}
		})

		if strings.TrimSpace(out) == "" {
			t.Fatal("config print produced empty output")
		}
		if !strings.Contains(out, "http:") {
			t.Errorf("config print YAML missing 'http:' key\ngot:\n%s", out)
		}
	})

	t.Run("redacts the PostgreSQL DSN password", func(t *testing.T) {
		t.Chdir(t.TempDir())
		t.Setenv("SQI_STORE_DRIVER", "postgres")
		t.Setenv("SQI_STORE_POSTGRES_DSN", "postgres://sqi:hunter2@db:5432/sqi?sslmode=require")
		prepareRoot([]string{"config", "print"})

		out := captureStdout(t, func() {
			if err := Execute(); err != nil {
				t.Errorf("Execute(config print) unexpected error: %v", err)
			}
		})

		if strings.Contains(out, "hunter2") {
			t.Fatalf("config print leaks the DSN password\ngot:\n%s", out)
		}
		for _, want := range []string{"driver: postgres", "postgres://sqi:redacted@db:5432/sqi?sslmode=require", "max_conns: 16"} {
			if !strings.Contains(out, want) {
				t.Errorf("config print YAML missing %q\ngot:\n%s", want, out)
			}
		}
	})
}

// TestMigrateCmd_WithTempDB exercises the migrate subcommands (status, up,
// status again) against a real temporary SQLite database, and pins the
// status format: one line per migration, its applied time (RFC 3339, UTC) or
// "pending", then its file name.
func TestMigrateCmd_WithTempDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test-migrate.db")

	t.Run("migrate status on a fresh database lists every migration pending", func(t *testing.T) {
		prepareRoot([]string{"migrate", "status", "--db", dbPath})
		out := captureStdout(t, func() {
			if err := Execute(); err != nil {
				t.Errorf("Execute(migrate status) error = %v", err)
			}
		})
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) < 2 {
			t.Fatalf("migrate status listed %d migrations:\n%s", len(lines), out)
		}
		for _, l := range lines {
			if !strings.HasPrefix(l, "pending ") || !strings.HasSuffix(l, ".sql") {
				t.Errorf("migrate status line %q, want \"pending   <file>.sql\"", l)
			}
		}
		if !strings.Contains(out, "00001_placeholder.sql") {
			t.Errorf("migrate status does not list 00001_placeholder.sql:\n%s", out)
		}
	})

	t.Run("migrate up applies all pending migrations", func(t *testing.T) {
		prepareRoot([]string{"migrate", "up", "--db", dbPath})
		out := captureStdout(t, func() {
			if err := Execute(); err != nil {
				t.Errorf("Execute(migrate up) error = %v", err)
			}
		})
		if !strings.Contains(out, "00001_placeholder.sql") {
			t.Errorf("migrate up does not report the migrations it applied:\n%s", out)
		}
	})

	t.Run("migrate status after up lists nothing pending", func(t *testing.T) {
		prepareRoot([]string{"migrate", "status", "--db", dbPath})
		out := captureStdout(t, func() {
			if err := Execute(); err != nil {
				t.Errorf("Execute(migrate status) error = %v", err)
			}
		})
		for l := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
			stamp, _, ok := strings.Cut(l, " ")
			if !ok || !strings.HasSuffix(l, ".sql") {
				t.Fatalf("migrate status line %q, want \"<time> <file>.sql\"", l)
			}
			if _, err := time.Parse(time.RFC3339, stamp); err != nil {
				t.Errorf("migrate status line %q: applied time %q is not RFC 3339: %v", l, stamp, err)
			}
		}
	})

	t.Run("migrate up again reports nothing pending", func(t *testing.T) {
		prepareRoot([]string{"migrate", "up", "--db", dbPath})
		out := captureStdout(t, func() {
			if err := Execute(); err != nil {
				t.Errorf("Execute(migrate up) error = %v", err)
			}
		})
		if strings.TrimSpace(out) != "no pending migrations" {
			t.Errorf("second migrate up printed %q, want \"no pending migrations\"", out)
		}
	})
}

// TestMigrateCmd_DBPath_ExplicitFlagBeatsConfig verifies that --db wins even
// when a config file names a different database, proving migrate consults
// cmd.Flags().Changed("db") rather than always preferring the config layer.
func TestMigrateCmd_DBPath_ExplicitFlagBeatsConfig(t *testing.T) {
	explicitPath := filepath.Join(t.TempDir(), "explicit-migrate.db")
	configuredPath := filepath.Join(t.TempDir(), "from-config-migrate.db")
	withConfigFile(t, writeStoreConfigFile(t, configuredPath))

	prepareRoot([]string{"migrate", "up", "--db", explicitPath})
	_ = captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("migrate up: unexpected error: %v", err)
		}
	})

	if _, err := os.Stat(explicitPath); err != nil {
		t.Errorf("expected migrate to create the explicit --db path %q: %v", explicitPath, err)
	}
	if _, err := os.Stat(configuredPath); err == nil {
		t.Errorf("migrate created a database at the configured path %q despite an explicit --db", configuredPath)
	}
}

// TestMigrateCmd_DBPath_ConfigFileHonoredWhenFlagOmitted verifies that
// omitting --db resolves the database through the config layer, and that
// migrate — unlike backup and worker — creates it there.
func TestMigrateCmd_DBPath_ConfigFileHonoredWhenFlagOmitted(t *testing.T) {
	withFlagUnchanged(t, migrateCmd.PersistentFlags(), "db")
	unsetStoreSQLitePathEnv(t)
	configuredPath := filepath.Join(t.TempDir(), "from-config-migrate.db")
	withConfigFile(t, writeStoreConfigFile(t, configuredPath))

	prepareRoot([]string{"migrate", "up"})
	_ = captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("migrate up: unexpected error: %v", err)
		}
	})

	if _, err := os.Stat(configuredPath); err != nil {
		t.Errorf("expected migrate to create the configured store.sqlite_path %q: %v", configuredPath, err)
	}
}

// TestBackupCmd_ErrorPaths exercises backup's validation guards through the
// real command tree, so cobra's own --out requirement and resolveDBPath's
// flag-changed detection (which needs a live *cobra.Command, not a nil one)
// are both in play.
func TestBackupCmd_ErrorPaths(t *testing.T) {
	t.Run("missing --out is rejected by cobra", func(t *testing.T) {
		prepareRoot([]string{"backup", "--db", "sqi.db"})
		_ = captureStdout(t, func() {
			if err := Execute(); err == nil {
				t.Fatal("expected an error for a missing required --out flag, got nil")
			}
		})
	})

	t.Run("explicit empty --db returns a descriptive error", func(t *testing.T) {
		outPath := filepath.Join(t.TempDir(), "out.db")
		prepareRoot([]string{"backup", "--db", "", "--out", outPath})
		var runErr error
		_ = captureStdout(t, func() {
			runErr = Execute()
		})
		if runErr == nil {
			t.Fatal("expected an error for an explicitly empty --db, got nil")
		}
		if !strings.Contains(runErr.Error(), "empty") {
			t.Errorf("error should mention 'empty'; got: %v", runErr)
		}
	})

	t.Run("missing source database is an error, not a fresh empty backup", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "does-not-exist.db")
		outPath := filepath.Join(t.TempDir(), "out.db")
		prepareRoot([]string{"backup", "--db", dbPath, "--out", outPath})
		var runErr error
		_ = captureStdout(t, func() {
			runErr = Execute()
		})
		if runErr == nil {
			t.Fatal("expected an error for a missing source database, got nil")
		}
		if !strings.Contains(runErr.Error(), dbPath) {
			t.Errorf("error should name the resolved path %q; got: %v", dbPath, runErr)
		}
		if _, statErr := os.Stat(outPath); statErr == nil {
			t.Error("backup should not have produced an output file when the source database is missing")
		}
	})
}

// TestBackupCmd_DBPath_ConfigFileHonoredWhenFlagOmitted verifies that
// omitting --db resolves the source database through the config layer
// (store.sqlite_path) rather than the built-in "sqi.db" default.
func TestBackupCmd_DBPath_ConfigFileHonoredWhenFlagOmitted(t *testing.T) {
	withFlagUnchanged(t, backupCmd.Flags(), "db")
	unsetStoreSQLitePathEnv(t)
	configuredPath := filepath.Join(t.TempDir(), "from-config-backup.db")
	createTestDB(t, configuredPath)
	withConfigFile(t, writeStoreConfigFile(t, configuredPath))

	outPath := filepath.Join(t.TempDir(), "out.db")
	prepareRoot([]string{"backup", "--out", outPath})
	_ = captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("backup: unexpected error: %v", err)
		}
	})

	if _, err := os.Stat(outPath); err != nil {
		t.Errorf("expected a backup file at %q: %v", outPath, err)
	}
}

// TestMigrateCmd_Postgres_UpThenStatus migrates an empty PostgreSQL database
// through the CLI and checks that status then reports every migration applied.
func TestMigrateCmd_Postgres_UpThenStatus(t *testing.T) {
	dsn := storetest.PostgresEmptyDSN(t)
	prepareRoot([]string{"migrate", "up", "--dsn", dsn})
	_ = captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("migrate up: %v", err)
		}
	})
	prepareRoot([]string{"migrate", "status", "--dsn", dsn})
	out := captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("migrate status: %v", err)
		}
	})
	if !strings.Contains(out, "00037_baseline.sql") || strings.Contains(out, "pending") {
		t.Fatalf("migrate status after up:\n%s", out)
	}
}

// TestBackupCmd_Postgres backs up a migrated PostgreSQL database through the
// CLI with pg_dump.
func TestBackupCmd_Postgres(t *testing.T) {
	storetest.RequirePostgresTool(t, "pg_dump")
	dsn := storetest.PostgresDSN(t)
	out := filepath.Join(t.TempDir(), "sqi.dump")
	prepareRoot([]string{"backup", "--dsn", dsn, "--out", out})
	if err := Execute(); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		t.Fatalf("backup archive: %v, %v", fi, err)
	}
}

// TestMigrateUp_RefusesWhileAServerHoldsTheLock verifies migrate up takes the
// single-server lock and so refuses to run beside a server.
func TestMigrateUp_RefusesWhileAServerHoldsTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	srv, err := sqlite.Open(t.Context(), path, sqlite.Options{AutoMigrate: true, ServerLock: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	prepareRoot([]string{"migrate", "up", "--db", path})
	err = Execute()
	if err == nil || !strings.Contains(err.Error(), "stop it first") {
		t.Fatalf("migrate up beside a server = %v, want a lock refusal", err)
	}
}

// TestMigrateStatus_WritesNothing verifies migrate status on SQLite leaves the
// database file byte-for-byte as it was — it is safe to run on a backup — and
// creates no file where there is none: an empty file and a missing one list
// every migration pending.
func TestMigrateStatus_WritesNothing(t *testing.T) {
	sum := func(path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%x", sha256.Sum256(b))
	}
	status := func(path string) string {
		t.Helper()
		prepareRoot([]string{"migrate", "status", "--db", path})
		var runErr error
		out := captureStdout(t, func() { runErr = Execute() })
		if runErr != nil {
			t.Fatalf("migrate status --db %s: %v", path, runErr)
		}
		return out
	}
	allPending := func(t *testing.T, out string) {
		t.Helper()
		for l := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
			if !strings.HasPrefix(l, "pending ") {
				t.Errorf("migrate status line %q, want pending", l)
			}
		}
	}

	t.Run("a migrated database", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "sqi.db")
		createTestDB(t, path)
		before := sum(path)
		out := status(path)
		if strings.Contains(out, "pending") {
			t.Errorf("migrate status on a migrated database lists pending migrations:\n%s", out)
		}
		if after := sum(path); after != before {
			t.Errorf("migrate status changed the database file (sha256 %s -> %s)", before, after)
		}
	})
	// A backup is what an operator checks with migrate status, and VACUUM
	// INTO writes it in rollback-journal mode: opened the way migrate up opens
	// a database, it would be switched to WAL, rewriting its header.
	t.Run("a backup", func(t *testing.T) {
		live := filepath.Join(t.TempDir(), "sqi.db")
		createTestDB(t, live)
		path := filepath.Join(t.TempDir(), "backup.db")
		if err := sqlite.BackupPath(t.Context(), live, path); err != nil {
			t.Fatal(err)
		}
		before := sum(path)
		if out := status(path); strings.Contains(out, "pending") {
			t.Errorf("migrate status on a backup lists pending migrations:\n%s", out)
		}
		if after := sum(path); after != before {
			t.Errorf("migrate status changed the backup (sha256 %s -> %s)", before, after)
		}
	})
	t.Run("an empty file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty.db")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		allPending(t, status(path))
		if fi, err := os.Stat(path); err != nil || fi.Size() != 0 {
			t.Errorf("empty file after migrate status = %v, %v; want it still empty", fi, err)
		}
	})
	t.Run("no file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing.db")
		allPending(t, status(path))
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("migrate status created %s (err %v)", path, err)
		}
	})
}

// TestMigrateUp_FoldsInACrashedServersWAL backs the advice restore --in-place
// gives for a database whose own -wal is not empty (its server crashed, or a
// snapshot took them together): "sqi-server migrate up" folds the -wal into
// the database file, so --in-place can then run, without a server ever
// starting on the database unfenced.
func TestMigrateUp_FoldsInACrashedServersWAL(t *testing.T) {
	live := filepath.Join(t.TempDir(), "live.db")
	st, err := sqlite.Open(t.Context(), live, sqlite.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.CreateFarm(t.Context(), store.Farm{ID: "f1", Name: "after-the-crash"}); err != nil {
		t.Fatal(err)
	}

	// A snapshot of a running server: the database file and its -wal, taken
	// while the store is still open, so the farm is only in the -wal.
	path := filepath.Join(t.TempDir(), "sqi.db")
	for _, suffix := range []string{"", "-wal"} {
		b, err := os.ReadFile(live + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+suffix, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if fi, err := os.Stat(path + "-wal"); err != nil || fi.Size() == 0 {
		t.Fatalf("snapshot -wal = %v, %v; the test needs a non-empty one", fi, err)
	}

	prepareRoot([]string{"migrate", "up", "--db", path})
	var runErr error
	_ = captureStdout(t, func() { runErr = Execute() })
	if runErr != nil {
		t.Fatalf("migrate up: %v", runErr)
	}

	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() > 0 {
		t.Fatalf("-wal after migrate up is %d bytes, want it folded in (absent or empty)", fi.Size())
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	// Read the database file alone: with no -wal, the farm must be in it.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var name string
	if err := db.QueryRowContext(t.Context(), `SELECT name FROM farms WHERE id = 'f1'`).Scan(&name); err != nil {
		t.Fatalf("farm written before the crash, after migrate up: %v", err)
	}
	if name != "after-the-crash" {
		t.Errorf("farm name = %q, want %q", name, "after-the-crash")
	}
}

// TestCLI_APathWithAQuestionMark verifies that the commands opening a SQLite
// file by its --db path keep a "?" in it as part of the name: handed to the
// driver raw, it would start a query, and migrate up would create a database
// named for the text before it.
func TestCLI_APathWithAQuestionMark(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip(`"?" cannot appear in a Windows file name`)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "sqi?mode=ro.db")
	for _, args := range [][]string{
		{"migrate", "up", "--db", path},
		{"migrate", "status", "--db", path},
		{"worker", "list", "--db", path},
	} {
		prepareRoot(args)
		var runErr error
		out := captureStdout(t, func() { runErr = Execute() })
		if runErr != nil {
			t.Fatalf("%v: %v", args, runErr)
		}
		if args[1] == "status" && strings.Contains(out, "pending") {
			t.Errorf("migrate status after up lists pending migrations:\n%s", out)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no database at %s: %v", path, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sqi")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a database named for the text before the \"?\" exists (err %v)", err)
	}
}

// TestCLI_RefusesANewerSchema verifies that a database migrated by a newer
// sqi-server is refused by the commands that would write to it, while backup,
// which never opens the store, still works: it is the safety net taken before
// a rollback.
func TestCLI_RefusesANewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	createTestDB(t, path)
	maxV, err := migrations.MaxVersion("sqlite")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, maxV+1); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"migrate", "up", "--db", path},
		{"worker", "list", "--db", path},
	} {
		prepareRoot(args)
		var runErr error
		_ = captureStdout(t, func() { runErr = Execute() })
		if runErr == nil || !strings.Contains(runErr.Error(), "newer than this sqi-server") {
			t.Errorf("%v = %v, want a newer-schema refusal", args, runErr)
		}
	}

	out := filepath.Join(t.TempDir(), "backup.db")
	prepareRoot([]string{"backup", "--db", path, "--out", out})
	var runErr error
	_ = captureStdout(t, func() { runErr = Execute() })
	if runErr != nil {
		t.Fatalf("backup of a newer schema: %v", runErr)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("no backup file: %v", err)
	}
}

// restoreFixture makes a migrated SQLite at dir/sqi.db holding one running
// task, a backup of it at dir/backup.db, and a NATS directory at dir/nats.
func restoreFixture(t *testing.T) (db, backup, natsDir string) {
	t.Helper()
	dir := t.TempDir()
	db = filepath.Join(dir, "sqi.db")
	st, err := sqlite.Open(t.Context(), db, sqlite.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateFarm(t.Context(), store.Farm{ID: "f1", Name: "f"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateQueue(t.Context(), store.Queue{ID: "q1", FarmID: "f1", Name: "q"}); err != nil {
		t.Fatal(err)
	}
	storetest.NewJob("j1", "f1", "q1").
		Step("s1", store.StepStatusReady).
		Task("t1", "s1", store.TaskStatusRunning).
		SubmitLeasing(t, st, "w1")
	backup = filepath.Join(dir, "backup.db")
	if err := st.Backup(t.Context(), backup); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	natsDir = filepath.Join(dir, "nats")
	if err := os.MkdirAll(natsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return db, backup, natsDir
}

// TestRestoreCmd_SQLite restores a SQLite backup holding a running task
// through the CLI, and checks the summary names the fence and where the
// previous database and the NATS directory went.
func TestRestoreCmd_SQLite(t *testing.T) {
	db, backup, natsDir := restoreFixture(t)

	prepareRoot([]string{"restore", backup, "--db", db, "--nats-data-dir", natsDir})
	var runErr error
	out := captureStdout(t, func() { runErr = Execute() })
	if runErr != nil {
		t.Fatalf("restore: %v", runErr)
	}
	for _, want := range []string{"fenced 1 attempt(s)", db + ".pre-restore-", natsDir + ".pre-restore-"} {
		if !strings.Contains(out, want) {
			t.Errorf("restore summary missing %q\ngot:\n%s", want, out)
		}
	}

	t.Run("neither a backup nor --in-place is an error", func(t *testing.T) {
		prepareRoot([]string{"restore", "--db", db, "--nats-data-dir", natsDir})
		err := Execute()
		if err == nil || !strings.Contains(err.Error(), "give a backup file, or --in-place") {
			t.Fatalf("restore with no backup = %v, want a usage error", err)
		}
	})

	t.Run("both a backup and --in-place is an error", func(t *testing.T) {
		prepareRoot([]string{"restore", backup, "--in-place", "--db", db, "--nats-data-dir", natsDir})
		err := Execute()
		if err == nil || !strings.Contains(err.Error(), "give a backup file, or --in-place") {
			t.Fatalf("restore with a backup and --in-place = %v, want a usage error", err)
		}
	})
}

// TestRestoreCmd_UnloadableConfig verifies restore does not fall back to the
// built-in defaults when the configuration fails to load without -c, as the
// other CLI commands do: it would restore into, and quarantine, the wrong
// places. Naming both the store and the NATS directory is the way through.
func TestRestoreCmd_UnloadableConfig(t *testing.T) {
	breakages := map[string]func(t *testing.T){
		"unparseable SQI_ variable": func(t *testing.T) {
			t.Setenv("SQI_SCHEDULER_TICK_INTERVAL", "not-a-duration")
		},
		"malformed discovered config file": func(t *testing.T) {
			if err := os.MkdirAll("config", 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join("config", "sqi-server.yaml"), []byte("store: ["), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, breakConfig := range breakages {
		t.Run(name, func(t *testing.T) {
			unsetStoreSQLitePathEnv(t)
			isolateConfigDiscovery(t)
			breakConfig(t)
			db, backup, natsDir := restoreFixture(t)
			// What the built-in defaults would name, relative to the working
			// directory: neither may be touched.
			if err := os.MkdirAll(filepath.Join("data", "nats"), 0o700); err != nil {
				t.Fatal(err)
			}

			for _, args := range [][]string{
				{"restore", backup},
				{"restore", backup, "--db", db},
				{"restore", backup, "--nats-data-dir", natsDir},
			} {
				prepareRoot(args)
				var runErr error
				_ = captureStderr(t, func() { _ = captureStdout(t, func() { runErr = Execute() }) })
				if runErr == nil || !strings.Contains(runErr.Error(), "could not load configuration") ||
					!strings.Contains(runErr.Error(), "--nats-data-dir") {
					t.Fatalf("%v = %v, want a refusal naming the config problem", args, runErr)
				}
			}
			for _, p := range []string{filepath.Join("data", "nats"), natsDir, db} {
				if _, err := os.Stat(p); err != nil {
					t.Fatalf("%s moved although restore refused: %v", p, err)
				}
			}
			if _, err := os.Stat("sqi.db"); err == nil {
				t.Fatal("restore created the default database although it refused")
			}

			prepareRoot([]string{"restore", backup, "--db", db, "--nats-data-dir", natsDir})
			var runErr error
			var out string
			_ = captureStderr(t, func() { out = captureStdout(t, func() { runErr = Execute() }) })
			if runErr != nil || !strings.Contains(out, "fenced 1 attempt(s)") {
				t.Fatalf("restore with both explicit = %v; stdout:\n%s", runErr, out)
			}
			if _, err := os.Stat(filepath.Join("data", "nats")); err != nil {
				t.Fatalf("the default NATS directory moved: %v", err)
			}
		})
	}
}

// TestRestoreCmd_RefusesAnEmptyNATSDataDir verifies an explicit empty
// --nats-data-dir is refused rather than silently skipping the quarantine.
func TestRestoreCmd_RefusesAnEmptyNATSDataDir(t *testing.T) {
	db, backup, natsDir := restoreFixture(t)
	prepareRoot([]string{"restore", backup, "--db", db, "--nats-data-dir", ""})
	err := Execute()
	if err == nil || !strings.Contains(err.Error(), "--nats-data-dir is empty") {
		t.Fatalf("restore --nats-data-dir \"\" = %v, want a refusal", err)
	}
	if _, err := os.Stat(db + ".restore-tmp"); err == nil {
		t.Fatal("restore staged a copy although it refused")
	}
	if _, err := os.Stat(natsDir); err != nil {
		t.Fatal(err)
	}
}

// TestRootCmd_StoreSubcommands verifies "store" offers copy.
func TestRootCmd_StoreSubcommands(t *testing.T) {
	var got []string
	for _, c := range storeCmd.Commands() {
		got = append(got, c.Name())
	}
	if want := []string{"copy"}; !slices.Equal(got, want) {
		t.Errorf("store subcommands = %v; want %v", got, want)
	}
	if !slices.Contains(rootCmd.Commands(), storeCmd) {
		t.Error("store is not registered on the root command")
	}
}

// storeCopyFixture migrates a SQLite database holding one farm.
func storeCopyFixture(t *testing.T) string {
	t.Helper()
	db := filepath.Join(t.TempDir(), "sqi.db")
	st, err := sqlite.Open(t.Context(), db, sqlite.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateFarm(t.Context(), store.Farm{ID: "farm-copy", Name: "copied"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return db
}

// TestStoreCopyCmd_RequiresATarget verifies store copy with no DSN from any
// layer refuses, naming every way to give one.
func TestStoreCopyCmd_RequiresATarget(t *testing.T) {
	isolateConfigDiscovery(t)
	unsetStoreSQLitePathEnv(t)
	t.Setenv("SQI_STORE_POSTGRES_DSN", "")
	db := storeCopyFixture(t)

	prepareRoot([]string{"store", "copy", "--from", db})
	err := Execute()
	if err == nil || !strings.Contains(err.Error(), "no PostgreSQL DSN") {
		t.Fatalf("store copy without a DSN = %v, want a refusal", err)
	}

	prepareRoot([]string{"store", "copy", "--from", db, "--to", ""})
	if err := Execute(); err == nil || !strings.Contains(err.Error(), "--to is empty") {
		t.Fatalf("store copy --to \"\" = %v, want a refusal", err)
	}
}

// TestStoreCopyCmd_RefusesWhileAServerHoldsTheSource verifies the command
// does not read a database a server is using. The source's lock is taken
// before the target is contacted, so no PostgreSQL is needed.
func TestStoreCopyCmd_RefusesWhileAServerHoldsTheSource(t *testing.T) {
	isolateConfigDiscovery(t)
	db := storeCopyFixture(t)
	srv, err := sqlite.Open(t.Context(), db, sqlite.Options{ServerLock: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	prepareRoot([]string{"store", "copy", "--from", db, "--to", "postgres://127.0.0.1:1/none?connect_timeout=1"})
	if err := Execute(); !errors.Is(err, sqlite.ErrServerLockHeld) {
		t.Fatalf("store copy beside a server = %v, want ErrServerLockHeld", err)
	}
}

// TestStoreCopyCmd_UnloadableConfig verifies store copy does not fall back to
// built-in defaults for a configuration that failed to load: it would copy
// from, or into, a database the operator did not name.
func TestStoreCopyCmd_UnloadableConfig(t *testing.T) {
	isolateConfigDiscovery(t)
	unsetStoreSQLitePathEnv(t)
	t.Setenv("SQI_SCHEDULER_TICK_INTERVAL", "not-a-duration")
	db := storeCopyFixture(t)
	for _, args := range [][]string{
		{"store", "copy"},
		{"store", "copy", "--from", db},
		{"store", "copy", "--to", "postgres://127.0.0.1:1/none"},
	} {
		prepareRoot(args)
		var runErr error
		_ = captureStderr(t, func() { runErr = Execute() })
		if runErr == nil || !strings.Contains(runErr.Error(), "could not load configuration") ||
			!strings.Contains(runErr.Error(), "--from and --to") {
			t.Fatalf("%v = %v, want a refusal naming the config problem", args, runErr)
		}
	}
}

// TestStoreCopyCmd_CopiesIntoPostgres runs the command end to end into an
// empty PostgreSQL database and opens a store on the copy.
func TestStoreCopyCmd_CopiesIntoPostgres(t *testing.T) {
	isolateConfigDiscovery(t)
	db := storeCopyFixture(t)
	dsn := storetest.PostgresEmptyDSN(t)

	prepareRoot([]string{"store", "copy", "--from", db, "--to", dsn})
	var runErr error
	out := captureStdout(t, func() { runErr = Execute() })
	if runErr != nil {
		t.Fatalf("store copy: %v", runErr)
	}
	for _, want := range []string{"farms", "1 rows", "copied and verified", "set store.driver: postgres"} {
		if !strings.Contains(out, want) {
			t.Errorf("store copy output missing %q\ngot:\n%s", want, out)
		}
	}
	st, err := postgres.Open(t.Context(), dsn, postgres.Options{MaxConns: 2})
	if err != nil {
		t.Fatalf("open the copy: %v", err)
	}
	defer st.Close()
	if f, err := st.GetFarm(t.Context(), "farm-copy"); err != nil || f.Name != "copied" {
		t.Fatalf("farm in the copy = %+v, %v", f, err)
	}
}
