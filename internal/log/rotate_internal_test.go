// SPDX-License-Identifier: AGPL-3.0-or-later

package log

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func writeLines(t *testing.T, w *RotatingFile, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if _, err := w.Write([]byte(l + "\n")); err != nil {
			t.Fatalf("write %q: %v", l, err)
		}
	}
}

func TestRotatingFile_AppendsToExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.log")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := openRotatingFile(path, 1<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	writeLines(t, w, "new")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "old\nnew\n" {
		t.Fatalf("content = %q, want old then new", got)
	}
}

func TestRotatingFile_RotatesAtLimitAndKeepsMaxBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.log")
	w, err := openRotatingFile(path, 10, 2) // 10 bytes: each 6-byte line after the first rotates
	if err != nil {
		t.Fatal(err)
	}
	writeLines(t, w, "line1", "line2", "line3", "line4")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "line4\n" {
		t.Errorf("current = %q, want line4", got)
	}
	if got := readFile(t, path+".1"); got != "line3\n" {
		t.Errorf(".1 = %q, want line3", got)
	}
	if got := readFile(t, path+".2"); got != "line2\n" {
		t.Errorf(".2 = %q, want line2", got)
	}
	if _, err := os.Stat(path + ".3"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".3 exists (err=%v); max_backups=2 must drop line1", err)
	}
}

func TestRotatingFile_ZeroBackupsTruncates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.log")
	w, err := openRotatingFile(path, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	writeLines(t, w, "line1", "line2")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "line2\n" {
		t.Errorf("current = %q, want line2", got)
	}
	if _, err := os.Stat(path + ".1"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".1 exists with max_backups=0 (err=%v)", err)
	}
}

func TestRotatingFile_RenameFailureKeepsAppendingAndRetriesLater(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.log")
	w, err := openRotatingFile(path, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	w.rename = func(_, _ string) error {
		attempts++
		return errors.New("sharing violation")
	}
	writeLines(t, w, "line1", "line2") // rotation attempted before line2, fails
	if attempts == 0 {
		t.Fatal("no rotation attempted")
	}
	first := attempts
	// The failed rotation moved the threshold to size+max = 6+10 = 16; "x"
	// takes the file to 14, still within it, so no retry yet.
	writeLines(t, w, "x")
	if attempts != first {
		t.Fatalf("retried after only 2 more bytes (attempts %d -> %d); must wait another max size", first, attempts)
	}
	writeLines(t, w, "line3", "line4") // line3 reaches 20 > 16: retries
	if attempts == first {
		t.Fatal("never retried rotation after another max size of output")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	for _, want := range []string{"line1", "line2", "x", "line3", "line4"} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("line %q lost after failed rotation; file = %q", want, got)
		}
	}
}

// A reader holding the current file without FILE_SHARE_DELETE (Get-Content
// -Wait) blocks only the rename of the current file. Every deferred retry must
// leave the existing backups alone, not drop the oldest one each time.
func TestRotatingFile_BlockedCurrentFileRenameKeepsBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.log")
	for i, content := range []string{"b1\n", "b2\n", "b3\n"} {
		if err := os.WriteFile(backupName(path, i+1), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w, err := openRotatingFile(path, 10, 3)
	if err != nil {
		t.Fatal(err)
	}
	blocked := 0
	w.rename = func(oldpath, newpath string) error {
		if oldpath == path {
			blocked++
			return errors.New("sharing violation")
		}
		return os.Rename(oldpath, newpath)
	}
	// Enough output for several deferred retries.
	writeLines(t, w, "line1", "line2", "line3", "line4", "line5", "line6", "line7", "line8")
	if blocked < 2 {
		t.Fatalf("rotation retried %d time(s), want several", blocked)
	}
	for i, want := range []string{"b1\n", "b2\n", "b3\n"} {
		if got := readFile(t, backupName(path, i+1)); got != want {
			t.Errorf("backup %d = %q, want %q", i+1, got, want)
		}
	}

	// Once the handle closes, rotation shifts the backups as usual.
	w.rename = os.Rename
	writeLines(t, w, "after")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "after\n" {
		t.Errorf("current = %q, want the line written after rotation", got)
	}
	if got := readFile(t, backupName(path, 1)); !strings.Contains(got, "line8\n") {
		t.Errorf(".1 = %q, want the held file's lines", got)
	}
	if got := readFile(t, backupName(path, 2)); got != "b1\n" {
		t.Errorf(".2 = %q, want the old .1", got)
	}
}

// A backup rename failing after the current file was moved aside puts the
// current file back, so its lines stay where the writer reopens them.
func TestRotatingFile_BackupShiftFailurePutsCurrentFileBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.log")
	if err := os.WriteFile(backupName(path, 1), []byte("b1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := openRotatingFile(path, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	w.rename = func(oldpath, newpath string) error {
		if oldpath == backupName(path, 1) {
			return errors.New("sharing violation")
		}
		return os.Rename(oldpath, newpath)
	}
	writeLines(t, w, "line1", "line2")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "line1\nline2\n" {
		t.Errorf("current = %q, want both lines", got)
	}
	if _, err := os.Stat(path + ".rotating"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pending file left behind: %v", err)
	}
}

func TestOpenRotatingFile_RejectsBadLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.log")
	if _, err := OpenRotatingFile(path, 0, 1); err == nil {
		t.Error("max size 0 accepted")
	}
	if _, err := OpenRotatingFile(path, 1, -1); err == nil {
		t.Error("negative backups accepted")
	}
}

func TestOutput_EmptyPathIsStderrAndCloseIsNoop(t *testing.T) {
	w, err := Output("", 100, 5)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// stderr must still be usable after Close.
	if _, err := os.Stderr.Write(nil); err != nil {
		t.Fatalf("stderr closed by Output's closer: %v", err)
	}
}

func TestOutput_OpenFailureReturnsNilWriter(t *testing.T) {
	// A directory that does not exist: the open must fail.
	w, err := Output(filepath.Join(t.TempDir(), "missing", "a.log"), 100, 5)
	if err == nil {
		t.Fatal("open of a path in a missing directory succeeded")
	}
	if w != nil {
		t.Fatalf("writer = %#v on error; want a nil interface, not a typed-nil *RotatingFile", w)
	}
}

// failOpens replaces w's opener with one that fails while *failing is true, as
// ENOSPC or a transient Windows lock would.
func failOpens(w *RotatingFile, failing *bool) {
	orig := w.openFile
	w.openFile = func(name string) (*os.File, error) {
		if *failing {
			return nil, errors.New("no space left on device")
		}
		return orig(name)
	}
}

func TestRotatingFile_RecoversAfterFailedReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.log")
	w, err := openRotatingFile(path, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	failing := false
	failOpens(w, &failing)
	writeLines(t, w, "line1")

	failing = true
	if _, err := w.Write([]byte("line2\n")); err == nil { // rotates, then the reopen fails
		t.Fatal("write that hit a failed reopen returned nil; that record is lost and must say so")
	}

	failing = false
	writeLines(t, w, "line3") // must reopen and carry on, not die on a closed file

	if err := w.Close(); err != nil {
		t.Fatalf("close after recovery: %v", err)
	}
	if got := readFile(t, path); got != "line3\n" {
		t.Errorf("current = %q, want line3", got)
	}
	if got := readFile(t, path+".1"); got != "line1\n" {
		t.Errorf(".1 = %q, want line1", got)
	}
	// Idempotent: a second Close is not an error either.
	if err := w.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestRotatingFile_CloseAfterUnrecoveredReopenIsNil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.log")
	w, err := openRotatingFile(path, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	failing := false
	failOpens(w, &failing)
	writeLines(t, w, "line1")

	failing = true
	if _, err := w.Write([]byte("line2\n")); err == nil {
		t.Fatal("write that hit a failed reopen returned nil")
	}
	// The opener is still failing: the next write must retry it (and fail
	// honestly), not touch the closed file.
	if _, err := w.Write([]byte("line3\n")); err == nil {
		t.Fatal("write with the opener still failing returned nil")
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close with no open file: %v", err)
	}
	// A closed writer stays closed: it must not reopen the file behind the
	// caller's back and leak the handle.
	failing = false
	if _, err := w.Write([]byte("late\n")); err == nil {
		t.Fatal("write after Close succeeded")
	}
}

func TestFileProblem_EmptyIsStderr(t *testing.T) {
	if got := FileProblem("", "x.log"); got != "" {
		t.Fatalf("FileProblem(\"\") = %q, want \"\" (stderr)", got)
	}
}
