// SPDX-License-Identifier: AGPL-3.0-or-later

package staging_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/staging"
)

// These tests prove StageOut's boundary check (validateStageOutSource, run
// upstream of both the built-in copy and an operator sync_command) refuses
// every primitive a task could use to make root read or leak content outside
// its own scratch directory when its output is copied back to the real
// path: a symlinked source, a source with an extra hardlink, and a source that resolves
// outside scratch via a directory-symlink swap. An ordinary staged output
// must still copy normally — see TestStager_StageOut_CopiesBack and
// TestStager_RoundTripBuiltin elsewhere in this package for that regression
// coverage; TestStager_StageOut_OrdinaryFileStillCopies below repeats the
// minimal case here so this file documents the full boundary on its own.

// TestStager_StageOut_RefusesSymlinkSource proves a task that plants a
// symlink at its declared output path (in place of a real output file)
// cannot have root read whatever it points to and copy those bytes to the
// job's real output location — arbitrary file disclosure as root.
func TestStager_StageOut_RefusesSymlinkSource(t *testing.T) {
	scratch := t.TempDir()
	s := staging.New(scratch, fakeSync(t), false, discard())
	scratchDir := filepath.Join(scratch, "job1", "att1")
	stagedDir := filepath.Join(scratchDir, "0")
	if err := os.MkdirAll(stagedDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Something outside scratch that must never be leaked.
	secret := filepath.Join(t.TempDir(), "secret.txt")
	writeFile(t, secret, "root-only-contents")

	// The task's "output" is a symlink to that secret instead of a real file.
	if err := os.Symlink(secret, filepath.Join(stagedDir, "render.exr")); err != nil {
		t.Fatal(err)
	}

	outOrig := filepath.Join(t.TempDir(), "render.exr")
	err := s.StageOut(context.Background(), scratchDir, []protocol.StageEntry{
		{Path: outOrig, Direction: "OUT", ObjectType: "FILE"},
	})
	if err == nil {
		t.Fatal("want error refusing a symlinked stage-out source")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("err = %v, want it to mention the symlink refusal", err)
	}
	if _, statErr := os.Stat(outOrig); statErr == nil {
		t.Error("outOrig must not exist: the secret's contents must never reach it")
	}
}

// TestStager_StageOut_RefusesHardlinkedSource proves a hardlink planted
// inside scratch (which passes an Lstat regular-file check — a hardlink IS a
// regular file, sharing one inode with whatever it is linked to) is refused
// too. fs.protected_hardlinks narrows who can CREATE such a hardlink but is a
// host kernel setting sqi does not control, so this must be enforced
// independently of it.
func TestStager_StageOut_RefusesHardlinkedSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hardlink-count check is unimplemented on Windows (see staging_windows.go); " +
			"task isolation itself is not yet supported there")
	}

	scratch := t.TempDir()
	s := staging.New(scratch, fakeSync(t), false, discard())
	scratchDir := filepath.Join(scratch, "job1", "att1")
	stagedDir := filepath.Join(scratchDir, "0")
	if err := os.MkdirAll(stagedDir, 0o755); err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(t.TempDir(), "secret.txt")
	writeFile(t, outside, "root-only-contents")

	staged := filepath.Join(stagedDir, "render.exr")
	if err := os.Link(outside, staged); err != nil {
		t.Skipf("hardlinks not supported between these temp dirs: %v", err)
	}

	outOrig := filepath.Join(t.TempDir(), "render.exr")
	err := s.StageOut(context.Background(), scratchDir, []protocol.StageEntry{
		{Path: outOrig, Direction: "OUT", ObjectType: "FILE"},
	})
	if err == nil {
		t.Fatal("want error refusing a stage-out source with an extra hardlink")
	}
	if !strings.Contains(err.Error(), "hardlink") {
		t.Errorf("err = %v, want it to mention the hardlink refusal", err)
	}
	if _, statErr := os.Stat(outOrig); statErr == nil {
		t.Error("outOrig must not exist: the hardlink partner's contents must never reach it")
	}
}

// TestStager_StageOut_RefusesSourceOutsideScratch proves that even a
// perfectly ordinary regular file with a link count of 1 is refused if its
// real (symlink-resolved) location is outside scratchDir — the case where a
// task deletes one of its own per-entry scratch subdirectories (which it
// owns, per StageIn's ChownRecursive) and replaces it with a symlink to an
// arbitrary directory. Every check but containment would pass here, since
// the file itself is entirely ordinary.
func TestStager_StageOut_RefusesSourceOutsideScratch(t *testing.T) {
	scratch := t.TempDir()
	s := staging.New(scratch, fakeSync(t), false, discard())
	scratchDir := filepath.Join(scratch, "job1", "att1")
	if err := os.MkdirAll(scratchDir, 0o755); err != nil {
		t.Fatal(err)
	}

	outsideDir := t.TempDir()
	writeFile(t, filepath.Join(outsideDir, "render.exr"), "root-only-contents")

	// The task's per-index scratch subdirectory is now a symlink escaping
	// scratch entirely; the file behind it is an ordinary regular file.
	if err := os.Symlink(outsideDir, filepath.Join(scratchDir, "0")); err != nil {
		t.Fatal(err)
	}

	outOrig := filepath.Join(t.TempDir(), "render.exr")
	err := s.StageOut(context.Background(), scratchDir, []protocol.StageEntry{
		{Path: outOrig, Direction: "OUT", ObjectType: "FILE"},
	})
	if err == nil {
		t.Fatal("want error refusing a source resolving outside scratch")
	}
	if !strings.Contains(err.Error(), "outside scratch") {
		t.Errorf("err = %v, want it to mention scratch containment", err)
	}
	if _, statErr := os.Stat(outOrig); statErr == nil {
		t.Error("outOrig must not exist")
	}
}

// TestStager_StageOut_OrdinaryFileStillCopies is the negative case for the
// three refusals above: a task's genuine output — a plain regular file, one
// link, physically inside scratch — must still be copied back normally.
func TestStager_StageOut_OrdinaryFileStillCopies(t *testing.T) {
	scratch := t.TempDir()
	s := staging.New(scratch, fakeSync(t), false, discard())
	scratchDir := filepath.Join(scratch, "job1", "att1")
	stagedDir := filepath.Join(scratchDir, "0")
	if err := os.MkdirAll(stagedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(stagedDir, "render.exr"), "pixels")

	outOrig := filepath.Join(t.TempDir(), "render.exr")
	if err := s.StageOut(context.Background(), scratchDir, []protocol.StageEntry{
		{Path: outOrig, Direction: "OUT", ObjectType: "FILE"},
	}); err != nil {
		t.Fatalf("StageOut: %v", err)
	}
	got, err := os.ReadFile(outOrig)
	if err != nil || string(got) != "pixels" {
		t.Fatalf("outOrig = %q, err=%v; want %q", got, err, "pixels")
	}
}
