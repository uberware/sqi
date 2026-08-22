// SPDX-License-Identifier: AGPL-3.0-or-later

package certgen_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/certgen"
)

func TestWriteCA_FileModes(t *testing.T) {
	dir := t.TempDir()
	ca, err := certgen.NewCA("sqi farm CA", time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	if err := certgen.WriteCA(dir, ca); err != nil {
		t.Fatalf("WriteCA: %v", err)
	}

	for name, want := range map[string]os.FileMode{
		"ca.crt": 0o644,
		"ca.key": 0o600,
	} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %04o, want %04o", name, got, want)
		}
	}
}

func TestWriteCA_RefusesToOverwriteExistingCA(t *testing.T) {
	dir := t.TempDir()
	first, err := certgen.NewCA("sqi farm CA", time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	if err := certgen.WriteCA(dir, first); err != nil {
		t.Fatalf("WriteCA (first): %v", err)
	}
	original, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatalf("read ca.key: %v", err)
	}

	second, err := certgen.NewCA("sqi farm CA", time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	err = certgen.WriteCA(dir, second)
	if !errors.Is(err, certgen.ErrCAExists) {
		t.Fatalf("WriteCA (second) error = %v, want ErrCAExists", err)
	}

	after, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatalf("re-read ca.key: %v", err)
	}
	if string(after) != string(original) {
		t.Error("ca.key was modified despite the refusal; a replaced farm CA invalidates every certificate issued from it")
	}
}

func TestWriteLeaf_FileModes(t *testing.T) {
	dir := t.TempDir()
	ca, err := certgen.NewCA("sqi farm CA", time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	leaf, err := ca.NewServerCert([]string{"sqi.example"}, time.Hour)
	if err != nil {
		t.Fatalf("NewServerCert: %v", err)
	}
	if err := certgen.WriteLeaf(dir, "server", leaf); err != nil {
		t.Fatalf("WriteLeaf: %v", err)
	}

	for name, want := range map[string]os.FileMode{
		"server.crt": 0o644,
		"server.key": 0o600,
	} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %04o, want %04o", name, got, want)
		}
	}
}
