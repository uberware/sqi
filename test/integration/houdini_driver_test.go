// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/presettest"
)

// houdiniRunner runs the shipped driver under a stub `hou` module and prints
// what the driver asked Houdini to do as one JSON document: how many times it
// loaded the hip file, and each frame_range it rendered. A real Python runs
// the shipped bytes; only Houdini itself is stubbed, since no Houdini is
// available to CI.
//
// It is a separate runner rather than a hou.py next to the driver because
// `python -I` puts neither the script's directory nor the current directory on
// sys.path.
const houdiniRunner = `import json, runpy, sys, types

state = {"loads": 0, "renders": []}


class Rop:
    def render(self, frame_range):
        state["renders"].append(list(frame_range))


hou = types.ModuleType("hou")
hou.hipFile = types.SimpleNamespace(
    load=lambda path, ignore_load_warnings=False: state.__setitem__("loads", state["loads"] + 1)
)
hou.node = lambda path: Rop()
sys.modules["hou"] = hou

driver = sys.argv[1]
sys.argv = sys.argv[1:]
runpy.run_path(driver, run_name="__main__")
print(json.dumps(state))
`

// TestHoudiniROPRender_DriverCoalescesFrameRuns pins what the embedded driver
// asks hou to render for a task's comma-separated frame list. sqi writes a
// NONCONTIGUOUS chunk as a flat list of single frames, so the driver has to
// merge each run of consecutive frames back into one render call: that keeps
// an unstepped range rendering exactly as it did before the preset moved to
// NONCONTIGUOUS (one call per chunk), while a stepped range still renders
// frame by frame. A legacy `lo-hi:step` part is still honored.
func TestHoudiniROPRender_DriverCoalescesFrameRuns(t *testing.T) {
	python := requireStubPython(t)

	tmpl, err := presettest.PresetTemplate(presettest.SourcePresets, "houdini-rop-render")
	if err != nil {
		t.Fatalf("PresetTemplate: %v", err)
	}
	parsed, err := openjd.Parse([]byte(tmpl), openjd.FormatYAML)
	if err != nil {
		t.Fatalf("openjd.Parse: %v", err)
	}
	var driver string
	for _, f := range parsed.Steps[0].Script.EmbeddedFiles {
		if f.Name == "render_chunk" {
			driver = f.Data
		}
	}
	if driver == "" {
		t.Fatal("houdini-rop-render declares no embedded file named render_chunk")
	}

	dir := t.TempDir()
	driverPath := filepath.Join(dir, "render_chunk.py")
	runnerPath := filepath.Join(dir, "runner.py")
	if err := os.WriteFile(driverPath, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runnerPath, []byte(houdiniRunner), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		frames string
		want   [][]int
	}{
		{"consecutive run", "1,2,3", [][]int{{1, 3, 1}}},
		{"no two consecutive", "1,3,5", [][]int{{1, 1, 1}, {3, 3, 1}, {5, 5, 1}}},
		{"run then run", "1,2,3,7,8", [][]int{{1, 3, 1}, {7, 8, 1}}},
		{"run then single", "1,2,3,7", [][]int{{1, 3, 1}, {7, 7, 1}}},
		{"single frame", "7", [][]int{{7, 7, 1}}},
		{"order given is kept", "7,8,1,2", [][]int{{7, 8, 1}, {1, 2, 1}}},
		{"legacy stepped part", "1-3:2", [][]int{{1, 3, 2}}},
		{"legacy range part", "4-9", [][]int{{4, 9, 1}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := exec.CommandContext(t.Context(), python, "-I", runnerPath, driverPath,
				"scene.hip", "/out/mantra1", tc.frames).CombinedOutput()
			if err != nil {
				t.Fatalf("driver failed: %v\n%s", err, out)
			}
			var got struct {
				Loads   int     `json:"loads"`
				Renders [][]int `json:"renders"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
				t.Fatalf("runner output %q: %v", out, err)
			}
			if got.Loads != 1 {
				t.Errorf("hip file loaded %d times, want once per task", got.Loads)
			}
			if !reflect.DeepEqual(got.Renders, tc.want) {
				t.Errorf("frames %q rendered %v, want %v", tc.frames, got.Renders, tc.want)
			}
		})
	}
}

// requireStubPython returns a working Python 3 or skips. A path is only
// accepted once it has run: on Windows `python3` commonly resolves to an App
// Execution Alias that opens the Store instead of running anything.
func requireStubPython(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"python3", "python"} {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		out, err := exec.CommandContext(t.Context(), path, "-c", "import sys; print(sys.version_info[0])").Output()
		if err == nil && strings.TrimSpace(string(out)) == "3" {
			return path
		}
	}
	t.Skip("no working Python 3 on PATH (tried python3, python)")
	return ""
}
