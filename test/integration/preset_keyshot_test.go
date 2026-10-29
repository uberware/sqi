// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

// preset_keyshot_test.go — what the KeyShot presets' golden files cannot show
// on their own: that params.json is JSON a script can decode, that its cost
// stays inside the worker's EXPR limits at the largest chunk the presets
// allow, that a bad Output File is refused at submission, and what the
// embedded render.py asks KeyShot to do with it.
//
// Untagged, like the rest of the preset harness, so `make test-cover` runs it.
// Every test is named TestPresetKeyShot_* so the preset harness's -run regex
// (make test-preset-harness, and CI's preset-harness-windows job) selects it:
// params.json's escaping differs by host, and only a Windows run can show a
// backslash it failed to escape.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/presettest"
	"github.com/uberware/sqi/internal/store"
)

// keyshotPresets are the two KeyShot presets, which share one render.py and one
// params.json shape (the studio one adds "studio").
var keyshotPresets = []string{"keyshot-render", "keyshot-studio-render"}

// keyshotMaxFramesPerTask is the FramesPerTask maxValue both presets declare
// and their readmes state.
const keyshotMaxFramesPerTask = 1000

// captureKeyShot expands one KeyShot preset under params through the
// production pipeline (presettest.Capture, which resolves phase 3 against the
// WORKER's default EXPR limits) and returns the snapshot.
func captureKeyShot(t *testing.T, preset string, params map[string]string) (presettest.Snapshot, error) {
	t.Helper()
	tmpl, err := presettest.PresetTemplate(presettest.SourcePresets, preset)
	if err != nil {
		t.Fatalf("PresetTemplate %s: %v", preset, err)
	}
	return presettest.Capture(t.Context(), tmpl, presettest.Options{Params: params, Format: store.TemplateFormatYAML})
}

// keyshotBaseParams are the parameters a valid job of preset needs.
func keyshotBaseParams(preset string) map[string]string {
	params := map[string]string{
		"SceneFile":  "/mnt/show/product/chair_v004.bip",
		"Frames":     "1-3",
		"OutputFile": "/mnt/renders/chair/beauty.####.png",
	}
	if preset == "keyshot-studio-render" {
		params["Studio"] = "Front"
	}
	return params
}

// keyshotParams is params.json as the script reads it.
type keyshotParams struct {
	Frames []int  `json:"frames"`
	Output string `json:"output"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Studio string `json:"studio"`
}

// decodeKeyShotParams decodes params.json strictly: an unknown key is an error,
// so a key the script does not read cannot slip in unnoticed.
func decodeKeyShotParams(t *testing.T, data string) keyshotParams {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(data))
	dec.DisallowUnknownFields()
	var p keyshotParams
	if err := dec.Decode(&p); err != nil {
		t.Fatalf("params.json is not the JSON object the script expects: %v\n%s", err, data)
	}
	return p
}

// TestPresetKeyShot_ParamsCostAtMaxChunk pins params.json's cost where it is
// highest. It reads Task.Param.Frame, so it is evaluated at phase 3 on the
// worker, against the worker's EXPR limits, and grows with the chunk: the
// largest chunk the presets accept (FramesPerTask = maxValue) must resolve
// under the worker defaults and list every frame. One past the maximum must be
// refused at submission, or the maximum bounds nothing.
func TestPresetKeyShot_ParamsCostAtMaxChunk(t *testing.T) {
	for _, preset := range keyshotPresets {
		t.Run(preset, func(t *testing.T) {
			params := keyshotBaseParams(preset)
			params["Frames"] = "1-1000"
			params["FramesPerTask"] = "1000"
			snap, err := captureKeyShot(t, preset, params)
			if err != nil {
				t.Fatalf("a %d-frame chunk must resolve under the worker's default EXPR limits: %v", keyshotMaxFramesPerTask, err)
			}
			if len(snap.Steps) != 1 || len(snap.Steps[0].Tasks) != 1 {
				t.Fatalf("1-1000 at %d frames per task must be one task, got %+v", keyshotMaxFramesPerTask, snap.Steps)
			}
			p := decodeKeyShotParams(t, taskFile(t, snap.Steps[0].Tasks[0], "params"))
			if len(p.Frames) != keyshotMaxFramesPerTask || p.Frames[0] != 1 || p.Frames[len(p.Frames)-1] != 1000 {
				t.Errorf("params.json frames: %d entries from %v, want the %d frames 1 to 1000",
					len(p.Frames), p.Frames[:min(3, len(p.Frames))], keyshotMaxFramesPerTask)
			}

			params["FramesPerTask"] = "1001"
			// The refusal must be the maxValue bound itself, not some other
			// failure a larger chunk happened to provoke (an EXPR limit, say).
			_, err = captureKeyShot(t, preset, params)
			wantRefusal := fmt.Sprintf("/parameterValues/FramesPerTask: value 1001 is above maximum %d", keyshotMaxFramesPerTask)
			if err == nil || !strings.Contains(err.Error(), wantRefusal) {
				t.Errorf("FramesPerTask 1001: err = %v, want the parameter binding refusal %q", err, wantRefusal)
			}
		})
	}
}

// TestPresetKeyShot_ParamsJSONRoundTrips runs each preset's `quoting` fixture
// case (a single quote, a double quote and spaces in Output File's name, and in
// keyshot-studio-render also a backslash in Studio) and decodes params.json
// with encoding/json: every value must come back exactly as given. A path
// comes back in the worker's own form, since under EXPR phase 3 renders PATH
// values host-native, so on Windows the JSON also carries escaped backslashes.
func TestPresetKeyShot_ParamsJSONRoundTrips(t *testing.T) {
	for _, preset := range keyshotPresets {
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
			snap, err := captureKeyShot(t, preset, params)
			if err != nil {
				t.Fatalf("Capture: %v", err)
			}
			for _, task := range snap.Steps[0].Tasks {
				p := decodeKeyShotParams(t, taskFile(t, task, "params"))
				if want := filepath.FromSlash(params["OutputFile"]); p.Output != want {
					t.Errorf("output = %q, want %q", p.Output, want)
				}
				if p.Studio != params["Studio"] {
					t.Errorf("studio = %q, want %q", p.Studio, params["Studio"])
				}
				if len(p.Frames) == 0 {
					t.Error("frames is empty")
				}
			}
		})
	}
}

// TestPresetKeyShot_ShareOneScript keeps the two presets' render.py byte for
// byte the same, so a fix to one cannot miss the other. The script reads
// "studio" only when params.json carries it.
func TestPresetKeyShot_ShareOneScript(t *testing.T) {
	var first string
	for i, preset := range keyshotPresets {
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
			t.Errorf("%s's render.py differs from %s's", preset, keyshotPresets[0])
		}
	}
}

// TestPresetKeyShot_OutputFileRules pins what Output File accepts. The frame
// number is one run of # in the file name (a run in a folder does not count),
// and the extension picks the image format: anything KeyShot's renderImage
// cannot write is refused when the job is submitted, not on the first task.
func TestPresetKeyShot_OutputFileRules(t *testing.T) {
	const (
		wantRun = "Output File must contain exactly one run of # characters marking the frame number"
		wantExt = "Output File must end in .jpg, .jpeg, .png, .exr, .tif, .tiff or .psd"
	)
	tests := []struct {
		output  string
		wantErr string
	}{
		{"/mnt/r/beauty.####.png", ""},
		{"/mnt/r/beauty.#.jpg", ""},
		{"/mnt/r/beauty.##.jpeg", ""},
		{"/mnt/r/a_###_b.exr", ""},
		{"/mnt/r/beauty.####.tif", ""},
		{"/mnt/r/beauty.####.tiff", ""},
		{"/mnt/r/beauty.####.psd", ""},
		{"/mnt/r/beauty.####.PNG", ""},
		{"/mnt/r#1/beauty.####.png", ""},
		{"/mnt/r/beauty.png", wantRun},
		{"/mnt/r#1/beauty.png", wantRun},
		{"/mnt/r/beauty.##.####.png", wantRun},
		{"/mnt/r/be#auty.####.png", wantRun},
		{"/mnt/r/beauty.####.bmp", wantExt},
		{"/mnt/r/beauty.####", wantExt},
		{"/mnt/r/beauty.####.png.gz", wantExt},
	}
	for _, preset := range keyshotPresets {
		for _, tc := range tests {
			t.Run(preset+"/"+strings.ReplaceAll(tc.output, "/", "_"), func(t *testing.T) {
				params := keyshotBaseParams(preset)
				params["OutputFile"] = tc.output
				_, err := captureKeyShot(t, preset, params)
				switch {
				case tc.wantErr == "" && err != nil:
					t.Errorf("Output File %q must be accepted: %v", tc.output, err)
				case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
					t.Errorf("Output File %q: err = %v, want a refusal containing %q", tc.output, err, tc.wantErr)
				}
			})
		}
	}
}

// keyshotRunner stands in for KeyShot: it installs a recording fake of the lux
// module, then runs the task's render.py the way KeyShot might, and writes
// every call it saw to a JSON file.
//
// argv: mode, script path, record path, and what the fake should do wrong
// ("" for nothing; "raise:<name>" makes that lux function raise; "false@<n>"
// makes renderImage return False at frame n; "nofile" makes it return True
// without writing; "none" makes it return None after writing, as a headless
// renderImage might). mode "file" runs the script as a file with __file__ set
// and lux importable; mode "cwd" executes its text with no __file__ in a
// namespace holding lux, from the script's folder with sys.argv holding no
// script path; mode "argv" does the same from another folder with the script
// named on sys.argv, so each of the script's three ways of finding params.json
// is exercised by exactly one mode. Markers go to file
// descriptor 1, which is this process's stdout.
const keyshotRunner = `import json, os, runpy, sys, types

mode, script, record, fail = sys.argv[1:5]
calls = []


class Opts:
    def setAddToQueue(self, value):
        calls.append(["setAddToQueue", value])


def raising(name):
    if fail == "raise:" + name:
        raise RuntimeError("the fake " + name + " failed")


def setActiveStudio(name):
    calls.append(["setActiveStudio", name])
    raising("setActiveStudio")


def getRenderOptions():
    calls.append(["getRenderOptions"])
    return Opts()


def setAnimationFrame(frame):
    calls.append(["setAnimationFrame", frame])
    raising("setAnimationFrame")


def renderImage(path, **kw):
    kw["opts"] = "opts" if isinstance(kw.get("opts"), Opts) else repr(kw.get("opts"))
    calls.append(["renderImage", path, kw])
    raising("renderImage")
    frame = calls[-2][1]
    if fail == "false@%d" % frame:
        return False
    if fail != "nofile":
        with open(path, "wb") as f:
            f.write(b"image")
    if fail == "none":
        return None
    return True


lux = types.ModuleType("lux")
for name in ("JPEG", "TIFF8", "EXR", "TIFF32", "PNG", "PSD8", "PSD32", "PSD16"):
    setattr(lux, "RENDER_OUTPUT_" + name, "RENDER_OUTPUT_" + name)
for fn in (setActiveStudio, getRenderOptions, setAnimationFrame, renderImage):
    setattr(lux, fn.__name__, fn)

if mode == "file":
    sys.modules["lux"] = lux
    runpy.run_path(script, run_name="__main__")
else:
    with open(script, encoding="utf-8") as f:
        source = f.read()
    if mode == "cwd":
        sys.argv = [""]
        os.chdir(os.path.dirname(script))
    else:
        sys.argv = ["", script]
    exec(compile(source, "<keyshot>", "exec"), {"lux": lux, "__name__": "__main__"})

with open(record, "w", encoding="utf-8") as f:
    json.dump(calls, f)
`

// keyshotRun is one run of render.py under keyshotRunner.
type keyshotRun struct {
	mode     string
	fail     string
	override func(p map[string]any) // changes params.json before the run
	stale    string                 // a file to create before the run
}

// runKeyShotScript writes the task's resolved render.py and its params.json
// (with output pointed into the test's own folder and run.override applied)
// into a fresh directory and runs the script under keyshotRunner. It returns
// the calls the fake recorded, as canonical JSON, and what the script wrote to
// stdout.
func runKeyShotScript(t *testing.T, python string, task presettest.TaskSnapshot, outPattern string, run keyshotRun) (calls, stdout string) {
	t.Helper()
	dir := t.TempDir()
	var p map[string]any
	if err := json.Unmarshal([]byte(taskFile(t, task, "params")), &p); err != nil {
		t.Fatalf("params.json: %v", err)
	}
	p["output"] = outPattern
	if run.override != nil {
		run.override(p)
	}
	params, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "task", "render.py")
	runner := filepath.Join(dir, "runner.py")
	record := filepath.Join(dir, "calls.json")
	if err := os.MkdirAll(filepath.Dir(script), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{
		script: taskFile(t, task, "render"), filepath.Join(dir, "task", "params.json"): string(params), runner: keyshotRunner,
	} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if run.stale != "" {
		if err := os.MkdirAll(filepath.Dir(run.stale), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(run.stale, []byte("old frame"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(t.Context(), python, "-I", runner, run.mode, script, record, run.fail)
	cmd.Dir = dir
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

// TestPresetKeyShot_ScriptDrivesKeyShot runs the shipped render.py, as
// resolved for a real task, under a real Python with lux faked, and pins the
// calls it makes: the studio first when given, the scene's own size unless
// Width or Height is set, one setAnimationFrame and renderImage per frame in
// the format the extension picks, a file check after each, the success line
// after the last, and on any error the failure line, openjd_fail, and no
// success line.
func TestPresetKeyShot_ScriptDrivesKeyShot(t *testing.T) {
	python := findPython3(t)
	if python == "" {
		t.Skip("no working Python 3 on PATH (tried python3, python)")
	}
	studio := `Cam 'A' "wide" \ left`
	// A double quote cannot be part of a Windows file name, and this test
	// really writes the files; the quoting case of the fixtures covers the
	// spelling in params.json on every OS.
	hero := `it's the "hero" shot`
	if runtime.GOOS == "windows" {
		hero = `it's the hero shot`
	}
	// frame renders the call pair the script makes for one frame.
	type call = []any
	frame := func(path string, f int, format string, extra map[string]any) []call {
		kw := map[string]any{"opts": "opts", "format": format}
		maps.Copy(kw, extra)
		return []call{{"setAnimationFrame", f}, {"renderImage", path, kw}}
	}
	head := call{"getRenderOptions"}
	queue := call{"setAddToQueue", false}
	tests := []struct {
		name     string
		preset   string
		frames   string
		pattern  string // under the test's output folder
		params   map[string]string
		run      keyshotRun
		want     func(out string) []call // given the folder, the calls
		wantOK   bool
		wantFail string // substring of the failure reason
	}{
		{
			name: "a chunk at the scene size", preset: "keyshot-render", frames: "1-3",
			pattern: "chair/beauty.####.png", wantOK: true,
			want: func(out string) []call {
				c := []call{head, queue}
				for f := 1; f <= 3; f++ {
					c = append(c, frame(filepath.Join(out, "chair", fmt.Sprintf("beauty.%04d.png", f)), f, "RENDER_OUTPUT_PNG", nil)...)
				}
				return c
			},
		},
		{
			name: "a gapped list with a size", preset: "keyshot-render", frames: "1-3,7",
			params:  map[string]string{"Width": "1280", "Height": "720"},
			pattern: "chair/beauty.##.exr", wantOK: true,
			want: func(out string) []call {
				c := []call{head, queue}
				for _, f := range []int{1, 2, 3, 7} {
					c = append(c, frame(filepath.Join(out, "chair", fmt.Sprintf("beauty.%02d.exr", f)), f, "RENDER_OUTPUT_EXR", map[string]any{"width": 1280, "height": 720})...)
				}
				return c
			},
		},
		{
			name: "only the width", preset: "keyshot-render", frames: "5",
			params:  map[string]string{"Width": "640"},
			pattern: "beauty.####.jpg", wantOK: true,
			want: func(out string) []call {
				c := []call{head, queue}
				return append(c, frame(filepath.Join(out, "beauty.0005.jpg"), 5, "RENDER_OUTPUT_JPEG", map[string]any{"width": 640})...)
			},
		},
		{
			name: "negative frames", preset: "keyshot-render", frames: "-2-0",
			pattern: "beauty.####.tif", wantOK: true,
			want: func(out string) []call {
				c := []call{head, queue}
				for f := -2; f <= 0; f++ {
					c = append(c, frame(filepath.Join(out, fmt.Sprintf("beauty.%04d.tif", f)), f, "RENDER_OUTPUT_TIFF8", nil)...)
				}
				return c
			},
		},
		{
			name: "studio with quotes and a backslash, output with quotes and spaces", preset: "keyshot-studio-render", frames: "1-2",
			params:  map[string]string{"Studio": studio},
			pattern: "hero car/" + hero + ".####.psd", wantOK: true,
			want: func(out string) []call {
				c := []call{{"setActiveStudio", studio}, head, queue}
				for f := 1; f <= 2; f++ {
					c = append(c, frame(filepath.Join(out, "hero car", fmt.Sprintf("%s.%04d.psd", hero, f)), f, "RENDER_OUTPUT_PSD8", nil)...)
				}
				return c
			},
		},
		{
			name: "renderImage returns nothing", preset: "keyshot-render", frames: "1",
			pattern: "beauty.####.png", run: keyshotRun{fail: "none"}, wantOK: true,
			want: func(out string) []call {
				return append([]call{head, queue}, frame(filepath.Join(out, "beauty.0001.png"), 1, "RENDER_OUTPUT_PNG", nil)...)
			},
		},
		{
			name: "renderImage returns False at the second frame", preset: "keyshot-render", frames: "1-3",
			pattern: "beauty.####.png", run: keyshotRun{fail: "false@2"},
			want: func(out string) []call {
				c := []call{head, queue}
				c = append(c, frame(filepath.Join(out, "beauty.0001.png"), 1, "RENDER_OUTPUT_PNG", nil)...)
				return append(c, frame(filepath.Join(out, "beauty.0002.png"), 2, "RENDER_OUTPUT_PNG", nil)...)
			},
			wantFail: "RuntimeError: KeyShot did not render frame 2",
		},
		{
			name: "renderImage raises", preset: "keyshot-render", frames: "1-3",
			pattern: "beauty.####.png", run: keyshotRun{fail: "raise:renderImage"},
			want: func(out string) []call {
				return append([]call{head, queue}, frame(filepath.Join(out, "beauty.0001.png"), 1, "RENDER_OUTPUT_PNG", nil)...)
			},
			wantFail: "RuntimeError: the fake renderImage failed",
		},
		{
			name: "no image file is written", preset: "keyshot-render", frames: "1",
			pattern: "beauty.####.png", run: keyshotRun{fail: "nofile"},
			want: func(out string) []call {
				return append([]call{head, queue}, frame(filepath.Join(out, "beauty.0001.png"), 1, "RENDER_OUTPUT_PNG", nil)...)
			},
			wantFail: "RuntimeError: frame 1 was rendered but ",
		},
		{
			name: "an old file of the same name does not count", preset: "keyshot-render", frames: "1",
			pattern: "beauty.####.png", run: keyshotRun{fail: "nofile", stale: "beauty.0001.png"},
			want: func(out string) []call {
				return append([]call{head, queue}, frame(filepath.Join(out, "beauty.0001.png"), 1, "RENDER_OUTPUT_PNG", nil)...)
			},
			wantFail: "RuntimeError: frame 1 was rendered but ",
		},
		{
			name: "setActiveStudio raises", preset: "keyshot-studio-render", frames: "1",
			pattern: "beauty.####.png", run: keyshotRun{fail: "raise:setActiveStudio"},
			want:     func(string) []call { return []call{{"setActiveStudio", "Front"}} },
			wantFail: "RuntimeError: the fake setActiveStudio failed",
		},
		{
			name: "an extension KeyShot cannot write is refused", preset: "keyshot-render", frames: "1",
			pattern: "beauty.####.png",
			run: keyshotRun{override: func(p map[string]any) {
				p["output"] = strings.TrimSuffix(fmt.Sprint(p["output"]), "png") + "bmp"
			}},
			want:     func(string) []call { return []call{} },
			wantFail: "ValueError: unsupported output extension '.bmp'",
		},
		{
			name: "an output name with no run of # is refused", preset: "keyshot-render", frames: "1",
			pattern: "beauty.####.png",
			run: keyshotRun{override: func(p map[string]any) {
				p["output"] = strings.Replace(fmt.Sprint(p["output"]), "####", "", 1)
			}},
			want:     func(string) []call { return []call{} },
			wantFail: "ValueError: the output file name needs exactly one run of #, found 0: beauty..png",
		},
		{
			name: "an output name with two runs of # is refused", preset: "keyshot-render", frames: "1",
			pattern: "beauty.####.png",
			run: keyshotRun{override: func(p map[string]any) {
				p["output"] = strings.Replace(fmt.Sprint(p["output"]), ".png", ".##.png", 1)
			}},
			want:     func(string) []call { return []call{} },
			wantFail: "ValueError: the output file name needs exactly one run of #, found 2: beauty.####.##.png",
		},
		{
			name: "a task with no frames is refused", preset: "keyshot-render", frames: "1",
			pattern: "beauty.####.png",
			run:     keyshotRun{override: func(p map[string]any) { p["frames"] = []any{} }},
			want:    func(string) []call { return []call{} }, wantFail: "ValueError: the task has no frames",
		},
	}
	for _, tc := range tests {
		params := keyshotBaseParams(tc.preset)
		params["Frames"] = tc.frames
		maps.Copy(params, tc.params)
		snap, err := captureKeyShot(t, tc.preset, params)
		if err != nil {
			t.Fatalf("%s: Capture: %v", tc.name, err)
		}
		task := snap.Steps[0].Tasks[0]
		for _, mode := range []string{"file", "cwd", "argv"} {
			t.Run(strings.ReplaceAll(tc.name, " ", "_")+"/"+mode, func(t *testing.T) {
				out := filepath.Join(t.TempDir(), "renders")
				run := tc.run
				run.mode = mode
				if run.stale != "" {
					run.stale = filepath.Join(out, run.stale)
				}
				pattern := filepath.Join(out, filepath.FromSlash(tc.pattern))
				calls, stdout := runKeyShotScript(t, python, task, pattern, run)
				wantJSON, err := json.Marshal(tc.want(out))
				if err != nil {
					t.Fatal(err)
				}
				if want := canonicalJSON(t, wantJSON); calls != want {
					t.Errorf("lux calls:\n  got  %s\n  want %s", calls, want)
				}
				gotOK := strings.Contains(stdout, "SQI_RENDER: OK\n")
				if gotOK != tc.wantOK {
					t.Errorf("success line printed = %v, want %v; stdout:\n%s", gotOK, tc.wantOK, stdout)
				}
				if tc.wantFail == "" {
					if strings.Contains(stdout, "SQI_RENDER: FAILED:") || strings.Contains(stdout, "openjd_fail:") {
						t.Errorf("failure line printed on success; stdout:\n%s", stdout)
					}
					return
				}
				for _, prefix := range []string{"SQI_RENDER: FAILED: ", "openjd_fail: "} {
					if !strings.Contains(stdout, prefix+tc.wantFail) {
						t.Errorf("stdout lacks %q followed by %q; stdout:\n%s", prefix, tc.wantFail, stdout)
					}
				}
			})
		}
	}
}
