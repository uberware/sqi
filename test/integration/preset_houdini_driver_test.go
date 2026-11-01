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

// TestHoudiniROPRender_DriverRendersEachRange pins what the embedded driver
// asks hou to render for a task's chunk. sqi spells a NONCONTIGUOUS chunk as a
// compact <IntRangeExpr> (internal/openjd/expand.go, formatNoncontiguousChunk):
// a run of three or more frames at one step is a lo-hi or lo-hi:step range,
// rendered in one call, while a shorter run is listed frame by frame, so the
// driver merges consecutive frames back into one call.
func TestHoudiniROPRender_DriverRendersEachRange(t *testing.T) {
	python := findPython3(t)
	if python == "" {
		t.Skip("no working Python 3 on PATH (tried python3, python)")
	}

	tmpl, err := presettest.PresetTemplate(presettest.SourcePresets, "houdini-rop-render")
	if err != nil {
		t.Fatalf("PresetTemplate: %v", err)
	}
	driver := embeddedFileData(t, tmpl, "render_chunk")

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
		{"range", "1-10", [][]int{{1, 10, 1}}},
		{"stepped range", "1-19:2", [][]int{{1, 19, 2}}},
		{"range then a pair", "1-3,7,8", [][]int{{1, 3, 1}, {7, 8, 1}}},
		{"pairs", "1,2,4,5", [][]int{{1, 2, 1}, {4, 5, 1}}},
		{"stepped pair", "7,9", [][]int{{7, 7, 1}, {9, 9, 1}}},
		{"range then a continuing frame", "1-3,4", [][]int{{1, 4, 1}}},
		{"single frame", "7", [][]int{{7, 7, 1}}},
		{"order given is kept", "10-14:2,1", [][]int{{10, 14, 2}, {1, 1, 1}}},
		// Negative frames (simulation pre-roll) in every shape sqi emits.
		{"range across zero", "-5-5", [][]int{{-5, 5, 1}}},
		{"negative range", "-3--1", [][]int{{-3, -1, 1}}},
		{"negative stepped range", "-9--1:2", [][]int{{-9, -1, 2}}},
		{"negative stepped pair", "-10,-8", [][]int{{-10, -10, 1}, {-8, -8, 1}}},
		{"negative pair", "-2,-1", [][]int{{-2, -1, 1}}},
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
