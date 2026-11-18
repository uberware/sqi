// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package staging

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/uberware/sqi/internal/fsutil/fsutiltest"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// These tests plant directory junctions (fsutiltest.Junction). A junction
// (IO_REPARSE_TAG_MOUNT_POINT) needs no privilege to create, where an NTFS
// symlink needs SeCreateSymbolicLinkPrivilege or Developer Mode, so it is the
// link an ordinary run-as-user task can make. A path-based guard misses it:
// os.Lstat reports a junction as fs.ModeIrregular rather than fs.ModeSymlink,
// and filepath.EvalSymlinks does not resolve it.

// No Windows test asserts that the copy layer itself refuses a reparse-point
// source. The stage-out copy layer takes an already-open, already-validated
// descriptor (copyFromFile), so there is no path for a junction to be planted
// at; the refusal lives in openStageOutSource, which every test below drives.
// copyFile, the by-path opener, is the stage-in path only. Its O_NOFOLLOW
// guard is a no-op on Windows (noFollowFlag is 0 there), and the two
// primitives that would exercise it are unavailable: a junction is
// directory-only, so opening one by path fails ERROR_ACCESS_DENIED rather than
// exercising any reparse check, and an NTFS file symlink needs
// SeCreateSymbolicLinkPrivilege, which an ordinary task does not hold and
// this suite never creates.

// TestStageOut_RefusesJunctionedScratchSubdir is the primary junction-escape
// test.
//
// A task owns its per-entry scratch subdirectory (StageIn's ChownRecursive
// hands it over, and on Windows isolation ACL-secures it to the target
// account). So it can delete that subdirectory and replace it with a junction
// pointing anywhere on the volume. Stage-out then reads
// <scratchDir>/<i>/<basename>, which resolves through the junction, and the
// elevated daemon would copy those bytes to the job's real output path.
//
// The attack needs no privilege and no race. Without the guard this test fails
// by finding the secret's contents at outOrig.
func TestStageOut_RefusesJunctionedScratchSubdir(t *testing.T) {
	scratch := t.TempDir()
	s := New(scratch, builtinSentinel, false, discardLogger())

	outOrig := filepath.Join(t.TempDir(), "render.exr")
	entries := []protocol.StageEntry{
		{Path: outOrig, Direction: "OUT", ObjectType: "FILE"},
	}

	_, scratchDir, err := s.StageIn(context.Background(), "job1", "att1", entries, nil)
	if err != nil {
		t.Fatalf("StageIn: %v", err)
	}

	// Somewhere outside scratch that must never be leaked, holding a file
	// with the same basename the task's declared output has.
	secretDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(secretDir, "render.exr"), []byte("daemon-only-contents"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The task deletes the scratch subdirectory it owns and junctions it at
	// the secret directory. The junction must be live, or a pass would only
	// mean the attack setup failed.
	stagedDir := filepath.Join(scratchDir, "0")
	if err := os.RemoveAll(stagedDir); err != nil {
		t.Fatal(err)
	}
	fsutiltest.Junction(t, stagedDir, secretDir)
	if _, err := os.Stat(filepath.Join(stagedDir, "render.exr")); err != nil {
		t.Fatalf("junction is not live, so this test would prove nothing: %v", err)
	}

	err = s.StageOut(context.Background(), scratchDir, entries)
	if err == nil {
		t.Fatal("want error refusing a stage-out source reached through a junction")
	}
	// Assert the specific branch, not that the word "junction" appears. The
	// junction here is an intermediate component of rel, so root.Lstat fails
	// on it and the advisory block is skipped; the refusal comes from the
	// kernel-enforced open. Both messages mention a junction, so a substring
	// assertion would pass either way.
	if !errors.Is(err, errStageOutEscape) {
		t.Errorf("err = %v, want errStageOutEscape (the kernel-enforced open refused the lookup)", err)
	}
	if errors.Is(err, errStageOutReparse) {
		t.Errorf("err = %v, want the open to have refused this, not the advisory Lstat", err)
	}
	if !strings.Contains(err.Error(), "outside scratch") {
		t.Errorf("err = %v, want the operator-facing message to name the scratch boundary", err)
	}
	if _, statErr := os.Stat(outOrig); statErr == nil {
		t.Fatal("outOrig exists: the secret's contents reached the job's real output path")
	}
}

// TestStageOut_RefusesJunctionAtFinalComponent covers the advisory Lstat
// branch, which no other test reaches.
//
// TestStageOut_RefusesJunctionedScratchSubdir above plants its junction at an
// intermediate component, so root.Lstat itself errors and the advisory block
// is skipped; that refusal comes from the open. A junction at the final
// component makes root.Lstat succeed and report fs.ModeIrregular (Go surfaces
// an IO_REPARSE_TAG_MOUNT_POINT that way, never as ModeSymlink), so the
// advisory branch answers.
func TestStageOut_RefusesJunctionAtFinalComponent(t *testing.T) {
	scratch := t.TempDir()
	stagedDir := filepath.Join(scratch, "0")
	if err := os.MkdirAll(stagedDir, 0o750); err != nil {
		t.Fatal(err)
	}
	secretDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(secretDir, "inner.txt"), []byte("daemon-only-contents"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The task's declared output path is itself a junction at a directory it
	// must not be able to hand the daemon.
	fsutiltest.Junction(t, filepath.Join(stagedDir, "render.exr"), secretDir)

	root, err := os.OpenRoot(scratch)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	rel := filepath.Join("0", "render.exr")

	// Assert the setup puts the advisory branch in play: if Lstat failed, the
	// test would exercise the open again.
	li, err := root.Lstat(rel)
	if err != nil {
		t.Fatalf("Lstat on a final-component junction: %v; this test would prove nothing", err)
	}
	if li.Mode()&os.ModeIrregular == 0 {
		t.Fatalf("Lstat mode = %v, want fs.ModeIrregular; this test would prove nothing", li.Mode())
	}

	f, err := openStageOutSource(root, rel)
	if err == nil {
		f.Close()
		t.Fatal("want error refusing a junction at the stage-out source itself")
	}
	if !errors.Is(err, errStageOutReparse) {
		t.Errorf("err = %v, want errStageOutReparse (the advisory Lstat branch)", err)
	}
	if errors.Is(err, errStageOutEscape) {
		t.Errorf("err = %v, want the advisory Lstat to have refused this, not the open", err)
	}
}

// TestStageOut_RefusesHardlinkedSourceOnWindows is the Windows half of
// TestStager_StageOut_RefusesHardlinkedSource, which skips on Windows: it
// drives StageOut with the fakeSync fixture, a POSIX "#!/bin/sh" script
// Windows cannot execute. NTFS supports hardlinks and os.Link needs no
// privilege, so a task can create one.
//
// It drives openStageOutSource directly, as the swap test below does. No sync
// command is needed, because the check runs before either transfer mechanism.
func TestStageOut_RefusesHardlinkedSourceOnWindows(t *testing.T) {
	scratch := t.TempDir()
	stagedDir := filepath.Join(scratch, "0")
	if err := os.MkdirAll(stagedDir, 0o750); err != nil {
		t.Fatal(err)
	}

	// A file outside scratch, and a second name for it inside scratch. The
	// staged entry is an ordinary regular file with one inode and two links, so
	// it passes every other check in openStageOutSource.
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("daemon-only-contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(stagedDir, "render.exr")
	if err := os.Link(outside, staged); err != nil {
		t.Skipf("hardlinks unsupported between these temp dirs (not NTFS?): %v", err)
	}

	root, err := os.OpenRoot(scratch)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	f, err := openStageOutSource(root, filepath.Join("0", "render.exr"))
	if err == nil {
		f.Close()
		t.Fatal("want error refusing a stage-out source with an extra hardlink")
	}
	if !errors.Is(err, errStageOutHardlink) {
		t.Errorf("err = %v, want errStageOutHardlink", err)
	}
	if !strings.Contains(err.Error(), "hardlink") {
		t.Errorf("err = %v, want the operator-facing message to mention the hardlink refusal", err)
	}
}

// TestStageOut_SwapAfterValidateIsRefused: a swap of the source between
// validation and the copy cannot change what is copied.
//
// The process tree is not killed when a task exits successfully (see
// executor.runTask / killAndWait), so a background child that outlives its
// task still owns the scratch subdirectory and can swap the source out after
// validation and before the copy. The test drives that window
// deterministically: the two halves are called in sequence with the swap
// performed between them, with no sleep, goroutine or timing dependence.
//
// copyFromFile reads the descriptor openStageOutSource validated, not the
// path. The post-swap check (a fresh path read yields the attacker's bytes)
// confirms the swap took effect.
func TestStageOut_SwapAfterValidateIsRefused(t *testing.T) {
	scratch := t.TempDir()
	stagedDir := filepath.Join(scratch, "0")
	if err := os.MkdirAll(stagedDir, 0o750); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(stagedDir, "render.exr")
	if err := os.WriteFile(src, []byte("legit-task-output"), 0o600); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(scratch)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	rel := filepath.Join("0", "render.exr")
	f, err := openStageOutSource(root, rel)
	if err != nil {
		t.Fatalf("openStageOutSource on an ordinary file: %v", err)
	}
	defer f.Close()

	// The swap, in the window between validation and the read. NTFS frees the
	// name immediately because os.Root opens with FILE_SHARE_DELETE, so this
	// works with the handle still open.
	if err := os.Rename(src, src+".moved"); err != nil {
		t.Fatalf("swap step 1 (rename away): %v", err)
	}
	if err := os.WriteFile(src, []byte("attacker-bytes"), 0o600); err != nil {
		t.Fatalf("swap step 2 (plant attacker file): %v", err)
	}
	if b, err := os.ReadFile(src); err != nil || string(b) != "attacker-bytes" {
		t.Fatalf("the swap did not take (%q, %v); this test would prove nothing", b, err)
	}

	dest := filepath.Join(t.TempDir(), "render.exr")
	if err := copyFromFile(f, dest, 0o600); err != nil {
		t.Fatalf("copyFromFile: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != "legit-task-output" {
		t.Errorf("dest = %q, want %q: the copy followed the path and read the "+
			"post-swap file instead of the validated descriptor", got, "legit-task-output")
	}
}

// TestStageOut_OrdinaryFileUnaffected is the default-configuration test for
// stage-out: an ordinary staged output copies back byte for byte with the
// built-in copy, which is what a worker with no staging configuration uses.
func TestStageOut_OrdinaryFileUnaffected(t *testing.T) {
	scratch := t.TempDir()
	s := New(scratch, builtinSentinel, false, discardLogger())

	outOrig := filepath.Join(t.TempDir(), "render.exr")
	entries := []protocol.StageEntry{
		{Path: outOrig, Direction: "OUT", ObjectType: "FILE"},
	}
	rules, scratchDir, err := s.StageIn(context.Background(), "job1", "att1", entries, nil)
	if err != nil {
		t.Fatalf("StageIn: %v", err)
	}
	if err := os.WriteFile(rules[0].DestinationPath, []byte("rendered"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.StageOut(context.Background(), scratchDir, entries); err != nil {
		t.Fatalf("StageOut on an ordinary output: %v", err)
	}
	if b, err := os.ReadFile(outOrig); err != nil || string(b) != "rendered" {
		t.Fatalf("copied-out = %q err=%v, want %q", b, err, "rendered")
	}
}

// TestStageOut_SharingViolationIsNotReportedAsEscape covers the access-error
// branch: a stage-out source the daemon cannot open right now must not be
// reported as a containment breach.
//
// The process tree is not killed when a task exits successfully (see
// proctree.Tree.Release), so a background child that outlives its task can
// still hold the staged output open with a restrictive share mode; NTFS then
// answers the daemon's open with ERROR_SHARING_VIOLATION. Go maps that to
// neither fs.ErrPermission nor fs.ErrNotExist, so the classifier names it
// explicitly. Stage-out fails closed either way; only the reported cause
// differs.
func TestStageOut_SharingViolationIsNotReportedAsEscape(t *testing.T) {
	scratch := t.TempDir()
	stagedDir := filepath.Join(scratch, "0")
	if err := os.MkdirAll(stagedDir, 0o750); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(stagedDir, "render.exr")
	if err := os.WriteFile(staged, []byte("half-written-output"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Stand in for the surviving child: hold the file open sharing nothing.
	name, err := windows.UTF16PtrFromString(staged)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ, 0 /* dwShareMode: deny all */, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Skipf("cannot take an exclusive handle on this filesystem: %v", err)
	}
	defer windows.CloseHandle(h) //nolint:errcheck // test teardown; a failure here cannot affect the assertion

	root, err := os.OpenRoot(scratch)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	// The deny-all handle above is a precondition that some hosts do not
	// honor: there, CreateFile(dwShareMode: 0) returns a handle and a second
	// open of the same path still succeeds, through os.Root and through a plain
	// os.Open alike. No sharing violation can be provoked on such a host, so
	// the test skips rather than report a missing OS behavior as a defect.
	//
	// The probe is a plain open, which shares no code with openStageOutSource.
	// TestClassifyStageOutOpenError_SharingViolationIsUnreadable below proves
	// the classification on every host; this test adds the end-to-end check
	// where the OS makes it possible.
	if probe, probeErr := os.Open(staged); probeErr == nil {
		probe.Close()
		t.Skip("this host does not enforce the deny-all share mode: a plain open of the " +
			"exclusively-held file succeeded, so no sharing violation can be provoked here")
	}

	f, err := openStageOutSource(root, filepath.Join("0", "render.exr"))
	if err == nil {
		f.Close()
		t.Fatal("want an error: the source cannot be opened while another handle denies sharing")
	}
	if !errors.Is(err, errStageOutUnreadable) {
		t.Errorf("err = %v, want errStageOutUnreadable (an access failure, not an attack)", err)
	}
	if errors.Is(err, errStageOutEscape) {
		t.Errorf("err = %v, want it not classified as a containment breach", err)
	}
	if strings.Contains(err.Error(), "outside scratch") {
		t.Errorf("err = %v, want the operator-facing message not to allege an escape", err)
	}
}

// TestCopyFile_RefusesHardlinkedStageInSourceOnWindows pins the Windows
// stage-in behavior.
//
// hasExtraHardlinks applies the link-count refusal to both its callers:
// openStageOutSource, and copyFile, which is the built-in stage-in copy. So a
// job input asset that carries a second NTFS hardlink is refused on Windows,
// as on POSIX. Content-addressed and dedup asset stores, and
// "rsync --link-dest"-style delivery, produce multiply linked files routinely.
//
// This differs from TestCopyFile_RefusesSourceWithExtraHardlink in
// staging_copy_test.go, which frames the same check as a TOCTOU defense and
// calls copyFile directly. This test drives builtinCopy, the stage-in entry
// point Stager.transfer reaches, with a legitimate input.
func TestCopyFile_RefusesHardlinkedStageInSourceOnWindows(t *testing.T) {
	dir := t.TempDir()

	// The asset store's object, and the delivered job input that shares its
	// inode. This is what `rsync --link-dest` or a content-addressed store
	// produces on an ordinary delivery.
	object := filepath.Join(dir, "cas-object")
	if err := os.WriteFile(object, []byte("input-asset-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "shot.ma")
	if err := os.Link(object, src); err != nil {
		t.Skipf("hardlinks unsupported in this temp dir (not NTFS?): %v", err)
	}

	dest := filepath.Join(t.TempDir(), "shot.ma")
	err := builtinCopy(context.Background(), src, dest)
	if err == nil {
		t.Fatal("want stage-in to refuse an input carrying a second hardlink on Windows " +
			"(hasExtraHardlinks must be a real check here, not a stub)")
	}
	if !strings.Contains(err.Error(), "hardlink") {
		t.Errorf("err = %v, want the operator-facing message to name the hardlink refusal", err)
	}
	if strings.Contains(err.Error(), "outside scratch") {
		t.Errorf("err = %v, want the hardlink refusal not to borrow the containment refusal's wording", err)
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Error("dest must not exist: stage-in must refuse before copying any bytes")
	}
}

// TestClassifyStageOutOpenError_SharingViolationIsUnreadable: an
// ERROR_SHARING_VIOLATION is an access failure and is never reported as a
// containment breach.
//
// It feeds the classifier the errno the kernel produces instead of provoking
// one, so it holds on every Windows host, including those that do not enforce
// a deny-all share mode. The distinction is operator-facing: the escape
// wording says a task tried to break out of its scratch directory, and a
// background child still holding a render open is not that.
func TestClassifyStageOutOpenError_SharingViolationIsUnreadable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		errno syscall.Errno
	}{
		{"sharing violation", windows.ERROR_SHARING_VIOLATION},
		{"lock violation", windows.ERROR_LOCK_VIOLATION},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Shaped as the open really returns it: a *fs.PathError wrapping the errno.
			err := classifyStageOutOpenError("0/render.exr", "C:/scratch",
				&fs.PathError{Op: "openat", Path: "0/render.exr", Err: tc.errno})

			if !errors.Is(err, errStageOutUnreadable) {
				t.Errorf("err = %v, want errStageOutUnreadable (an access failure, not an attack)", err)
			}
			if errors.Is(err, errStageOutEscape) {
				t.Errorf("err = %v, want it not classified as a containment breach", err)
			}
			if !errors.Is(err, tc.errno) {
				t.Errorf("err = %v, want the underlying errno still unwrappable", err)
			}
		})
	}
}
