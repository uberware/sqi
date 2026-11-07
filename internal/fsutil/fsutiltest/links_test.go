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

// recordTB records whether a helper failed or skipped the test, without
// stopping the test running it.
type recordTB struct {
	testing.TB

	failed, skipped bool
}

func (r *recordTB) Fatalf(string, ...any) { r.failed = true }
func (r *recordTB) Skipf(string, ...any)  { r.skipped = true }

// Off Windows a symlink is never refused for want of privilege, so a failure
// is a broken fixture, which must fail the test rather than skip it.
func TestSymlinkOrSkip_FailsOnABrokenFixtureOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows skips when the symlink privilege is missing")
	}
	taken := filepath.Join(t.TempDir(), "taken")
	if err := os.WriteFile(taken, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &recordTB{TB: t}
	fsutiltest.SymlinkOrSkip(rec, taken, "anything")
	if !rec.failed || rec.skipped {
		t.Errorf("SymlinkOrSkip onto an existing file: failed=%v skipped=%v, want a failure", rec.failed, rec.skipped)
	}
}
