// SPDX-License-Identifier: AGPL-3.0-or-later

package fsutiltest_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/uberware/sqi/internal/fsutil/fsutiltest"
)

// requireReadsThrough fails unless name, read through link, is the file
// written in target.
func requireReadsThrough(t *testing.T, link, target, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(target, name), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(link, name)); err != nil || string(got) != "x" {
		t.Fatalf("read %s through %s = %q, %v; want the target's file", name, link, got, err)
	}
}

func TestJunction(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("junctions exist only on Windows")
	}
	target, link := t.TempDir(), filepath.Join(t.TempDir(), "link")
	fsutiltest.Junction(t, link, target)
	requireReadsThrough(t, link, target, "f")
}

func TestDirLink(t *testing.T) {
	target, link := t.TempDir(), filepath.Join(t.TempDir(), "link")
	fsutiltest.DirLink(t, link, target)
	requireReadsThrough(t, link, target, "f")
}

func TestSymlinkOrSkip(t *testing.T) {
	target := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	fsutiltest.SymlinkOrSkip(t, link, target)
	if got, err := os.ReadFile(link); err != nil || string(got) != "x" {
		t.Fatalf("read through %s = %q, %v; want the target's file", link, got, err)
	}
}
