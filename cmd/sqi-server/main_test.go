// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

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

// TestMigrateCmd_Postgres_DownRefusesTheBaseline pins that `migrate down` on
// PostgreSQL never rolls back the baseline, whose Down drops every table: the
// command fails and the schema stays applied.
func TestMigrateCmd_Postgres_DownRefusesTheBaseline(t *testing.T) {
	dsn := storetest.PostgresEmptyDSN(t)
	prepareRoot([]string{"migrate", "up", "--dsn", dsn})
	_ = captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("migrate up: %v", err)
		}
	})
	prepareRoot([]string{"migrate", "down", "--dsn", dsn})
	_ = captureStdout(t, func() {
		if err := Execute(); err == nil || !strings.Contains(err.Error(), "baseline") {
			t.Fatalf("migrate down from the baseline = %v; want a refusal naming the baseline", err)
		}
	})
	prepareRoot([]string{"migrate", "status", "--dsn", dsn})
	out := captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("migrate status: %v", err)
		}
	})
	if strings.Contains(out, "pending") {
		t.Fatalf("migrate status after a refused down:\n%s", out)
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
