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
	s := staging.New(scratch, fakeSync(t), discard())

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
	s := staging.New(scratch, "false {src} {dest}", discard()) // `false` exits non-zero
	_, _, err := s.StageIn(context.Background(), "job1", "att1", []protocol.StageEntry{
		{Path: "/nope", Direction: "IN"},
	})
	if err == nil {
		t.Fatal("want error from failing sync command")
	}
}

func TestStager_StageOut_CopiesBack(t *testing.T) {
	scratch := t.TempDir()
	s := staging.New(scratch, fakeSync(t), discard())
	// Create a staged scratch dir with an output file.
	scratchDir := filepath.Join(scratch, "job1", "att1")
	if err := os.MkdirAll(scratchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The output's original path (dest of copy-back) is a fresh temp file path.
	outOrig := filepath.Join(t.TempDir(), "render.exr")
	staged := filepath.Join(scratchDir, "render.exr")
	if err := os.WriteFile(staged, []byte("pixels"), 0o644); err != nil {
		t.Fatal(err)
	}
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

func TestStager_Configured(t *testing.T) {
	if staging.New("", "", discard()).Configured() {
		t.Error("empty config should not be Configured")
	}
	if !staging.New("/s", "rsync {src} {dest}", discard()).Configured() {
		t.Error("full config should be Configured")
	}
}
