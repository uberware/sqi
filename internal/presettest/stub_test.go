// SPDX-License-Identifier: AGPL-3.0-or-later

package presettest_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/uberware/sqi/internal/presettest"
)

func TestCommandNames_DeduplicatesAcrossTasksAndSteps(t *testing.T) {
	snap := presettest.Snapshot{Steps: []presettest.StepSnapshot{
		{Name: "Transcode", Tasks: []presettest.TaskSnapshot{
			{Command: "ffmpeg"}, {Command: "ffmpeg"},
		}},
		{Name: "Join", Tasks: []presettest.TaskSnapshot{
			{Command: "/abs/path/to/pwsh"},
		}},
	}}
	got := presettest.CommandNames(snap)
	if len(got) != 2 || got[0] != "ffmpeg" || got[1] != "pwsh" {
		t.Errorf("CommandNames = %v, want [ffmpeg pwsh] (basenames, deduplicated, sorted)", got)
	}
}

func TestInstallStub_PlacesAnExecutableUnderEachName(t *testing.T) {
	bin, err := presettest.BuildStub(t.Context())
	if err != nil {
		t.Skipf("BuildStub: %v (no Go toolchain?)", err)
	}
	dir := t.TempDir()
	if err := presettest.InstallStub(dir, []string{"Render", "ffmpeg"}); err != nil {
		t.Fatalf("InstallStub: %v", err)
	}
	for _, name := range []string{"Render", "ffmpeg"} {
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is not executable (mode %v)", name, info.Mode())
		}
	}
	if bin == "" {
		t.Error("BuildStub returned an empty path")
	}
}
