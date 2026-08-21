// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
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
	orig := f.Changed
	f.Changed = false
	t.Cleanup(func() { f.Changed = orig })
}

// prepareRoot sets the args that rootCmd will parse on the next Execute() call
// and redirects cobra's own output writers (help, usage, error messages) to a
// discard buffer so test output stays clean.
func prepareRoot(args []string) {
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
// database, applies pragmas, and configures goose when given a real path.
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
}

// TestMigrateCmd_WithTempDB exercises the migrate subcommands (status, up)
// against a real temporary SQLite database.  These tests cover the RunE
// closures that call openMigrateDB and goose operations.
func TestMigrateCmd_WithTempDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test-migrate.db")

	t.Run("migrate status succeeds on fresh database", func(t *testing.T) {
		prepareRoot([]string{"migrate", "status", "--db", dbPath})
		// goose.Status prints to its default logger; suppress stdout so we
		// don't pollute test output.
		_ = captureStdout(t, func() {
			err := Execute()
			if err != nil {
				t.Errorf("Execute(migrate status) error = %v", err)
			}
		})
	})

	t.Run("migrate up applies all pending migrations", func(t *testing.T) {
		prepareRoot([]string{"migrate", "up", "--db", dbPath})
		_ = captureStdout(t, func() {
			err := Execute()
			if err != nil {
				t.Errorf("Execute(migrate up) error = %v", err)
			}
		})
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
