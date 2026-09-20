// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package staging

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// These two tests are the POSIX counterpart of the branch-precision tests in
// staging_windows_test.go, and they exist for exactly the same reason.
//
// openStageOutSource refuses a symlink in two unrelated places: the ADVISORY
// root.Lstat, when the symlink is at rel's final component, and the
// kernel-enforced root.OpenFile, when the lookup leaves the root. Both
// operator-facing messages contain the word "symlink" — they have to, since
// both really are about one — so the package's existing
// strings.Contains(err, "symlink") assertions (staging_boundary_test.go, in
// the external test package, where these unexported sentinels are not
// visible) cannot tell which branch answered. Deleting the advisory block
// would leave them green, because a symlink pointing OUT of scratch then
// simply gets refused by the open instead, with a message that also says
// "symlink". Asserting on the sentinel is what makes each branch load-bearing.

// TestStageOut_SymlinkSourceRefusedByAdvisoryLstat pins the advisory branch:
// root.Lstat does not follow the final component, so it sees the symlink
// itself and refuses before the open is ever attempted.
func TestStageOut_SymlinkSourceRefusedByAdvisoryLstat(t *testing.T) {
	scratch := t.TempDir()
	stagedDir := filepath.Join(scratch, "0")
	if err := os.MkdirAll(stagedDir, 0o750); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("daemon-only-contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(stagedDir, "render.exr")); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(scratch)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	rel := filepath.Join("0", "render.exr")

	// Assert the setup actually put the advisory branch in play: if Lstat
	// failed, this would be testing the open again.
	li, err := root.Lstat(rel)
	if err != nil {
		t.Fatalf("Lstat on a final-component symlink: %v; this test would prove nothing", err)
	}
	if li.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("Lstat mode = %v, want fs.ModeSymlink; this test would prove nothing", li.Mode())
	}

	f, err := openStageOutSource(root, rel)
	if err == nil {
		f.Close()
		t.Fatal("want error refusing a symlinked stage-out source")
	}
	if !errors.Is(err, errStageOutSymlink) {
		t.Errorf("err = %v, want errStageOutSymlink (the advisory Lstat branch)", err)
	}
	if errors.Is(err, errStageOutEscape) {
		t.Errorf("err = %v, want the advisory Lstat to have refused this, not the open", err)
	}
}

// TestStageOut_EscapeRefusedByOpen pins the other branch: the symlink is an
// INTERMEDIATE component, so root.Lstat itself fails, the advisory block is
// skipped entirely, and the kernel-enforced open is what refuses the lookup.
func TestStageOut_EscapeRefusedByOpen(t *testing.T) {
	scratch := t.TempDir()
	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "render.exr"), []byte("daemon-only-contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The task's per-index scratch subdirectory is a symlink escaping scratch;
	// the file behind it is entirely ordinary.
	if err := os.Symlink(outsideDir, filepath.Join(scratch, "0")); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(scratch)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	f, err := openStageOutSource(root, filepath.Join("0", "render.exr"))
	if err == nil {
		f.Close()
		t.Fatal("want error refusing a source resolving outside scratch")
	}
	if !errors.Is(err, errStageOutEscape) {
		t.Errorf("err = %v, want errStageOutEscape (the kernel-enforced open refused the lookup)", err)
	}
	if errors.Is(err, errStageOutSymlink) {
		t.Errorf("err = %v, want the OPEN to have refused this, not the advisory Lstat", err)
	}
	// An escape must never be classified as a mundane access failure, or the
	// honest wording added for the permission case would swallow a real attack.
	if errors.Is(err, errStageOutUnreadable) {
		t.Errorf("err = %v, want a containment refusal, not an access/I/O failure", err)
	}
}
