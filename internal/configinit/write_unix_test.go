// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package configinit

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// These run against the real file system and account database, which the
// in-memory tests cannot see.

func TestRealUnixSys_ReplaceResetsModeAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	tgt := filepath.Join(dir, "sqi-worker.yaml")
	if err := os.WriteFile(tgt, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tgt, 0o644); err != nil { // beat the umask
		t.Fatal(err)
	}
	if err := writeUnix(tgt, []byte("new"), unixOpts{}, realUnixSys()); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(tgt)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
	got, err := os.ReadFile(tgt)
	if err != nil || string(got) != "new" {
		t.Errorf("content = %q, %v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the target", len(entries))
	}
}

func TestRealUnixSys_MissingGroup(t *testing.T) {
	if _, err := lookupGroup("sqi-no-such-group-for-test"); !errors.Is(err, errGroupNotFound) {
		t.Errorf("error = %v, want errGroupNotFound", err)
	}
}

// Not parallel: the umask belongs to the whole process. The umask clears the
// owner's write bit, so a directory left at the mode MkdirAll produced is
// 0500 and the test sees it. The temporary directory is made before the umask
// changes, which would otherwise leave it unwritable.
func TestRealUnixSys_CreatedDirectoryIgnoresUmask(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sqi")
	old := syscall.Umask(0o277)
	defer syscall.Umask(old)
	if err := writeUnix(filepath.Join(dir, "x.yaml"), []byte("b"), unixOpts{Shared: false}, realUnixSys()); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Errorf("created directory mode = %v, want 0700", st.Mode().Perm())
	}
}
