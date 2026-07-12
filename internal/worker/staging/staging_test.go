// SPDX-License-Identifier: AGPL-3.0-or-later

package staging_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/staging"
)

// writeFile is a test helper that creates a file with content, failing the test
// on error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

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
	})
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

func TestStager_StageIn_FailsWhenSyncFails(t *testing.T) {
	scratch := t.TempDir()
	s := staging.New(scratch, "false {src} {dest}", false, discard()) // `false` exits non-zero
	_, _, err := s.StageIn(context.Background(), "job1", "att1", []protocol.StageEntry{
		{Path: "/nope", Direction: "IN"},
	})
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
	})
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
	})
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

	_, scratchDir, err := s.StageIn(context.Background(), "job1", "att1", entries)
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
	rules, scratchDir, err := s.StageIn(context.Background(), "job1", "att1", entries)
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
