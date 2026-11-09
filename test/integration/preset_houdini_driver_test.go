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
// loaded the hip file, each frame_range it rendered, how many render-event
// callbacks it registered and left registered, and the lines it wrote to file
// descriptor 1, where the worker reads openjd_* directives. Each faked render
// fires the events Houdini 21 fires for a Geometry ROP, PreFrame then PostFrame
// for every frame, at that frame's time. A real Python runs the shipped bytes;
// only Houdini itself is stubbed, since no Houdini is available to CI.
//
// It is a separate runner rather than a hou.py next to the driver because
// `python -I` puts neither the script's directory nor the current directory on
// sys.path.
const houdiniRunner = `import json, os, runpy, sys, tempfile, types

state = {"loads": 0, "renders": [], "registered": 0, "remaining": 0, "lines": []}
callbacks = []
events = types.SimpleNamespace(
    PreRender="PreRender", PreFrame="PreFrame", PostFrame="PostFrame",
    PostWrite="PostWrite", PostRender="PostRender",
)


class Rop:
    def render(self, frame_range):
        state["renders"].append(list(frame_range))
        lo, hi, step = frame_range
        for frame in range(lo, hi + 1, step):
            for event in (events.PreFrame, events.PostFrame):
                for cb in list(callbacks):
                    cb(self, event, (frame - 1) / 24.0)

    def addRenderEventCallback(self, cb):
        state["registered"] += 1
        callbacks.append(cb)

    def removeRenderEventCallback(self, cb):
        callbacks.remove(cb)


hou = types.ModuleType("hou")
hou.hipFile = types.SimpleNamespace(
    load=lambda path, ignore_load_warnings=False: state.__setitem__("loads", state["loads"] + 1)
)
hou.node = lambda path: Rop()
hou.ropRenderEventType = events
sys.modules["hou"] = hou

driver = sys.argv[1]
sys.argv = sys.argv[1:]
saved = os.dup(1)
with tempfile.TemporaryFile() as fd1:
    os.dup2(fd1.fileno(), 1)
    try:
        runpy.run_path(driver, run_name="__main__")
    finally:
        sys.stdout.flush()
        os.dup2(saved, 1)
    fd1.seek(0)
    state["lines"] = fd1.read().decode().splitlines()
state["remaining"] = len(callbacks)
print(json.dumps(state))
`

// houdiniDriverRun is the runner's JSON document.
type houdiniDriverRun struct {
	Loads      int      `json:"loads"`
	Renders    [][]int  `json:"renders"`
	Registered int      `json:"registered"`
	Remaining  int      `json:"remaining"`
	Lines      []string `json:"lines"`
}

// houdiniDriver writes the shipped driver and the runner to a temporary
// directory and returns a function that runs the driver on a chunk. It skips
// the test when no Python 3 is available.
func houdiniDriver(t *testing.T) func(t *testing.T, frames string) houdiniDriverRun {
	t.Helper()
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
	return func(t *testing.T, frames string) houdiniDriverRun {
		t.Helper()
		out, err := exec.CommandContext(t.Context(), python, "-I", runnerPath, driverPath,
			"scene.hip", "/out/mantra1", frames).CombinedOutput()
		if err != nil {
			t.Fatalf("driver failed: %v\n%s", err, out)
		}
		var got houdiniDriverRun
		if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
			t.Fatalf("runner output %q: %v", out, err)
		}
		return got
	}
}

// TestHoudiniROPRender_DriverRendersEachRange pins what the embedded driver
// asks hou to render for a task's chunk. sqi spells a non-contiguous chunk as a
// compact <IntRangeExpr> (internal/openjd/expand.go, formatNoncontiguousChunk):
// a run of three or more frames at one step is a lo-hi or lo-hi:step range,
// rendered in one call, while a shorter run is listed frame by frame, so the
// driver merges consecutive frames back into one call.
func TestHoudiniROPRender_DriverRendersEachRange(t *testing.T) {
	run := houdiniDriver(t)

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
			got := run(t, tc.frames)
			if got.Loads != 1 {
				t.Errorf("hip file loaded %d times, want once per task", got.Loads)
			}
			if !reflect.DeepEqual(got.Renders, tc.want) {
				t.Errorf("frames %q rendered %v, want %v", tc.frames, got.Renders, tc.want)
			}
		})
	}
}

// TestPresetHoudiniDriver_EmitsProgress pins the directives the driver writes
// as each frame finishes: one PostFrame callback, registered once and removed
// after the last render, writes openjd_progress and openjd_status for every
// frame of the chunk in order, without changing the render calls.
func TestPresetHoudiniDriver_EmitsProgress(t *testing.T) {
	run := houdiniDriver(t)
	got := run(t, "1-3,7,8")

	if want := [][]int{{1, 3, 1}, {7, 8, 1}}; !reflect.DeepEqual(got.Renders, want) {
		t.Errorf("rendered %v, want %v", got.Renders, want)
	}
	if got.Registered != 1 {
		t.Errorf("registered %d render-event callbacks, want 1", got.Registered)
	}
	if got.Remaining != 0 {
		t.Errorf("%d render-event callbacks still registered after the last render, want 0", got.Remaining)
	}
	want := []string{
		"openjd_progress: 20", "openjd_status: rendered frame 1 (1 of 5)",
		"openjd_progress: 40", "openjd_status: rendered frame 2 (2 of 5)",
		"openjd_progress: 60", "openjd_status: rendered frame 3 (3 of 5)",
		"openjd_progress: 80", "openjd_status: rendered frame 7 (4 of 5)",
		"openjd_progress: 100", "openjd_status: rendered frame 8 (5 of 5)",
	}
	if !reflect.DeepEqual(got.Lines, want) {
		t.Errorf("file descriptor 1 lines =\n%q\nwant\n%q", got.Lines, want)
	}
}
