// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package staging

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/uberware/sqi/internal/worker/protocol"
)

// mklinkJunction creates a directory junction at link pointing at target.
//
// A junction (IO_REPARSE_TAG_MOUNT_POINT) is the whole reason this file
// exists: unlike an NTFS symlink, creating one requires NO privilege at all —
// not SeCreateSymbolicLinkPrivilege, not Developer Mode. It is therefore the
// primitive an ordinary run-as-user task actually has, and the one every
// guard in staging.go historically missed, because os.Lstat reports a
// junction as fs.ModeIrregular rather than fs.ModeSymlink and
// filepath.EvalSymlinks does not resolve it.
func mklinkJunction(t *testing.T, link, target string) {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Fatalf("mklink /J %q %q: %v: %s", link, target, err, out)
	}
}

// There is deliberately NO Windows test asserting that the copy layer itself
// refuses a reparse-point source, and a reader must not assume one exists.
// Since H3 the stage-out copy layer takes an already-open, already-validated
// descriptor (copyFromFile), so there is no path for a junction to be planted
// at — the refusal lives entirely in openStageOutSource, which every test
// below drives. copyFile, the by-path opener, is the STAGE-IN path only; its
// O_NOFOLLOW guard is a no-op on Windows (noFollowFlag is 0 there), and the
// two primitives that would exercise it are unavailable: a junction is
// directory-only, so opening one by path fails ERROR_ACCESS_DENIED rather
// than exercising any reparse check, and an NTFS FILE symlink needs
// SeCreateSymbolicLinkPrivilege, which an ordinary task does not hold and
// this suite therefore never creates. A test named for the copy layer that
// actually called openStageOutSource used to live here; it was a near-exact
// duplicate of TestStageOut_RefusesJunctionedScratchSubdir wearing a name
// that claimed coverage it did not provide, and was removed rather than
// renamed.

// TestStageOut_RefusesJunctionedScratchSubdir is the primary H3 regression.
//
// A task owns its per-entry scratch subdirectory (StageIn's ChownRecursive
// hands it over, and on Windows isolation genuinely ACL-secures it to the
// target account). So it can delete that subdirectory and replace it with a
// junction pointing anywhere on the volume. Stage-out then reads
// <scratchDir>/<i>/<basename> — which now resolves through the junction —
// and the ELEVATED daemon copies those bytes to the job's real output path.
//
// This is not a race and needs no privilege: it is one-shot and
// deterministic. Before the fix this test fails by finding the secret's
// contents at outOrig.
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
	// the secret directory. Asserting mklink SUCCEEDED matters: a test that
	// passes because the attack setup failed proves nothing.
	stagedDir := filepath.Join(scratchDir, "0")
	if err := os.RemoveAll(stagedDir); err != nil {
		t.Fatal(err)
	}
	mklinkJunction(t, stagedDir, secretDir)
	if _, err := os.Stat(filepath.Join(stagedDir, "render.exr")); err != nil {
		t.Fatalf("junction is not live, so this test would prove nothing: %v", err)
	}

	err = s.StageOut(context.Background(), scratchDir, entries)
	if err == nil {
		t.Fatal("want error refusing a stage-out source reached through a junction")
	}
	// Assert the SPECIFIC branch, not just that the word "junction" appears
	// somewhere. The junction here is an INTERMEDIATE component of rel, so
	// root.Lstat fails on it and the advisory block is skipped entirely — the
	// refusal comes from the kernel-enforced open. Both messages mention a
	// junction, so a substring assertion would pass either way and could not
	// see the two branches being confused for one another.
	if !errors.Is(err, errStageOutEscape) {
		t.Errorf("err = %v, want errStageOutEscape (the kernel-enforced open refused the lookup)", err)
	}
	if errors.Is(err, errStageOutReparse) {
		t.Errorf("err = %v, want the OPEN to have refused this, not the advisory Lstat", err)
	}
	if !strings.Contains(err.Error(), "outside scratch") {
		t.Errorf("err = %v, want the operator-facing message to name the scratch boundary", err)
	}
	if _, statErr := os.Stat(outOrig); statErr == nil {
		t.Fatal("outOrig exists: the secret's contents reached the job's real output path")
	}
}

// TestStageOut_RefusesJunctionAtFinalComponent covers the ADVISORY Lstat
// branch, which no other test reaches.
//
// TestStageOut_RefusesJunctionedScratchSubdir above plants its junction at an
// intermediate component, so root.Lstat itself errors and the advisory block
// is skipped — every junction refusal in this package actually came from the
// open. Planting the junction at the FINAL component instead is what makes
// root.Lstat succeed and report fs.ModeIrregular (Go surfaces a
// IO_REPARSE_TAG_MOUNT_POINT that way, never as ModeSymlink), so the advisory
// branch is the one that answers. Without this test, deleting that whole
// block would leave the package green.
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
	mklinkJunction(t, filepath.Join(stagedDir, "render.exr"), secretDir)

	root, err := os.OpenRoot(scratch)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	rel := filepath.Join("0", "render.exr")

	// Assert the setup actually put the advisory branch in play: a test that
	// passed because Lstat failed would be testing the open again.
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
// TestStager_StageOut_RefusesHardlinkedSource, which never runs here: that
// test drives StageOut with the fakeSync fixture, a POSIX "#!/bin/sh" script
// Windows cannot execute, so it skips and Windows link-count coverage on the
// stage-out path was zero. NTFS supports hardlinks and os.Link needs no
// privilege, so the primitive itself is fully available to a task.
//
// It drives openStageOutSource directly, the way the swap test below does —
// no sync command is needed to exercise the check, which runs upstream of
// both transfer mechanisms.
func TestStageOut_RefusesHardlinkedSourceOnWindows(t *testing.T) {
	scratch := t.TempDir()
	stagedDir := filepath.Join(scratch, "0")
	if err := os.MkdirAll(stagedDir, 0o750); err != nil {
		t.Fatal(err)
	}

	// A file outside scratch, and a second NAME for it inside scratch. The
	// staged entry is an ordinary regular file with one inode and two links —
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

// TestStageOut_SwapAfterValidateIsRefused proves the stage-out TOCTOU is
// CLOSED, not merely narrowed.
//
// Nothing kills a task's process group on a SUCCESSFUL exit (see
// executor.runTask / killAndWait), so a background child that outlives its
// task still owns the scratch subdirectory and can swap the source out after
// validation and before the copy. This drives that window deterministically:
// the two halves are called in sequence with the swap performed between them.
// No sleep, no goroutine, no timing dependence — a flaky race test would get
// marked flaky and disabled, and the gap would reopen silently.
//
// The property under test is that copyFromFile reads the DESCRIPTOR
// openStageOutSource validated, so a swap at the path cannot change what is
// copied. Step 3's assertion (a fresh path read now yields the attacker's
// bytes) is what stops this test from being vacuous.
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

	// THE SWAP — the window between validation and the read. NTFS frees the
	// name immediately because os.Root opens with FILE_SHARE_DELETE, so this
	// works with our handle still open.
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
		t.Errorf("dest = %q, want %q — the copy followed the PATH and read the "+
			"post-swap file instead of the validated descriptor", got, "legit-task-output")
	}
}

// TestStageOut_OrdinaryFileUnaffected is the named default-configuration
// regression test for H3: the adversarial checks must not cost the normal
// path anything. An ordinary staged output still copies back byte for byte
// with the built-in copy, which is what a worker with no staging
// configuration uses.
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

// TestStageOut_SharingViolationIsNotReportedAsEscape covers the honest-error
// branch: a stage-out source the daemon simply cannot open right now must not
// be reported as a containment breach.
//
// This is not hypothetical on Windows. Nothing kills a task's process group on
// a SUCCESSFUL exit (see executor.processTree.release), so a background child
// that outlives its task can still hold the staged output open with a
// restrictive share mode; NTFS then answers the daemon's open with
// ERROR_SHARING_VIOLATION. Go maps that to neither fs.ErrPermission nor
// fs.ErrNotExist, so before the classification split it fell through to the
// escape wording and told the operator their task had tried to break out of
// scratch. Failing closed is right and unchanged — only the story is.
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

	f, err := openStageOutSource(root, filepath.Join("0", "render.exr"))
	if err == nil {
		f.Close()
		// Self-diagnosing on the one path that has actually gone wrong: this
		// open SUCCEEDED on a windows-latest runner while passing on every
		// developer box, and the bare "want an error" said nothing about which
		// assumption broke -- each guess then cost a full CI cycle.
		//
		// The discriminator is a PLAIN os.Open of the same path. Go's os.Root
		// reaches the file through NtCreateFile with
		// FILE_OPEN_FOR_BACKUP_INTENT (internal/syscall/windows.Openat), whose
		// effect depends on privileges the caller holds; os.Open does not use
		// it. So a plain open that FAILS while the rooted one succeeded means
		// backup intent -- a privilege the runner's token has and a developer
		// shell does not. BOTH succeeding means the deny-all share mode is not
		// enforced on that host at all, and the premise of this test rather
		// than os.Root is what does not hold there.
		plain, plainErr := os.Open(staged)
		if plainErr == nil {
			plain.Close()
		}
		t.Fatalf("want an error: the source cannot be opened while another handle denies sharing "+
			"[rooted open with FILE_OPEN_FOR_BACKUP_INTENT: succeeded] "+
			"[plain os.Open without it: %v] [process elevated: %v]",
			plainErr, windows.GetCurrentProcessToken().IsElevated())
	}
	if !errors.Is(err, errStageOutUnreadable) {
		t.Errorf("err = %v, want errStageOutUnreadable (an access failure, not an attack)", err)
	}
	if errors.Is(err, errStageOutEscape) {
		t.Errorf("err = %v, want it NOT classified as a containment breach", err)
	}
	if strings.Contains(err.Error(), "outside scratch") {
		t.Errorf("err = %v, want the operator-facing message not to allege an escape", err)
	}
}

// TestCopyFile_RefusesHardlinkedStageInSourceOnWindows pins a real BEHAVIOR
// CHANGE H3 made to Windows STAGE-IN, so that it stands on the record as a
// decision rather than surviving as an accident nobody wrote down.
//
// hasExtraHardlinks used to return (false, nil) unconditionally on Windows.
// Making it real gave the link-count refusal to BOTH its callers at once —
// openStageOutSource, which is adversarial and is the point of H3, and
// copyFile, which is the built-in stage-in copy and is not adversarial at
// all. So a job INPUT asset that happens to carry a second NTFS hardlink is
// now refused on Windows where it previously staged in fine: content-
// addressed and dedup asset stores, and "rsync --link-dest"-style delivery,
// all produce multiply linked files routinely. The refusal is deliberate,
// is POSIX parity, and is kept — but if this test ever has to change, THAT
// is the conversation to have first, not a quiet edit to the check.
//
// Deliberately distinct from TestCopyFile_RefusesSourceWithExtraHardlink in
// staging_copy_test.go, which frames the same check as a TOCTOU defense and
// calls copyFile directly. This one drives builtinCopy — the actual stage-in
// entry point Stager.transfer reaches — with a wholly legitimate input, which
// is the scenario an operator will actually hit.
func TestCopyFile_RefusesHardlinkedStageInSourceOnWindows(t *testing.T) {
	dir := t.TempDir()

	// The asset store's object, and the delivered job input that shares its
	// inode. Nothing here is an attack: this is what `rsync --link-dest` or a
	// content-addressed store produces on a normal, successful delivery.
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
			"(pre-H3 this copied: hasExtraHardlinks was a stub here)")
	}
	if !strings.Contains(err.Error(), "hardlink") {
		t.Errorf("err = %v, want the operator-facing message to name the hardlink refusal", err)
	}
	if strings.Contains(err.Error(), "outside scratch") {
		t.Errorf("err = %v, want the hardlink refusal NOT to borrow the containment refusal's wording", err)
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Error("dest must not exist: stage-in must refuse before copying any bytes")
	}
}
