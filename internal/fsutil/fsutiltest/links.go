// SPDX-License-Identifier: AGPL-3.0-or-later

// Package fsutiltest makes filesystem links for tests.
package fsutiltest

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
)

// Junction makes link a directory junction to target. A junction, unlike a
// symbolic link, needs no privilege on Windows. Junctions exist only on
// Windows, so anywhere else it fails the test.
func Junction(t testing.TB, link, target string) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Fatalf("fsutiltest.Junction: junctions exist only on Windows, not %s", runtime.GOOS)
	}
	if out, err := exec.CommandContext(t.Context(), "cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J %q %q: %v: %s", link, target, err, out)
	}
}

// SymlinkOrSkip makes link a symbolic link to target. Windows refuses one
// without Developer Mode or the symlink privilege, so there it skips the test;
// anywhere else a failure is a broken fixture and fails it.
func SymlinkOrSkip(t testing.TB, link, target string) {
	t.Helper()
	err := os.Symlink(target, link)
	switch {
	case err == nil:
	case runtime.GOOS == "windows":
		t.Skipf("cannot create a symlink here: %v", err)
	default:
		t.Fatalf("symlink %s -> %s: %v", link, target, err)
	}
}

// DirLink makes link point at the directory target: a symbolic link, or on
// Windows, where that needs a privilege, a junction.
func DirLink(t testing.TB, link, target string) {
	t.Helper()
	err := os.Symlink(target, link)
	switch {
	case err == nil:
	case runtime.GOOS == "windows":
		Junction(t, link, target)
	default:
		t.Fatalf("symlink %s -> %s: %v", link, target, err)
	}
}
