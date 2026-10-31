// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

// preset_vred_test.go: what the VRED presets' golden files cannot show on
// their own: that params.json is JSON a script can decode, that its cost stays
// inside the worker's EXPR limits at the largest chunk the presets allow, and
// what the embedded render.py asks VRED to do with it.
//
// Untagged, like the rest of the preset harness, so `make test-cover` runs it.
// Every test is named TestPresetVRED_* so the preset harness's -run regex
// (make test-preset-harness, and CI's preset-harness-windows job) selects it:
// params.json's escaping differs by host, and only a Windows run can show a
// backslash it failed to escape (the goldens' host-path rewrite maps both the
// single and the doubled spelling to one POSIX form, so they cannot).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/presettest"
)

// vredPresets are the two VRED presets, which share one render.py and one
// params.json shape (the viewpoint one adds "view").
var vredPresets = []string{"vred-render", "vred-viewpoint-render"}

// vredMaxFramesPerTask is the FramesPerTask maxValue both presets declare and
// their readmes state.
const vredMaxFramesPerTask = 1000

// vredParams is params.json as the script reads it.
type vredParams struct {
	Scene  string `json:"scene"`
	Frames []int  `json:"frames"`
	Output string `json:"output"`
	View   string `json:"view"`
}

// taskFile returns the resolved body of the task's embedded file called name.
func taskFile(t *testing.T, task presettest.TaskSnapshot, name string) string {
	t.Helper()
	for _, f := range task.Files {
		if f.Name == name {
			return f.Data
		}
	}
	t.Fatalf("task %q has no embedded file %q", task.Name, name)
	return ""
}

// TestPresetVRED_ParamsCostAtMaxChunk pins params.json's cost where it is
// highest. It reads Task.Param.Frame, so it is evaluated at phase 3 on the
// worker, against the worker's EXPR limits, and grows with the chunk: the
// largest chunk the presets accept (FramesPerTask = maxValue) must resolve
// under the worker defaults and list every frame. One past the maximum must be
// refused at submission, or the maximum bounds nothing.
func TestPresetVRED_ParamsCostAtMaxChunk(t *testing.T) {
	for _, preset := range vredPresets {
		t.Run(preset, func(t *testing.T) {
			params := map[string]string{
				"SceneFile":     "/mnt/show/cars/hero_car_v012.vpb",
				"Frames":        "1-1000",
				"FramesPerTask": "1000",
				"OutputFile":    "/mnt/renders/hero_car/beauty.png",
				"View":          "Front",
			}
			if preset == "vred-render" {
				delete(params, "View")
			}
			snap, err := capturePreset(t, preset, params)
			if err != nil {
				t.Fatalf("a %d-frame chunk must resolve under the worker's default EXPR limits: %v", vredMaxFramesPerTask, err)
			}
			if len(snap.Steps) != 1 || len(snap.Steps[0].Tasks) != 1 {
				t.Fatalf("1-1000 at %d frames per task must be one task, got %+v", vredMaxFramesPerTask, snap.Steps)
			}
			p := decodeStrict[vredParams](t, taskFile(t, snap.Steps[0].Tasks[0], "params"))
			if len(p.Frames) != vredMaxFramesPerTask || p.Frames[0] != 1 || p.Frames[len(p.Frames)-1] != 1000 {
				t.Errorf("params.json frames: %d entries from %v, want the %d frames 1 to 1000",
					len(p.Frames), p.Frames[:min(3, len(p.Frames))], vredMaxFramesPerTask)
			}

			params["FramesPerTask"] = "1001"
			// The refusal must be the maxValue bound itself, not some other
			// failure a larger chunk happened to provoke (an EXPR limit, say).
			_, err = capturePreset(t, preset, params)
			wantRefusal := fmt.Sprintf("/parameterValues/FramesPerTask: value 1001 is above maximum %d", vredMaxFramesPerTask)
			if err == nil || !strings.Contains(err.Error(), wantRefusal) {
				t.Errorf("FramesPerTask 1001: err = %v, want the parameter binding refusal %q", err, wantRefusal)
			}
		})
	}
}

// TestPresetVRED_ParamsJSONRoundTrips runs each preset's `quoting` fixture case
// (a single quote, a double quote and spaces in Output File's name, and in
// vred-viewpoint-render also a backslash in View) and decodes params.json with
// encoding/json: every value must come back exactly as given. A path comes
// back in the worker's own form, since under EXPR phase 3 renders PATH values
// host-native, so on Windows the JSON also carries escaped backslashes.
func TestPresetVRED_ParamsJSONRoundTrips(t *testing.T) {
	for _, preset := range vredPresets {
		t.Run(preset, func(t *testing.T) {
			cases, err := presettest.LoadCases(filepath.Join(presetCaseDir, preset+".yaml"))
			if err != nil {
				t.Fatal(err)
			}
			var params map[string]string
			for _, c := range cases {
				if c.Name == "quoting" {
					params = c.Params
				}
			}
			if params == nil {
				t.Fatalf("%s.yaml has no quoting case", preset)
			}
			if !strings.ContainsAny(params["OutputFile"], `'"`) || !strings.Contains(params["OutputFile"], " ") {
				t.Fatalf("quoting case's OutputFile %q needs both quotes and a space", params["OutputFile"])
			}
			snap, err := capturePreset(t, preset, params)
			if err != nil {
				t.Fatalf("Capture: %v", err)
			}
			for _, task := range snap.Steps[0].Tasks {
				p := decodeStrict[vredParams](t, taskFile(t, task, "params"))
				if want := filepath.FromSlash(params["SceneFile"]); p.Scene != want {
					t.Errorf("scene = %q, want %q", p.Scene, want)
				}
				if want := filepath.FromSlash(params["OutputFile"]); p.Output != want {
					t.Errorf("output = %q, want %q", p.Output, want)
				}
				if p.View != params["View"] {
					t.Errorf("view = %q, want %q", p.View, params["View"])
				}
				if len(p.Frames) == 0 {
					t.Error("frames is empty")
				}
			}
		})
	}
}

// TestPresetVRED_ShareOneScript keeps the two presets' render.py byte for byte
// the same, so a fix to one cannot miss the other. The script reads "view"
// only when params.json carries it.
func TestPresetVRED_ShareOneScript(t *testing.T) {
	assertPresetsShareFile(t, vredPresets, "render")
}

// vredRunner stands in for VRED: it installs recording fakes of the API v1
// functions render.py calls, then runs the preset's own -postpython code
// exactly as VRED would, and writes every call it saw to a JSON file.
//
// argv: mode, code, record path, and the name of a fake that should raise
// ("" for none). mode "modules" puts the fakes in importable vrRenderSettings
// and vrController modules and EXECs the code; mode "globals" makes them
// globals of the namespace the code runs in, as VRED's Python does, and EVALs
// it, so both of the script's ways of finding the API, and both ways VRED
// might run -postpython, are exercised. Markers go to file descriptor 1, which
// is this process's stdout.
const vredRunner = `import json, sys, types

mode, code, record, fail = sys.argv[1:5]
calls = []


def fake(name):
    def call(*args):
        calls.append([name] + list(args))
        if name == fail:
            raise RuntimeError("the fake " + name + " failed")
    return call


API = {
    "vrRenderSettings": [
        "setRenderAnimation", "setRenderUseClipRange", "setRenderStartFrame",
        "setRenderStopFrame", "setRenderFrameStep", "setRenderView",
        "setRenderFilename", "startRenderToFile",
    ],
    "vrController": ["terminateVred", "crashVred"],
}
for module, names in API.items():
    if mode == "modules":
        m = types.ModuleType(module)
        for n in names:
            setattr(m, n, fake(n))
        sys.modules[module] = m
    else:
        for n in names:
            globals()[n] = fake(n)

if mode == "modules":
    exec(code)
else:
    eval(code)

with open(record, "w", encoding="utf-8") as f:
    json.dump(calls, f)
`

// scriptLiteral matches the quoted script path the -postpython code ends with.
var scriptLiteral = regexp.MustCompile(`\('[^']*'\)$`)

// runVREDScript writes the task's resolved render.py and params.json (or
// paramsOverride) into a fresh directory, points the task's own -postpython
// code at that render.py, and runs it under vredRunner. It returns the calls
// the fakes recorded, as canonical JSON, and what the script wrote to stdout.
func runVREDScript(t *testing.T, python string, task presettest.TaskSnapshot, mode, fail, paramsOverride string) (calls, stdout string) {
	t.Helper()
	dir := t.TempDir()
	params := taskFile(t, task, "params")
	if paramsOverride != "" {
		params = paramsOverride
	}
	script := filepath.Join(dir, "render.py")
	runner := filepath.Join(dir, "runner.py")
	record := filepath.Join(dir, "calls.json")
	for path, data := range map[string]string{
		script: taskFile(t, task, "render"), filepath.Join(dir, "params.json"): params, runner: vredRunner,
	} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(script, "'") {
		t.Skipf("temp directory %q holds a quote; it cannot be written as the script literal", script)
	}
	code := task.Args[len(task.Args)-1]
	if !scriptLiteral.MatchString(code) {
		t.Fatalf("-postpython code %q does not end with a quoted script path", code)
	}
	code = scriptLiteral.ReplaceAllLiteralString(code, "('"+strings.ReplaceAll(script, `\`, `\\`)+"')")

	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(t.Context(), python, "-I", runner, mode, code, record, fail)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("runner failed: %v\nstdout:\n%s\nstderr:\n%s", err, out.String(), errOut.String())
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("runner recorded nothing: %v\nstderr:\n%s", err, errOut.String())
	}
	return canonicalJSON(t, data), out.String()
}

// canonicalJSON re-marshals data so two encoders' spacing cannot differ.
func canonicalJSON(t *testing.T, data []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, data)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestPresetVRED_ScriptDrivesVRED runs the shipped render.py, as resolved for a
// real task, under a real Python with VRED faked, and pins the calls it makes:
// the task's first and last frame with a frame step of 1, the view and the
// render filename only when given, the success line and terminateVred after a
// render, and on any error the failure line, openjd_fail and crashVred(1)
// with no success line.
func TestPresetVRED_ScriptDrivesVRED(t *testing.T) {
	python := findPython3(t)
	if python == "" {
		t.Skip("no working Python 3 on PATH (tried python3, python)")
	}
	const scene = "/mnt/show/cars/hero_car_v012.vpb"
	out := `/mnt/renders/hero car/it's the "hero" shot.png`
	view := `Cam 'A' "wide" \ left`
	head := func(start, stop int) []any {
		return []any{
			[]any{"setRenderAnimation", true},
			[]any{"setRenderUseClipRange", false},
			[]any{"setRenderStartFrame", start},
			[]any{"setRenderStopFrame", stop},
			[]any{"setRenderFrameStep", 1},
		}
	}
	concat := func(parts ...[]any) []any {
		var all []any
		for _, p := range parts {
			all = append(all, p...)
		}
		return all
	}
	tests := []struct {
		name     string
		preset   string
		params   map[string]string
		fail     string
		override string
		want     []any
		wantOK   bool
		wantFail string
	}{
		{
			name: "chunk keeping the scene output", preset: "vred-render",
			params: map[string]string{"SceneFile": scene, "Frames": "1-10"},
			want:   concat(head(1, 10), []any{[]any{"startRenderToFile", true}, []any{"terminateVred"}}),
			wantOK: true,
		},
		{
			name: "output file with quotes and spaces", preset: "vred-render",
			params: map[string]string{"SceneFile": scene, "Frames": "1-2", "OutputFile": out},
			want: concat(head(1, 2), []any{
				[]any{"setRenderFilename", filepath.FromSlash(out)},
				[]any{"startRenderToFile", true},
				[]any{"terminateVred"},
			}),
			wantOK: true,
		},
		{
			name: "view with quotes and a backslash at negative frames", preset: "vred-viewpoint-render",
			params: map[string]string{"SceneFile": scene, "Frames": "-5--1", "View": view},
			want: concat(head(-5, -1), []any{
				[]any{"setRenderView", view},
				[]any{"startRenderToFile", true},
				[]any{"terminateVred"},
			}),
			wantOK: true,
		},
		{
			name: "render fails", preset: "vred-render",
			params: map[string]string{"SceneFile": scene, "Frames": "3-4"},
			fail:   "startRenderToFile",
			want: concat(head(3, 4), []any{
				[]any{"startRenderToFile", true}, []any{"crashVred", 1},
			}),
			wantFail: "RuntimeError: the fake startRenderToFile failed",
		},
		{
			name: "frames with a gap are refused", preset: "vred-render",
			params:   map[string]string{"SceneFile": scene, "Frames": "1"},
			override: `{"scene": "x.vpb", "frames": [1, 3], "output": ""}`,
			want:     []any{[]any{"crashVred", 1}},
			wantFail: "ValueError: the task's frames are not consecutive: [1, 3]",
		},
	}
	for _, tc := range tests {
		snap, err := capturePreset(t, tc.preset, tc.params)
		if err != nil {
			t.Fatalf("%s: Capture: %v", tc.name, err)
		}
		task := snap.Steps[0].Tasks[0]
		wantJSON, err := json.Marshal(tc.want)
		if err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"modules", "globals"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				calls, stdout := runVREDScript(t, python, task, mode, tc.fail, tc.override)
				if calls != string(wantJSON) {
					t.Errorf("VRED calls:\n  got  %s\n  want %s", calls, wantJSON)
				}
				gotOK := strings.Contains(stdout, "SQI_RENDER: OK\n")
				if gotOK != tc.wantOK {
					t.Errorf("success line printed = %v, want %v; stdout:\n%s", gotOK, tc.wantOK, stdout)
				}
				if tc.wantFail == "" {
					if strings.Contains(stdout, "SQI_RENDER: FAILED:") {
						t.Errorf("failure line printed on success; stdout:\n%s", stdout)
					}
					return
				}
				for _, line := range []string{"SQI_RENDER: FAILED: " + tc.wantFail + "\n", "openjd_fail: " + tc.wantFail + "\n"} {
					if !strings.Contains(stdout, line) {
						t.Errorf("stdout lacks %q; stdout:\n%s", line, stdout)
					}
				}
			})
		}
	}
}

// TestPresetTier3_FoldsAScriptPathQuotedInsideAnArgument pins the Tier 3
// cross-check's handling of a host-script argument: the session directory
// inside a quoted script path differs between the computed and the observed
// run, so it must fold to the same placeholder in both POSIX and Windows
// spellings, while an unrelated quoted path is left alone.
func TestPresetTier3_FoldsAScriptPathQuotedInsideAnArgument(t *testing.T) {
	files := map[string]bool{"render.py": true}
	const want = "(lambda p: run(p))('<FILE:render.py>')"
	for _, in := range []string{
		"(lambda p: run(p))('/tmp/presettest-sessions-1/sessions/abc/render.py')",
		"(lambda p: run(p))('/var/lib/sqi/sessions/def/render.py')",
		`(lambda p: run(p))('C:\\Users\\robin\\AppData\\Local\\Temp\\sessions\\abc\\render.py')`,
	} {
		if got := foldFilePaths([]string{in}, files)[0]; got != want {
			t.Errorf("fold(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"('/mnt/show/render.pyc')", "('relative/render.py')", "/mnt/show/scene.vpb"} {
		if got := foldFilePaths([]string{in}, files)[0]; got != in {
			t.Errorf("fold(%q) = %q, want it unchanged", in, got)
		}
	}
}
