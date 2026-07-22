// SPDX-License-Identifier: AGPL-3.0-or-later

package staging_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/staging"
)

// testUID and testGID return a uid/gid a fake account can be given that this
// test can actually chown a scratch dir to without real privilege: normally
// the current process's own uid/gid (a permitted no-op — see
// isolation.ChownRecursive), but a fixed non-zero synthetic pair when the
// current process IS root, since isolation.CheckNotPrivileged unconditionally
// refuses a uid-0 account regardless of caller privilege (mirrors the same
// helper in internal/worker/session's own tests).
func testUID() uint32 {
	if os.Getuid() == 0 {
		return 65534
	}
	return uint32(os.Getuid())
}

func testGID() uint32 {
	if os.Getuid() == 0 {
		return 65534
	}
	return uint32(os.Getgid())
}

// writeFile is a test helper that creates a file with content, failing the test
// on error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// newTraversableTestRoot returns a directory rooted directly under /tmp,
// chmod'd 0711, for tests that need isolation's ancestor-traversability
// check to actually PASS (not just fail on a deliberately narrow directory).
// t.TempDir() cannot be used here: it is 0700 by construction, and on macOS
// $TMPDIR itself is also 0700 by OS design, so anything built on it fails
// ValidateTraversable regardless of what this test does — proving nothing
// about the behavior under test. /tmp sidesteps both layers: only the one
// directory MkdirTemp creates here needs widening; every ancestor above it
// is already traversable by design on every POSIX system. Mirrors
// internal/worker/isolation's own newTraversableTestRoot test helper (same
// fix, same reasoning, unexported so not reusable across packages).
func newTraversableTestRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sqi-staging-traversable-*") //nolint:usetesting // t.TempDir() is exactly what this must NOT use — see doc above
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o711); err != nil {
		t.Fatalf("chmod %q: %v", dir, err)
	}
	return dir
}

// fakeSync writes a shell script that records its args and copies src->dest with cp -R.
func fakeSync(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "sync.sh")
	body := "#!/bin/sh\ncp -R \"$1\" \"$2\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script + " {src} {dest}"
}

func TestStager_StageIn_CopiesAndMaps(t *testing.T) {
	// Arrange a source file.
	srcRoot := t.TempDir()
	srcFile := filepath.Join(srcRoot, "shot.ma")
	if err := os.WriteFile(srcFile, []byte("scene"), 0o644); err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	s := staging.New(scratch, fakeSync(t), false, discard())

	rules, scratchDir, err := s.StageIn(context.Background(), "job1", "att1", []protocol.StageEntry{
		{Path: srcFile, Direction: "IN", ObjectType: "FILE"},
	}, nil)
	if err != nil {
		t.Fatalf("StageIn: %v", err)
	}
	if len(rules) != 1 || rules[0].SourcePath != srcFile {
		t.Fatalf("rules = %+v", rules)
	}
	if !strings.HasPrefix(rules[0].DestinationPath, scratchDir) {
		t.Fatalf("dest %q not under scratch %q", rules[0].DestinationPath, scratchDir)
	}
	if _, err := os.Stat(rules[0].DestinationPath); err != nil {
		t.Fatalf("staged file missing: %v", err)
	}
}

// TestStager_StageIn_IsolatedFailsOnNonTraversableScratchBase is Finding 4's
// per-assignment guard for staging: cred != nil is exactly the moment this
// attempt actually carries a run-as-user identity, so StageIn validates the
// scratch base's ancestors — the IDENTICAL check (same actionable message)
// cmd/sqi-worker runs at boot only when isolation.required is set — and
// fails just THIS attempt rather than the whole worker.
func TestStager_StageIn_IsolatedFailsOnNonTraversableScratchBase(t *testing.T) {
	dir := t.TempDir()
	narrow := filepath.Join(dir, "narrow")
	if err := os.Mkdir(narrow, 0o700); err != nil {
		t.Fatalf("create narrow ancestor: %v", err)
	}
	scratchBase := filepath.Join(narrow, "scratch")

	provider := isolation.NewFake(map[string]isolation.FakeAccount{"render": {UID: testUID(), GID: testGID()}})
	cred, err := provider.Resolve(context.Background(), isolation.Spec{User: "render"})
	if err != nil {
		t.Fatalf("resolve credential: %v", err)
	}

	s := staging.New(scratchBase, fakeSync(t), false, discard())
	_, _, err = s.StageIn(context.Background(), "job1", "att1", []protocol.StageEntry{
		{Path: "/nope", Direction: "IN"},
	}, cred)

	if err == nil {
		t.Fatal("expected an error naming the non-traversable ancestor; got nil")
	}
	if !strings.Contains(err.Error(), narrow) {
		t.Errorf("err = %v, want it to name the offending ancestor %q", err, narrow)
	}
}

// TestStager_StageIn_NonIsolatedSkipsAncestorValidation proves the
// per-assignment check is scoped to isolated attempts only: the exact same
// narrow ancestor that fails an isolated StageIn above must not stop a
// nil-credential one — no other identity will ever need to chdir through it.
func TestStager_StageIn_NonIsolatedSkipsAncestorValidation(t *testing.T) {
	dir := t.TempDir()
	narrow := filepath.Join(dir, "narrow")
	if err := os.Mkdir(narrow, 0o700); err != nil {
		t.Fatalf("create narrow ancestor: %v", err)
	}
	scratchBase := filepath.Join(narrow, "scratch")

	srcRoot := t.TempDir()
	srcFile := filepath.Join(srcRoot, "shot.ma")
	writeFile(t, srcFile, "scene")

	s := staging.New(scratchBase, fakeSync(t), false, discard())
	_, _, err := s.StageIn(context.Background(), "job1", "att1", []protocol.StageEntry{
		{Path: srcFile, Direction: "IN", ObjectType: "FILE"},
	}, nil)
	if err != nil {
		t.Fatalf("StageIn (nil credential) under a narrow-but-self-owned ancestor: %v", err)
	}
}

func TestStager_StageIn_FailsWhenSyncFails(t *testing.T) {
	scratch := t.TempDir()
	s := staging.New(scratch, "false {src} {dest}", false, discard()) // `false` exits non-zero
	_, _, err := s.StageIn(context.Background(), "job1", "att1", []protocol.StageEntry{
		{Path: "/nope", Direction: "IN"},
	}, nil)
	if err == nil {
		t.Fatal("want error from failing sync command")
	}
}

func TestStager_StageOut_CopiesBack(t *testing.T) {
	scratch := t.TempDir()
	s := staging.New(scratch, fakeSync(t), false, discard())
	// Create a staged scratch dir with an output file at the index-namespaced
	// path. StageOut looks for OUT entries at <scratchDir>/<index>/<basename>;
	// the single OUT entry is at slice index 0, so the staged file lives under
	// "0/render.exr".
	scratchDir := filepath.Join(scratch, "job1", "att1")
	stagedDir := filepath.Join(scratchDir, "0")
	if err := os.MkdirAll(stagedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The output's original path (dest of copy-back) is a fresh temp file path.
	outOrig := filepath.Join(t.TempDir(), "render.exr")
	writeFile(t, filepath.Join(stagedDir, "render.exr"), "pixels")

	err := s.StageOut(context.Background(), scratchDir, []protocol.StageEntry{
		{Path: outOrig, Direction: "OUT", ObjectType: "FILE"},
	})
	if err != nil {
		t.Fatalf("StageOut: %v", err)
	}
	if _, err := os.Stat(outOrig); err != nil {
		t.Fatalf("output not copied back: %v", err)
	}
}

// TestStager_StageIn_RulesHaveFormat verifies that StageIn populates
// SourcePathFormat on every path-map rule it returns.  An empty format violates
// the OpenJD pathmapping-1.0 schema ("POSIX"|"WINDOWS" required).
func TestStager_StageIn_RulesHaveFormat(t *testing.T) {
	srcDir := t.TempDir()
	srcFile := filepath.Join(srcDir, "shot.ma")
	writeFile(t, srcFile, "scene")

	scratch := t.TempDir()
	s := staging.New(scratch, fakeSync(t), false, discard())

	rules, _, err := s.StageIn(context.Background(), "job1", "att1", []protocol.StageEntry{
		{Path: srcFile, Direction: "IN", ObjectType: "FILE"},
	}, nil)
	if err != nil {
		t.Fatalf("StageIn: %v", err)
	}
	for i, r := range rules {
		if r.SourcePathFormat == "" {
			t.Errorf("rules[%d].SourcePathFormat is empty; want POSIX or WINDOWS", i)
		}
	}
}

// TestStager_StageIn_NoBasenameCollision verifies that two IN entries with the
// same basename (e.g. /showA/scene.ma and /showB/scene.ma) are staged to
// distinct per-index paths and that both path-map rules reference the correct
// staged files.
func TestStager_StageIn_NoBasenameCollision(t *testing.T) {
	src1Dir := t.TempDir()
	src2Dir := t.TempDir()
	src1 := filepath.Join(src1Dir, "scene.ma")
	src2 := filepath.Join(src2Dir, "scene.ma")
	writeFile(t, src1, "scene-A")
	writeFile(t, src2, "scene-B")

	scratch := t.TempDir()
	s := staging.New(scratch, fakeSync(t), false, discard())

	rules, scratchDir, err := s.StageIn(context.Background(), "job1", "att1", []protocol.StageEntry{
		{Path: src1, Direction: "IN", ObjectType: "FILE"},
		{Path: src2, Direction: "IN", ObjectType: "FILE"},
	}, nil)
	if err != nil {
		t.Fatalf("StageIn: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("want 2 rules; got %d", len(rules))
	}

	// Destinations must be distinct (no collision).
	if rules[0].DestinationPath == rules[1].DestinationPath {
		t.Errorf("both entries staged to the same path: %q", rules[0].DestinationPath)
	}

	// Both staged files must exist under the scratch dir.
	for i, r := range rules {
		if !strings.HasPrefix(r.DestinationPath, scratchDir) {
			t.Errorf("rules[%d].DestinationPath %q not under scratchDir %q", i, r.DestinationPath, scratchDir)
		}
		if _, err := os.Stat(r.DestinationPath); err != nil {
			t.Errorf("rules[%d] staged file %q missing: %v", i, r.DestinationPath, err)
		}
	}

	// Source paths must map back to their originals.
	if rules[0].SourcePath != src1 {
		t.Errorf("rules[0].SourcePath = %q; want %q", rules[0].SourcePath, src1)
	}
	if rules[1].SourcePath != src2 {
		t.Errorf("rules[1].SourcePath = %q; want %q", rules[1].SourcePath, src2)
	}
}

// TestStager_StageOut_ConsistentWithStageIn verifies that StageOut reads back
// from the same per-index subdirectories that StageIn wrote, even when multiple
// INOUT entries share the same basename.  This ensures a task's modified outputs
// are copied back to the correct originals.
func TestStager_StageOut_ConsistentWithStageIn(t *testing.T) {
	src1Dir := t.TempDir()
	src2Dir := t.TempDir()
	src1 := filepath.Join(src1Dir, "output.exr")
	src2 := filepath.Join(src2Dir, "output.exr")
	writeFile(t, src1, "data-A")
	writeFile(t, src2, "data-B")

	scratch := t.TempDir()
	s := staging.New(scratch, fakeSync(t), false, discard())

	entries := []protocol.StageEntry{
		{Path: src1, Direction: "INOUT", ObjectType: "FILE"},
		{Path: src2, Direction: "INOUT", ObjectType: "FILE"},
	}

	_, scratchDir, err := s.StageIn(context.Background(), "job1", "att1", entries, nil)
	if err != nil {
		t.Fatalf("StageIn: %v", err)
	}

	// Simulate worker output: overwrite the staged files.
	writeFile(t, filepath.Join(scratchDir, "0", "output.exr"), "result-A")
	writeFile(t, filepath.Join(scratchDir, "1", "output.exr"), "result-B")

	if err := s.StageOut(context.Background(), scratchDir, entries); err != nil {
		t.Fatalf("StageOut: %v", err)
	}

	// Each original must receive the correct result from its per-index staging path.
	got1, err := os.ReadFile(src1)
	if err != nil {
		t.Fatalf("read src1: %v", err)
	}
	got2, err := os.ReadFile(src2)
	if err != nil {
		t.Fatalf("read src2: %v", err)
	}
	if string(got1) != "result-A" {
		t.Errorf("src1 = %q; want %q", got1, "result-A")
	}
	if string(got2) != "result-B" {
		t.Errorf("src2 = %q; want %q", got2, "result-B")
	}
}

// TestStager_StageIn_MapsOutputEntries verifies that OUT entries get a scratch
// destination and a path-map rule (so swap_in_place/flags/env redirect the task's
// output into scratch), and that the per-index scratch subdirectory is created so
// the task can write there — without copying any bytes in (an output has nothing
// to copy in, and its original path may not exist yet). Regression test for
// outputs being written to the real path instead of scratch, which made copy-out
// fail on a missing scratch file.
func TestStager_StageIn_MapsOutputEntries(t *testing.T) {
	inDir := t.TempDir()
	inFile := filepath.Join(inDir, "in.txt")
	writeFile(t, inFile, "input")
	// A not-yet-existing output directory: if StageIn tried to copy it in, the
	// fake `cp -R` would fail and StageIn would return an error.
	outOrig := filepath.Join(t.TempDir(), "renders")

	scratch := t.TempDir()
	s := staging.New(scratch, fakeSync(t), false, discard())

	entries := []protocol.StageEntry{
		{Path: inFile, Direction: "IN", ObjectType: "FILE"},
		{Path: outOrig, Direction: "OUT", ObjectType: "DIRECTORY"},
	}
	rules, scratchDir, err := s.StageIn(context.Background(), "job1", "att1", entries, nil)
	if err != nil {
		t.Fatalf("StageIn: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("want 2 rules (IN + OUT); got %d: %+v", len(rules), rules)
	}

	var outRule *protocol.PathMapRule
	for i := range rules {
		if rules[i].SourcePath == outOrig {
			outRule = &rules[i]
		}
	}
	if outRule == nil {
		t.Fatalf("no path-map rule for the OUT entry %q; rules=%+v", outOrig, rules)
	}
	wantDest := filepath.Join(scratchDir, "1", "renders")
	if outRule.DestinationPath != wantDest {
		t.Errorf("OUT dest = %q; want %q", outRule.DestinationPath, wantDest)
	}
	// The per-index scratch subdir must exist so the task can write its output.
	if _, err := os.Stat(filepath.Join(scratchDir, "1")); err != nil {
		t.Errorf("scratch subdir for OUT entry not created: %v", err)
	}
}

func TestStager_Configured(t *testing.T) {
	if staging.New("", "", false, discard()).Configured() {
		t.Error("empty config should not be Configured")
	}
	if !staging.New("/s", "rsync {src} {dest}", false, discard()).Configured() {
		t.Error("full config should be Configured")
	}
}

// TestStager_StageIn_AncestorModeDecidedPerWorker proves the scratch base and
// job directory's mode is a per-WORKER decision made once at construction
// ([staging.WithIsolationCapable]), not a per-assignment one keyed on this
// particular call's cred — the fix for the shared-scratch-base ordering bug:
// a worker capable of isolation must get the traversable-from-birth 0711
// regardless of which job type happens to land FIRST on a fresh scratch base
// (previously, a non-isolated attempt landing first pinned the base at 0750
// forever, failing every isolated attempt after it). A worker never marked
// capable keeps the narrower pre-isolation 0750 default even if an assignment
// somehow carries a credential anyway — it can never actually isolate, so it
// must not gain a needless traversable-by-anyone directory.
func TestStager_StageIn_AncestorModeDecidedPerWorker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits do not apply on Windows")
	}

	assertMode := func(t *testing.T, path string, want os.FileMode) {
		t.Helper()
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %q: %v", path, err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode = %04o, want %04o", path, got, want)
		}
	}

	newCred := func(t *testing.T) *isolation.Credential {
		t.Helper()
		provider := isolation.NewFake(map[string]isolation.FakeAccount{"render": {UID: testUID(), GID: testGID()}})
		cred, err := provider.Resolve(context.Background(), isolation.Spec{User: "render"})
		if err != nil {
			t.Fatalf("resolve credential: %v", err)
		}
		return cred
	}

	t.Run("capable worker: non-isolated assignment first still gets 0711", func(t *testing.T) {
		scratch := t.TempDir()
		base := filepath.Join(scratch, "base")
		s := staging.New(base, fakeSync(t), false, discard(), staging.WithIsolationCapable(true))

		srcFile := filepath.Join(t.TempDir(), "in.txt")
		writeFile(t, srcFile, "x")

		// A non-isolated (cred == nil) attempt lands FIRST on a fresh base —
		// exactly the ordering the old per-assignment logic got wrong.
		if _, _, err := s.StageIn(context.Background(), "job1", "att1", []protocol.StageEntry{
			{Path: srcFile, Direction: "IN", ObjectType: "FILE"},
		}, nil); err != nil {
			t.Fatalf("StageIn (nil credential): %v", err)
		}
		assertMode(t, base, 0o711)
		assertMode(t, filepath.Join(base, "job1"), 0o711)
	})

	t.Run("capable worker: isolated assignment first also gets 0711", func(t *testing.T) {
		scratch := newTraversableTestRoot(t)
		base := filepath.Join(scratch, "base")
		s := staging.New(base, fakeSync(t), false, discard(), staging.WithIsolationCapable(true))

		srcFile := filepath.Join(t.TempDir(), "in.txt")
		writeFile(t, srcFile, "x")

		if _, _, err := s.StageIn(context.Background(), "job1", "att1", []protocol.StageEntry{
			{Path: srcFile, Direction: "IN", ObjectType: "FILE"},
		}, newCred(t)); err != nil {
			t.Fatalf("StageIn (isolated): %v", err)
		}
		assertMode(t, base, 0o711)
		assertMode(t, filepath.Join(base, "job1"), 0o711)
	})

	t.Run("non-capable worker stays 0750 even for an isolated assignment", func(t *testing.T) {
		scratch := newTraversableTestRoot(t)
		base := filepath.Join(scratch, "base")
		// No WithIsolationCapable: zero-value / non-capable, matching a
		// worker that can never assume another identity.
		s := staging.New(base, fakeSync(t), false, discard())

		srcFile := filepath.Join(t.TempDir(), "in.txt")
		writeFile(t, srcFile, "x")

		if _, _, err := s.StageIn(context.Background(), "job1", "att1", []protocol.StageEntry{
			{Path: srcFile, Direction: "IN", ObjectType: "FILE"},
		}, newCred(t)); err != nil {
			t.Fatalf("StageIn (isolated, non-capable worker): %v", err)
		}
		assertMode(t, base, 0o750)
		assertMode(t, filepath.Join(base, "job1"), 0o750)
	})
}
