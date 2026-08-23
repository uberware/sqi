// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package staging

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
	if !strings.Contains(err.Error(), "junction") && !strings.Contains(err.Error(), "outside scratch") {
		t.Errorf("err = %v, want it to name the junction or the scratch boundary", err)
	}
	if _, statErr := os.Stat(outOrig); statErr == nil {
		t.Fatal("outOrig exists: the secret's contents reached the job's real output path")
	}
}
