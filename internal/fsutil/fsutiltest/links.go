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

// SymlinkOrSkip makes link a symbolic link to target, skipping the test where
// the OS refuses (Windows without Developer Mode or the symlink privilege).
func SymlinkOrSkip(t testing.TB, link, target string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
}

// DirLink makes link point at the directory target: a symbolic link, or on
// Windows, where that needs a privilege, a junction. It skips the test only
// where neither can be made.
func DirLink(t testing.TB, link, target string) {
	t.Helper()
	err := os.Symlink(target, link)
	switch {
	case err == nil:
	case runtime.GOOS == "windows":
		Junction(t, link, target)
	default:
		t.Skipf("cannot create a symlink here: %v", err)
	}
}
