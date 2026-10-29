// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

// preset_modo_test.go — what the Modo presets' golden files cannot show on
// their own: that params.json is JSON a script can decode, that its cost stays
// inside the worker's EXPR limits at the largest chunk the presets allow, that
// the -cmd: argument names the script and its parameters file and carries no
// job value, and what the embedded render.py asks Modo to do.
//
// Untagged, like the rest of the preset harness, so `make test-cover` runs it.
// Every test is named TestPresetModo_* so the preset harness's -run regex
// (make test-preset-harness, and CI's preset-harness-windows job) selects it:
// params.json's escaping differs by host, and only a Windows run can show a
// backslash it failed to escape.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/presettest"
	"github.com/uberware/sqi/internal/store"
)

// modoPresets are the two Modo presets, which share one render.py and one
// params.json shape (the pass-group one adds "group").
var modoPresets = []string{"modo-render", "modo-pass-group-render"}

// modoMaxFramesPerTask is the FramesPerTask maxValue both presets declare and
// their readmes state.
const modoMaxFramesPerTask = 1000

// captureModo expands one Modo preset under params through the production
// pipeline (presettest.Capture, which resolves phase 3 against the WORKER's
// default EXPR limits) and returns the snapshot.
func captureModo(t *testing.T, preset string, params map[string]string) (presettest.Snapshot, error) {
	t.Helper()
	tmpl, err := presettest.PresetTemplate(presettest.SourcePresets, preset)
	if err != nil {
		t.Fatalf("PresetTemplate %s: %v", preset, err)
	}
	return presettest.Capture(t.Context(), tmpl, presettest.Options{Params: params, Format: store.TemplateFormatYAML})
}

// modoParams is params.json as the script reads it.
type modoParams struct {
	Scene  string `json:"scene"`
	Frames []int  `json:"frames"`
	Group  string `json:"group"`
}

// decodeModoParams decodes params.json strictly: an unknown key is an error,
// so a key the script does not read cannot slip in unnoticed. modo-render's
// has no "group"; the pass-group preset's has.
func decodeModoParams(t *testing.T, data string) modoParams {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(data))
	dec.DisallowUnknownFields()
	var p modoParams
	if err := dec.Decode(&p); err != nil {
		t.Fatalf("params.json is not the JSON object the script expects: %v\n%s", err, data)
	}
	return p
}

// TestPresetModo_ParamsCostAtMaxChunk pins params.json's cost where it is
// highest. It reads Task.Param.Frame, so it is evaluated at phase 3 on the
// worker, against the worker's EXPR limits, and grows with the chunk: the
// largest chunk the presets accept (FramesPerTask = maxValue) must resolve
// under the worker defaults and list every frame. One past the maximum must be
// refused at submission by the maxValue bound itself, or the maximum bounds
// nothing.
func TestPresetModo_ParamsCostAtMaxChunk(t *testing.T) {
	for _, preset := range modoPresets {
		t.Run(preset, func(t *testing.T) {
			params := map[string]string{
				"SceneFile":     "/mnt/show/shots/hero/hero_v012.lxo",
				"Frames":        "1-1000",
				"FramesPerTask": "1000",
				"PassGroup":     "Beauty",
			}
			if preset == "modo-render" {
				delete(params, "PassGroup")
			}
			snap, err := captureModo(t, preset, params)
			if err != nil {
				t.Fatalf("a %d-frame chunk must resolve under the worker's default EXPR limits: %v", modoMaxFramesPerTask, err)
			}
			if len(snap.Steps) != 1 || len(snap.Steps[0].Tasks) != 1 {
				t.Fatalf("1-1000 at %d frames per task must be one task, got %+v", modoMaxFramesPerTask, snap.Steps)
			}
			p := decodeModoParams(t, taskFile(t, snap.Steps[0].Tasks[0], "params"))
			if len(p.Frames) != modoMaxFramesPerTask || p.Frames[0] != 1 || p.Frames[len(p.Frames)-1] != 1000 {
				t.Errorf("params.json frames: %d entries from %v, want the %d frames 1 to 1000",
					len(p.Frames), p.Frames[:min(3, len(p.Frames))], modoMaxFramesPerTask)
			}

			params["FramesPerTask"] = "1001"
			// The refusal must be the maxValue bound itself, not some other
			// failure a larger chunk happened to provoke (an EXPR limit, say).
			_, err = captureModo(t, preset, params)
			wantRefusal := fmt.Sprintf("/parameterValues/FramesPerTask: value 1001 is above maximum %d", modoMaxFramesPerTask)
			if err == nil || !strings.Contains(err.Error(), wantRefusal) {
				t.Errorf("FramesPerTask 1001: err = %v, want the parameter binding refusal %q", err, wantRefusal)
			}
		})
	}
}

// TestPresetModo_ParamsJSONRoundTrips runs each preset's `quoting` fixture case
// (a single quote, a double quote and spaces in the scene's name, and in
// modo-pass-group-render also a backslash in PassGroup) and decodes params.json
// with encoding/json: every value must come back exactly as given. The scene
// path comes back in the worker's own form, since under EXPR phase 3 renders
// PATH values host-native, so on Windows the JSON also carries escaped
// backslashes.
func TestPresetModo_ParamsJSONRoundTrips(t *testing.T) {
	for _, preset := range modoPresets {
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
			if !strings.ContainsAny(params["SceneFile"], `'"`) || !strings.Contains(params["SceneFile"], " ") {
				t.Fatalf("quoting case's SceneFile %q needs both quotes and a space", params["SceneFile"])
			}
			snap, err := captureModo(t, preset, params)
			if err != nil {
				t.Fatalf("Capture: %v", err)
			}
			for _, task := range snap.Steps[0].Tasks {
				p := decodeModoParams(t, taskFile(t, task, "params"))
				if want := filepath.FromSlash(params["SceneFile"]); p.Scene != want {
					t.Errorf("scene = %q, want %q", p.Scene, want)
				}
				if p.Group != params["PassGroup"] {
					t.Errorf("group = %q, want %q", p.Group, params["PassGroup"])
				}
				if len(p.Frames) == 0 {
					t.Error("frames is empty")
				}
			}
		})
	}
}

// TestPresetModo_ShareOneScript keeps the two presets' render.py byte for byte
// the same, so a fix to one cannot miss the other. The script reads "group"
// only when params.json carries it.
func TestPresetModo_ShareOneScript(t *testing.T) {
	var first string
	for i, preset := range modoPresets {
		tmpl, err := presettest.PresetTemplate(presettest.SourcePresets, preset)
		if err != nil {
			t.Fatal(err)
		}
		script := embeddedFileData(t, tmpl, "render")
		if i == 0 {
			first = script
			continue
		}
		if script != first {
			t.Errorf("%s's render.py differs from %s's", preset, modoPresets[0])
		}
	}
}

// modoCommand matches the one -cmd: argument: the script and its parameters
// file, each quoted in braces.
var modoCommand = regexp.MustCompile(`^-cmd:@\{([^{}]+)\} \{([^{}]+)\}$`)

// TestPresetModo_CommandNamesTheScriptAndItsParams pins what the goldens hide
// (they fold both session paths to <WORKDIR>): the argument runs render.py and
// hands it params.json as its argument, in that order, both absolute and in the
// same session folder, and no job value (the scene, the pass group) is part of
// the command line. The script is told where its parameters are, rather than
// left to guess, and Modo never sees job text as a command.
func TestPresetModo_CommandNamesTheScriptAndItsParams(t *testing.T) {
	for _, preset := range modoPresets {
		t.Run(preset, func(t *testing.T) {
			cases, err := presettest.LoadCases(filepath.Join(presetCaseDir, preset+".yaml"))
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range cases {
				if c.Name != "quoting" {
					continue
				}
				snap, err := captureModo(t, preset, c.Params)
				if err != nil {
					t.Fatalf("Capture: %v", err)
				}
				for _, task := range snap.Steps[0].Tasks {
					if task.Command != "modo_cl" || len(task.Args) != 2 || task.Args[0] != "-dboff:crashreport" {
						t.Fatalf("command = %q %q, want modo_cl -dboff:crashreport and one -cmd: argument", task.Command, task.Args)
					}
					m := modoCommand.FindStringSubmatch(task.Args[1])
					if m == nil {
						t.Fatalf("argument %q is not -cmd:@{<script>} {<parameters>}", task.Args[1])
					}
					script, params := m[1], m[2]
					if !filepath.IsAbs(script) || filepath.Base(script) != "render.py" ||
						!filepath.IsAbs(params) || filepath.Base(params) != "params.json" ||
						filepath.Dir(script) != filepath.Dir(params) {
						t.Errorf("script %q and parameters %q must be render.py and params.json in one absolute folder", script, params)
					}
					for _, key := range []string{"SceneFile", "PassGroup"} {
						// A name distinctive enough to find, not a frame count.
						if value := c.Params[key]; value != "" && strings.Contains(strings.Join(task.Args, " "), value) {
							t.Errorf("job value %s=%q appears in the command line %q", key, value, task.Args)
						}
					}
				}
			}
		})
	}
}

// modoRunner stands in for Modo: it installs a recording fake of the lx module
// (and of the modo module, or makes importing it fail), then runs the task's
// render.py the way Modo might, and records every call it saw in a JSON file,
// rewritten after each one so the record survives the script's os._exit.
//
// argv: locate, api, script, record, what to fail, and a folder holding
// nothing. locate says how the script can find params.json, and ONLY that way
// works: "file" runs the script as a file with __file__ set, from the empty
// folder, with no argument; "args" executes its text with no __file__, from the
// empty folder, and lx.args() returns params.json's path; "cwd" executes its
// text with no __file__, no argument and sys.argv holding no script path, from
// the script's own folder; "none" is the same from the empty folder, so nothing
// finds it. api "modo" provides a modo module with Scene().renderItem; "commands"
// makes importing it fail, so the script falls back to commands. fail names a
// call that should raise: "eval:<command text>" or "command:<command name>".
// Markers go to file descriptor 1, which is this process's stdout.
const modoRunner = `import json, os, runpy, sys, types

locate, api, script, record, fail, elsewhere = sys.argv[1:7]
calls = []
params_path = os.path.join(os.path.dirname(script), "params.json")


def note(*call):
    calls.append(list(call))
    with open(record, "w", encoding="utf-8") as f:
        json.dump(calls, f)


def raising(key):
    if fail == key:
        raise RuntimeError("the fake " + key + " failed")


def lx_eval(cmd):
    note("eval", cmd)
    raising("eval:" + cmd)


def lx_command(name, **kw):
    note("command", name, kw)
    raising("command:" + name)


def lx_args():
    return (params_path,) if locate == "args" else ()


lx = types.ModuleType("lx")
lx.eval, lx.command, lx.args = lx_eval, lx_command, lx_args
sys.modules["lx"] = lx

if api == "modo":
    class Channel(object):
        def __init__(self, name):
            self.name = name

        def set(self, value):
            note("channel", self.name, value)

    class Item(object):
        def channel(self, name):
            return Channel(name)

    class Scene(object):
        renderItem = Item()

    modo = types.ModuleType("modo")
    modo.Scene = Scene
    sys.modules["modo"] = modo
else:
    sys.modules["modo"] = None

sys.argv = [""]
os.chdir(os.path.dirname(script) if locate == "cwd" else elsewhere)
if locate == "file":
    runpy.run_path(script, run_name="__main__")
else:
    with open(script, encoding="utf-8") as f:
        source = f.read()
    exec(compile(source, "<modo>", "exec"), {"__name__": "__main__"})

with open(record, "w", encoding="utf-8") as f:
    json.dump(calls, f)
`

// modoRun is one way of running render.py under modoRunner.
type modoRun struct{ locate, api string }

func (r modoRun) name() string { return r.locate + "_" + r.api }

// modoRuns are the ways every scenario is run: each of the script's three ways
// of finding params.json with the modo module present, and the command
// fallback with the script found beside it.
var modoRuns = []modoRun{{"file", "modo"}, {"args", "modo"}, {"cwd", "modo"}, {"file", "commands"}}

// runModoScript writes the task's resolved render.py and params.json (or
// paramsOverride) into a fresh folder and runs the script under modoRunner. It
// returns the calls the fakes recorded, as canonical JSON, what the script wrote
// to stdout and its exit status.
func runModoScript(t *testing.T, python string, task presettest.TaskSnapshot, run modoRun, fail, paramsOverride string) (calls, stdout string, exit int) {
	t.Helper()
	dir := t.TempDir()
	params := taskFile(t, task, "params")
	if paramsOverride != "" {
		params = paramsOverride
	}
	taskDir := filepath.Join(dir, "task")
	elsewhere := filepath.Join(dir, "elsewhere")
	for _, d := range []string{taskDir, elsewhere} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(taskDir, "render.py")
	runner := filepath.Join(dir, "runner.py")
	record := filepath.Join(dir, "calls.json")
	for path, data := range map[string]string{
		script: taskFile(t, task, "render"), filepath.Join(taskDir, "params.json"): params, runner: modoRunner,
	} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(t.Context(), python, "-I", runner, run.locate, run.api, script, record, fail, elsewhere)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("runner failed: %v\nstdout:\n%s\nstderr:\n%s", err, out.String(), errOut.String())
		}
		exit = ee.ExitCode()
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("runner recorded nothing: %v\nstderr:\n%s", err, errOut.String())
	}
	return canonicalJSON(t, data), out.String(), exit
}

// TestPresetModo_ScriptDrivesModo runs the shipped render.py, as resolved for a
// real task, under a real Python with Modo faked, and pins the calls it makes:
// console logging first, the scene opened by keyword arguments, the render
// item's first, last and step set from the task's chunk, render.animation (with
// the pass group as a keyword argument when there is one), the success line,
// then app.quit; and on any error the failure line, openjd_fail and app.quit
// with no success line, or exit status 1 when app.quit itself fails. It also
// runs each of the script's ways of finding params.json with the others
// unavailable, so a way that does not work cannot hide behind one that does.
func TestPresetModo_ScriptDrivesModo(t *testing.T) {
	python := findPython3(t)
	if python == "" {
		t.Skip("no working Python 3 on PATH (tried python3, python)")
	}
	const scene = "/mnt/show/shots/hero/hero_v012.lxo"
	quotedScene := `/mnt/show/shots/hero shot/it's the "hero" scene.lxo`
	group := `Group 'A' "wide" \ left`

	eval := func(cmd string) []any { return []any{"eval", cmd} }
	scenario := func(path string) []any {
		return []any{"command", "scene.open", map[string]any{"filename": filepath.FromSlash(path), "type": "normal"}}
	}
	// span is the calls that set the render item's range, by api.
	span := func(api string, start, stop int) [][]any {
		if api == "modo" {
			return [][]any{{"channel", "first", start}, {"channel", "last", stop}, {"channel", "step", 1}}
		}
		return [][]any{
			eval("select.Item Render"),
			eval(fmt.Sprintf("item.channel first %d", start)),
			eval(fmt.Sprintf("item.channel last %d", stop)),
			eval("item.channel step 1"),
		}
	}
	// opened is the calls up to and including setting the range.
	opened := func(api, path string, start, stop int) [][]any {
		return append([][]any{eval("log.toConsole true"), scenario(path)}, span(api, start, stop)...)
	}
	quit := eval("app.quit")

	tests := []struct {
		name     string
		preset   string
		params   map[string]string
		fail     string
		override string
		runs     []modoRun
		want     func(api string) [][]any
		wantOK   bool
		wantFail string
		wantExit int
	}{
		{
			name: "whole scene chunk", preset: "modo-render",
			params: map[string]string{"SceneFile": scene, "Frames": "1-10", "FramesPerTask": "10"},
			want: func(api string) [][]any {
				return append(opened(api, scene, 1, 10), eval("!render.animation {*}"), quit)
			},
			wantOK: true,
		},
		{
			name: "pass group with quotes and a backslash at negative frames", preset: "modo-pass-group-render",
			params: map[string]string{"SceneFile": quotedScene, "Frames": "-3--1", "PassGroup": group, "FramesPerTask": "3"},
			want: func(api string) [][]any {
				return append(opened(api, quotedScene, -3, -1),
					[]any{"command", "render.animation", map[string]any{"filename": "*", "group": group}}, quit)
			},
			wantOK: true,
		},
		{
			name: "scene open fails", preset: "modo-render",
			params: map[string]string{"SceneFile": scene, "Frames": "3-4", "FramesPerTask": "2"},
			fail:   "command:scene.open",
			want: func(string) [][]any {
				return [][]any{eval("log.toConsole true"), scenario(scene), quit}
			},
			wantFail: "RuntimeError: the fake command:scene.open failed",
		},
		{
			name: "render fails", preset: "modo-render",
			params: map[string]string{"SceneFile": scene, "Frames": "3-4", "FramesPerTask": "2"},
			fail:   "eval:!render.animation {*}",
			want: func(api string) [][]any {
				return append(opened(api, scene, 3, 4), eval("!render.animation {*}"), quit)
			},
			wantFail: "RuntimeError: the fake eval:!render.animation {*} failed",
		},
		{
			name: "group render fails", preset: "modo-pass-group-render",
			params: map[string]string{"SceneFile": scene, "Frames": "3", "PassGroup": "Beauty"},
			fail:   "command:render.animation",
			want: func(api string) [][]any {
				return append(opened(api, scene, 3, 3),
					[]any{"command", "render.animation", map[string]any{"filename": "*", "group": "Beauty"}}, quit)
			},
			wantFail: "RuntimeError: the fake command:render.animation failed",
		},
		{
			name: "frames with a gap are refused", preset: "modo-render",
			params:   map[string]string{"SceneFile": scene, "Frames": "1"},
			override: `{"scene": "x.lxo", "frames": [1, 3]}`,
			want:     func(string) [][]any { return [][]any{quit} },
			wantFail: "ValueError: the task's frames are not consecutive: [1, 3]",
		},
		{
			name: "no frames are refused", preset: "modo-render",
			params:   map[string]string{"SceneFile": scene, "Frames": "1"},
			override: `{"scene": "x.lxo", "frames": []}`,
			want:     func(string) [][]any { return [][]any{quit} },
			wantFail: "ValueError: the task has no frames",
		},
		{
			name: "app quit fails after a render", preset: "modo-render",
			params: map[string]string{"SceneFile": scene, "Frames": "5", "FramesPerTask": "1"},
			fail:   "eval:app.quit",
			want: func(api string) [][]any {
				return append(opened(api, scene, 5, 5), eval("!render.animation {*}"), quit)
			},
			wantOK:   true,
			wantExit: 0,
		},
		{
			name: "app quit fails after an error", preset: "modo-render",
			params:   map[string]string{"SceneFile": scene, "Frames": "1"},
			fail:     "eval:app.quit",
			override: `{"scene": "x.lxo", "frames": []}`,
			want:     func(string) [][]any { return [][]any{quit} },
			wantFail: "ValueError: the task has no frames",
			wantExit: 1,
		},
		{
			name: "params json is nowhere to be found", preset: "modo-render",
			params:   map[string]string{"SceneFile": scene, "Frames": "1"},
			runs:     []modoRun{{"none", "modo"}},
			want:     func(string) [][]any { return [][]any{quit} },
			wantFail: "OSError: params.json not found; looked at",
		},
	}
	for _, tc := range tests {
		snap, err := captureModo(t, tc.preset, tc.params)
		if err != nil {
			t.Fatalf("%s: Capture: %v", tc.name, err)
		}
		task := snap.Steps[0].Tasks[0]
		runs := tc.runs
		if runs == nil {
			runs = modoRuns
		}
		for _, run := range runs {
			t.Run(tc.name+"/"+run.name(), func(t *testing.T) {
				wantJSON, err := json.Marshal(tc.want(run.api))
				if err != nil {
					t.Fatal(err)
				}
				calls, stdout, exit := runModoScript(t, python, task, run, tc.fail, tc.override)
				if calls != canonicalJSON(t, wantJSON) {
					t.Errorf("Modo calls:\n  got  %s\n  want %s", calls, canonicalJSON(t, wantJSON))
				}
				if exit != tc.wantExit {
					t.Errorf("exit status = %d, want %d", exit, tc.wantExit)
				}
				if gotOK := strings.Contains(stdout, "SQI_RENDER: OK\n"); gotOK != tc.wantOK {
					t.Errorf("success line printed = %v, want %v; stdout:\n%s", gotOK, tc.wantOK, stdout)
				}
				if tc.wantFail == "" {
					if strings.Contains(stdout, "SQI_RENDER: FAILED:") {
						t.Errorf("failure line printed on success; stdout:\n%s", stdout)
					}
					return
				}
				// IOError is OSError on Python 3, so the reason names OSError.
				for _, line := range []string{"SQI_RENDER: FAILED: " + tc.wantFail, "openjd_fail: " + tc.wantFail} {
					if !strings.Contains(stdout, line) {
						t.Errorf("stdout lacks %q; stdout:\n%s", line, stdout)
					}
				}
			})
		}
	}
}
