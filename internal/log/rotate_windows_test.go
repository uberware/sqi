// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package log

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// openWithoutShareDelete opens path for reading with the share flags of a
// tailing reader that did not grant FILE_SHARE_DELETE.
func openWithoutShareDelete(t *testing.T, path string) windows.Handle {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, // deliberately no FILE_SHARE_DELETE
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// enforcesShareModes reports whether this host refuses to rename a file that
// another handle holds open without FILE_SHARE_DELETE. Windows normally does;
// some CI images (GitHub's windows-latest) do not, and on those the scenario
// under test cannot be reproduced.
func enforcesShareModes(t *testing.T) bool {
	t.Helper()
	dir := t.TempDir()
	scratch := filepath.Join(dir, "probe.txt")
	if err := os.WriteFile(scratch, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := openWithoutShareDelete(t, scratch)
	renamed := scratch + ".renamed"
	err := os.Rename(scratch, renamed)
	if cerr := windows.CloseHandle(h); cerr != nil {
		t.Fatal(cerr)
	}
	// Leave nothing behind whichever file survived; t.TempDir cleans the rest.
	os.Remove(scratch)
	os.Remove(renamed)
	return err != nil
}

// TestRotatingFile_RenameBlockedByOpenHandleKeepsAppending reproduces an
// operator tailing the log with a reader that did not grant FILE_SHARE_DELETE:
// Windows then refuses the rename rotation needs. No line may be lost, and once
// the handle closes rotation must resume.
func TestRotatingFile_RenameBlockedByOpenHandleKeepsAppending(t *testing.T) {
	if !enforcesShareModes(t) {
		t.Skip("this host does not enforce Windows share modes: a rename succeeds while another handle is open without FILE_SHARE_DELETE, so the blocked-rotation scenario cannot be reproduced")
	}
	path := filepath.Join(t.TempDir(), "a.log")
	// 5 backups: enough that the four rotations after the handle closes keep
	// every line, so any missing line is the writer's fault, not retention's.
	w, err := openRotatingFile(path, 10, 5)
	if err != nil {
		t.Fatal(err)
	}
	h := openWithoutShareDelete(t, path)
	writeLines(t, w, "line1", "line2", "line3")
	if err := windows.CloseHandle(h); err != nil {
		t.Fatal(err)
	}
	writeLines(t, w, "line4", "line5", "line6", "line7")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	var all strings.Builder
	for _, name := range []string{path, path + ".1", path + ".2", path + ".3", path + ".4", path + ".5"} {
		if b, err := os.ReadFile(name); err == nil {
			all.Write(b)
		}
	}
	for _, want := range []string{"line1", "line2", "line3", "line4", "line5", "line6", "line7"} {
		if !strings.Contains(all.String(), want+"\n") {
			t.Errorf("line %q lost", want)
		}
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("rotation never resumed after the blocking handle closed: %v", err)
	}
}
