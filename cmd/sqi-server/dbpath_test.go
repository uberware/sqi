// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// TestResolveDBPath_Precedence walks the four-layer precedence order:
// explicit flag > config layer (file/SQI_STORE_SQLITE_PATH) > legacy
// SQI_SQLITE_PATH > built-in default.
func TestResolveDBPath_Precedence(t *testing.T) {
	t.Run("explicit flag beats everything else", func(t *testing.T) {
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
		withConfigFile(t, "")
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
		withConfigFile(t, "")
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
		withConfigFile(t, "")

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
// specific config file.
func TestResolveDBPath_MalformedConfigIsAnError(t *testing.T) {
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
