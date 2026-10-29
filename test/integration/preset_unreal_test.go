// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

// preset_unreal_test.go — what the Unreal presets' golden files cannot show
// on their own: that params.json is JSON a script can decode, that its cost is
// fixed and inside the worker's EXPR limits, that the -execcmds argument is
// one comma-free line of Python importing the embedded script and carrying no
// job value, that a session folder Unreal could not be handed that way fails
// the task with a reason, and what the embedded script asks Unreal to do.
//
// Untagged, like the rest of the preset harness, so `make test-cover` runs it.
// Every test is named TestPresetUnreal_* so the preset harness's -run regex
// (make test-preset-harness, and CI's preset-harness-windows job) selects it.
// The presets are Windows-only, but nothing here starts Unreal or needs the
// worker's OS gate, so these tests run on every OS: params.json's escaping and
// the script's logic are host-independent, and only a Windows run can show a
// backslash that was not escaped.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/presettest"
	"github.com/uberware/sqi/internal/store"
)

// unrealPresets are the two Unreal presets, which share one script and one
// command line; their params.json differ in the asset keys.
var unrealPresets = []string{"unreal-mrq-job-render", "unreal-mrq-queue-render"}

// unrealMaxLength is the maxLength every asset-path parameter and
// OutputDirectory declare, which bounds params.json's cost.
const unrealMaxLength = 1024

// unrealScriptFile is the embedded script's file name, which the -execcmds
// command imports as a module.
const unrealScriptFile = "sqi_unreal_render.py"

// captureUnreal expands one Unreal preset under params through the production
// pipeline (presettest.Capture, which resolves phase 3 against the WORKER's
// default EXPR limits) and returns the snapshot.
func captureUnreal(t *testing.T, preset string, params map[string]string) (presettest.Snapshot, error) {
	t.Helper()
	tmpl, err := presettest.PresetTemplate(presettest.SourcePresets, preset)
	if err != nil {
		t.Fatalf("PresetTemplate %s: %v", preset, err)
	}
	return presettest.Capture(t.Context(), tmpl, presettest.Options{Params: params, Format: store.TemplateFormatYAML})
}

// unrealParams is params.json as the script reads it: the job preset's three
// asset paths, or the queue preset's queue, and the output directory.
type unrealParams struct {
	Sequence        *string `json:"sequence"`
	Map             *string `json:"map"`
	Config          *string `json:"config"`
	Queue           *string `json:"queue"`
	OutputDirectory *string `json:"output_directory"`
}

// decodeUnrealParams decodes params.json strictly: an unknown key is an error,
// so a key the script does not read cannot slip in unnoticed. It also checks
// that each preset's file carries exactly its own keys.
func decodeUnrealParams(t *testing.T, preset, data string) unrealParams {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(data))
	dec.DisallowUnknownFields()
	var p unrealParams
	if err := dec.Decode(&p); err != nil {
		t.Fatalf("params.json is not the JSON object the script expects: %v\n%s", err, data)
	}
	job := p.Sequence != nil && p.Map != nil && p.Config != nil && p.Queue == nil
	queue := p.Sequence == nil && p.Map == nil && p.Config == nil && p.Queue != nil
	if p.OutputDirectory == nil || (preset == unrealPresets[0] && !job) || (preset == unrealPresets[1] && !queue) {
		t.Fatalf("%s params.json has the wrong keys:\n%s", preset, data)
	}
	return p
}

// unrealBaseParams are a valid parameter set for preset.
func unrealBaseParams(preset string) map[string]string {
	params := map[string]string{"SceneFile": `C:\Projects\HeroFilm\HeroFilm.uproject`}
	if preset == unrealPresets[0] {
		params["LevelSequence"] = "/Game/Cinematics/Hero/SEQ_Hero"
		params["Map"] = "/Game/Maps/Hero"
		params["MovieRenderConfig"] = "/Game/Cinematics/MRQ_Final"
	} else {
		params["QueueAsset"] = "/Game/Cinematics/MRQ_HeroQueue"
	}
	return params
}

// TestPresetUnreal_ParamsCostIsFixed pins params.json's cost. The presets have
// no frame parameter, so it is the same for every task, bounded only by the
// lengths of the values it quotes: with every one of them at its maxLength it
// must still resolve under the worker's default EXPR limits (phase 3, on the
// worker) and carry each value whole. One character more must be refused at
// submission by the maxLength bound itself, or the bound bounds nothing.
func TestPresetUnreal_ParamsCostIsFixed(t *testing.T) {
	long := func(prefix string) string { return prefix + strings.Repeat("x", unrealMaxLength-len(prefix)) }
	for _, preset := range unrealPresets {
		t.Run(preset, func(t *testing.T) {
			params := unrealBaseParams(preset)
			assetKeys := []string{"QueueAsset"}
			if preset == unrealPresets[0] {
				assetKeys = []string{"LevelSequence", "Map", "MovieRenderConfig"}
			}
			for _, key := range assetKeys {
				params[key] = long("/Game/" + key + "/")
			}
			params["OutputDirectory"] = long(`C:\Renders\`)
			snap, err := captureUnreal(t, preset, params)
			if err != nil {
				t.Fatalf("params.json at every value's maxLength must resolve under the worker's default EXPR limits: %v", err)
			}
			if len(snap.Steps) != 1 || len(snap.Steps[0].Tasks) != 1 {
				t.Fatalf("an Unreal job must be one task, got %+v", snap.Steps)
			}
			p := decodeUnrealParams(t, preset, taskFile(t, snap.Steps[0].Tasks[0], "params"))
			got := map[string]*string{
				"LevelSequence": p.Sequence, "Map": p.Map, "MovieRenderConfig": p.Config, "QueueAsset": p.Queue,
				"OutputDirectory": p.OutputDirectory,
			}
			for _, key := range append(assetKeys, "OutputDirectory") {
				if got[key] == nil || *got[key] != params[key] {
					t.Errorf("params.json does not carry %s whole", key)
				}
			}

			params[assetKeys[0]] += "x"
			_, err = captureUnreal(t, preset, params)
			wantRefusal := fmt.Sprintf("/parameterValues/%s: value length %d is above maximum %d",
				assetKeys[0], unrealMaxLength+1, unrealMaxLength)
			if err == nil || !strings.Contains(err.Error(), wantRefusal) {
				t.Errorf("%s one past maxLength: err = %v, want the parameter binding refusal %q", assetKeys[0], err, wantRefusal)
			}
		})
	}
}

// TestPresetUnreal_ParamsJSONRoundTrips runs each preset's `quoting` fixture
// case (a single quote, a double quote, a backslash and spaces in an asset
// path, and both quotes and spaces in the output directory) and decodes
// params.json with encoding/json: every value must come back exactly as given.
// The output directory comes back in the worker's own form, since under EXPR
// phase 3 renders PATH values host-native; it is a Windows path, the same on
// every host.
func TestPresetUnreal_ParamsJSONRoundTrips(t *testing.T) {
	for _, preset := range unrealPresets {
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
			asset := params["LevelSequence"] + params["QueueAsset"]
			if !strings.ContainsAny(asset, `'"`) || !strings.Contains(asset, `\`) || !strings.Contains(asset, " ") ||
				!strings.ContainsAny(params["OutputDirectory"], `'"`) {
				t.Fatalf("quoting case needs quotes, a backslash and a space in the asset path and quotes in OutputDirectory: %v", params)
			}
			snap, err := captureUnreal(t, preset, params)
			if err != nil {
				t.Fatalf("Capture: %v", err)
			}
			p := decodeUnrealParams(t, preset, taskFile(t, snap.Steps[0].Tasks[0], "params"))
			want := map[*string]string{p.OutputDirectory: filepath.FromSlash(params["OutputDirectory"])}
			if preset == unrealPresets[0] {
				want[p.Sequence], want[p.Map], want[p.Config] = params["LevelSequence"], params["Map"], params["MovieRenderConfig"]
			} else {
				want[p.Queue] = params["QueueAsset"]
			}
			for got, w := range want {
				if *got != w {
					t.Errorf("params.json value = %q, want %q", *got, w)
				}
			}
		})
	}
}

// TestPresetUnreal_ShareOneScript keeps the two presets' script byte for byte
// the same, so a fix to one cannot miss the other. The script tells the two
// apart by params.json's keys.
func TestPresetUnreal_ShareOneScript(t *testing.T) {
	var first string
	for i, preset := range unrealPresets {
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
			t.Errorf("%s's script differs from %s's", preset, unrealPresets[0])
		}
	}
}

// unrealExecCmds matches the -execcmds argument: one Python expression that
// appends the script's folder to sys.path and imports the script by module
// name. The quoted path may hold neither a quote, a comma nor a backslash.
var unrealExecCmds = regexp.MustCompile(`^-execcmds=py __import__\('sys'\)\.path\.append\(__import__\('os'\)\.path\.dirname\('([^'",\\]+)'\)\) or __import__\('sqi_unreal_render'\)$`)

// unrealFlags are the switches between the project and -execcmds, in order.
var unrealFlags = []string{
	"-log", "-unattended", "-stdout", "-allowstdoutlogverbosity", "-NoLoadingScreen",
	"-NoScreenMessages", "-RenderOffscreen", "-nohmd", "-windowed",
}

// TestPresetUnreal_ExecCmdsImportsTheScript pins what the goldens hide (they
// fold the session path to <WORKDIR>): the command line is the project, the
// switches and one -execcmds command; that command names the embedded script
// by its absolute path in forward slashes, holds no comma (Unreal splits
// -execcmds at commas) and no double quote (Unreal quotes the value in double
// quotes), and no job value is anywhere in the command line but the project.
func TestPresetUnreal_ExecCmdsImportsTheScript(t *testing.T) {
	for _, preset := range unrealPresets {
		t.Run(preset, func(t *testing.T) {
			cases, err := presettest.LoadCases(filepath.Join(presetCaseDir, preset+".yaml"))
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range cases {
				if c.Name != "quoting" && c.Name != "default" {
					continue
				}
				snap, err := captureUnreal(t, preset, c.Params)
				if err != nil {
					t.Fatalf("%s: Capture: %v", c.Name, err)
				}
				task := snap.Steps[0].Tasks[0]
				n := len(task.Args)
				if task.Command != "UnrealEditor-Cmd" || n != len(unrealFlags)+2 ||
					task.Args[0] != filepath.FromSlash(c.Params["SceneFile"]) ||
					strings.Join(task.Args[1:n-1], " ") != strings.Join(unrealFlags, " ") {
					t.Fatalf("%s: command = %s %q, want UnrealEditor-Cmd <project> %v -execcmds=...", c.Name, task.Command, task.Args, unrealFlags)
				}
				m := unrealExecCmds.FindStringSubmatch(task.Args[n-1])
				if m == nil {
					t.Fatalf("%s: argument %q is not the one-line import of the script", c.Name, task.Args[n-1])
				}
				script := m[1]
				if !filepath.IsAbs(filepath.FromSlash(script)) || filepath.Base(script) != unrealScriptFile ||
					strings.Contains(script, `\`) {
					t.Errorf("%s: script path %q must be the absolute, forward-slash path of %s", c.Name, script, unrealScriptFile)
				}
				for _, key := range []string{"LevelSequence", "Map", "MovieRenderConfig", "QueueAsset", "OutputDirectory"} {
					if value := c.Params[key]; value != "" && strings.Contains(strings.Join(task.Args, " "), value) {
						t.Errorf("%s: job value %s=%q appears in the command line %q", c.Name, key, value, task.Args)
					}
				}
			}
		})
	}
}

// TestPresetUnreal_SessionFolderThatBreaksExecCmdsIsRefused points the
// session folder (Capture's, like the worker's, is made under the temporary
// directory) at a folder whose path Unreal's -execcmds could not carry: a
// comma would split the command in two, and a quote would end or flip the
// Python string. The task must fail when it is resolved, naming the folder and
// worker.session_dir, rather than hand Unreal a broken command.
func TestPresetUnreal_SessionFolderThatBreaksExecCmdsIsRefused(t *testing.T) {
	names := map[string]string{"comma": "renders, farm", "quote": "o'brien"}
	if runtime.GOOS != "windows" {
		// Windows takes neither a backslash nor a double quote in a name.
		names["backslash"] = `a\b`
		names["double quote"] = `say "hi"`
	}
	for label, name := range names {
		t.Run(label, func(t *testing.T) {
			tmp := filepath.Join(t.TempDir(), name)
			if err := os.MkdirAll(tmp, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
				t.Setenv(key, tmp)
			}
			if !strings.HasPrefix(os.TempDir(), tmp) {
				t.Skipf("the temporary directory did not follow the environment: %s", os.TempDir())
			}
			for _, preset := range unrealPresets {
				_, err := captureUnreal(t, preset, unrealBaseParams(preset))
				want := "holds a comma, a quote or a backslash, which an Unreal -execcmds command cannot carry; set worker.session_dir to a folder without one"
				if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), name) {
					t.Errorf("%s with a session folder under %q: err = %v, want the refusal %q naming the folder", preset, tmp, err, want)
				}
			}
		})
	}
}

// unrealRunner stands in for the editor. It installs a recording fake of the
// unreal module, runs the task's script the way the editor would, then plays
// the events the movie pipeline would deliver to the executor's delegates, and
// records every call the script made in a JSON file, rewritten after each one
// so the record survives the script's os._exit.
//
// argv: locate, events, fail, script, record, elsewhere (an empty folder),
// bootstrap, assets. locate says how the script can find params.json, and
// ONLY that way works: "import" runs the preset's own -execcmds Python
// (bootstrap) from the empty folder, so the script is imported as a module and
// has __file__; "cwd" executes the script's text with no __file__ from its own
// folder; "none" does the same from the empty folder, so nothing finds it.
// events is a ;-separated list: "job:<1|0>:<name>" (a job's work finished,
// with or without success), "error:<1|0>:<text>" (an error, fatal or not),
// "finished:<1|0>". fail names a fake call that raises. assets maps an asset
// path to the job names of the queue saved there, or to null for an asset that
// does not exist; any other path loads as an asset with no jobs.
const unrealRunner = `import json, os, sys, types

locate, events, fail, script, record, elsewhere, bootstrap, assets = sys.argv[1:9]
assets = json.loads(assets)
calls = []
executors = []


def note(*call):
    calls.append(list(call))
    with open(record, "w", encoding="utf-8") as f:
        json.dump(calls, f)


def raising(key):
    if fail == key:
        raise RuntimeError("the fake " + key + " failed")


class Delegate(object):
    def __init__(self, name):
        self.name = name
        self.callables = []

    def add_callable(self, fn):
        note("add_callable", self.name, fn.__name__)
        self.callables.append(fn)


class MoviePipelinePIEExecutor(object):
    def __init__(self):
        note("executor")
        self.on_executor_errored_delegate = Delegate("on_executor_errored_delegate")
        self.on_executor_finished_delegate = Delegate("on_executor_finished_delegate")
        self.on_individual_job_work_finished_delegate = Delegate("on_individual_job_work_finished_delegate")
        executors.append(self)


class MoviePipelineQueueSubsystem(object):
    def get_queue(self):
        note("get_queue")
        return QUEUE

    def render_queue_with_executor_instance(self, executor):
        note("render_queue_with_executor_instance")
        raising("render")


class MoviePipelineExecutorJob(object):
    pass


class MoviePipelineOutputSetting(object):
    pass


class SoftObjectPath(object):
    def __init__(self, path):
        self.path = path

    def __str__(self):
        return self.path


class DirectoryPath(object):
    def set_editor_property(self, name, value):
        note("set_editor_property", name, value)
        self.path = value


class Setting(object):
    def __init__(self, job):
        object.__setattr__(self, "job", job)

    def __setattr__(self, name, value):
        note(name, self.job.job_name, value.path)


class Config(object):
    def __init__(self, job):
        self.job = job

    def find_or_add_setting_by_class(self, cls):
        note("find_or_add_setting_by_class", self.job.job_name, cls.__name__)
        return Setting(self.job)


class Job(object):
    def __init__(self, name=""):
        object.__setattr__(self, "job_name", name)
        object.__setattr__(self, "config", Config(self))

    def __setattr__(self, name, value):
        note("set", name, str(value))
        object.__setattr__(self, name, value)

    def set_configuration(self, config):
        note("set_configuration", config.path)
        raising("set_configuration")

    def get_configuration(self):
        return self.config


class Asset(object):
    def __init__(self, path, jobs):
        self.path = path
        self.jobs = jobs


class Queue(object):
    def __init__(self):
        self.jobs = [Job("left over")]

    def delete_all_jobs(self):
        note("delete_all_jobs")
        self.jobs = []

    def copy_from(self, asset):
        note("copy_from", asset.path)
        raising("copy_from")
        self.jobs = [Job(name) for name in asset.jobs]

    def allocate_new_job(self, cls):
        note("allocate_new_job", cls.__name__)
        job = Job()
        self.jobs.append(job)
        return job

    def get_jobs(self):
        note("get_jobs")
        return list(self.jobs)


QUEUE = Queue()


class EditorAssetLibrary(object):
    @staticmethod
    def load_asset(path):
        note("load_asset", path)
        if path in assets and assets[path] is None:
            return None
        return Asset(path, assets.get(path) or [])


class Registry(object):
    def wait_for_completion(self):
        note("wait_for_completion")


class AssetRegistryHelpers(object):
    @staticmethod
    def get_asset_registry():
        return Registry()


class SystemLibrary(object):
    @staticmethod
    def quit_editor():
        note("quit_editor")
        raising("quit_editor")


def get_editor_subsystem(cls):
    note("get_editor_subsystem", cls.__name__)
    return cls()


def log(text):
    note("log", text)


unreal = types.ModuleType("unreal")
for name, value in list(globals().items()):
    if isinstance(value, type) or name in ("get_editor_subsystem", "log"):
        setattr(unreal, name, value)
sys.modules["unreal"] = unreal

sys.argv = [""]
if locate == "import":
    os.chdir(elsewhere)
    exec(compile(bootstrap, "<execcmds>", "eval"), {"__name__": "__main__"})
else:
    os.chdir(os.path.dirname(script) if locate == "cwd" else elsewhere)
    with open(script, encoding="utf-8") as f:
        source = f.read()
    exec(compile(source, "<unreal>", "exec"), {"__name__": "__main__"})


class Output(object):
    def __init__(self, success, name):
        self.success = success
        self.job = Job(name)


for event in [e for e in events.split(";") if e]:
    note("event", event)
    kind, _, rest = event.partition(":")
    flag, _, text = rest.partition(":")
    for executor in executors:
        if kind == "job":
            for fn in executor.on_individual_job_work_finished_delegate.callables:
                fn(Output(flag == "1", text))
        elif kind == "error":
            for fn in executor.on_executor_errored_delegate.callables:
                fn(executor, None, flag == "1", text)
        elif kind == "finished":
            for fn in executor.on_executor_finished_delegate.callables:
                fn(executor, flag == "1")

with open(record, "w", encoding="utf-8") as f:
    json.dump(calls, f)
`

// unrealBootstrap is the Python inside the task's -execcmds argument, with the
// script's path pointed at script instead of the Capture's session folder.
func unrealBootstrap(t *testing.T, task presettest.TaskSnapshot, script string) string {
	t.Helper()
	m := unrealExecCmds.FindStringSubmatch(task.Args[len(task.Args)-1])
	if m == nil {
		t.Fatalf("no -execcmds import in %q", task.Args)
	}
	posix := filepath.ToSlash(script)
	if strings.ContainsAny(posix, `'",\`) {
		t.Skipf("the temporary folder %q holds a character the bootstrap cannot carry", posix)
	}
	code := strings.TrimPrefix(m[0], "-execcmds=py ")
	return strings.Replace(code, "'"+m[1]+"'", "'"+posix+"'", 1)
}

// unrealRun is one way of running the script under unrealRunner.
type unrealRun string

// unrealRuns are the script's ways of finding params.json: through the
// preset's own import of it (beside __file__), and from the current folder
// with no __file__.
var unrealRuns = []unrealRun{"import", "cwd"}

// runUnrealScript writes the task's resolved script and params.json, with the
// output directory (when set) moved into the test's folder, and runs the
// script under unrealRunner. It returns the calls the fake recorded, as
// canonical JSON, what the script wrote to stdout and its exit status.
func runUnrealScript(t *testing.T, python string, task presettest.TaskSnapshot, run unrealRun, events, fail, assets string) (calls, stdout string, exit int) {
	t.Helper()
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "task")
	elsewhere := filepath.Join(dir, "elsewhere")
	for _, d := range []string{taskDir, elsewhere} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(taskFile(t, task, "params")), &params); err != nil {
		t.Fatal(err)
	}
	if params["output_directory"] != "" {
		params["output_directory"] = filepath.Join(dir, "out dir")
	}
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(taskDir, unrealScriptFile)
	runner := filepath.Join(dir, "runner.py")
	record := filepath.Join(dir, "calls.json")
	for path, data := range map[string]string{
		script: taskFile(t, task, "render"), filepath.Join(taskDir, "params.json"): string(paramsJSON), runner: unrealRunner,
	} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bootstrap := unrealBootstrap(t, task, script)
	if assets == "" {
		assets = "{}"
	}

	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(t.Context(), python, "-I", runner, string(run), events, fail, script, record, elsewhere, bootstrap, assets)
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

// unrealNotFound folds the folders a "params.json not found" reason lists,
// which differ per run.
var unrealNotFound = regexp.MustCompile(`params\.json not found; looked at [^"]*`)

// TestPresetUnreal_ScriptDrivesUnreal runs the shipped script, as resolved for
// a real task, under a real Python with the unreal module faked, and pins the
// calls it makes: the editor's queue emptied and filled with the job the
// parameters describe (or a copy of the saved queue's jobs), the output
// directory applied to every job when one is given, the PIE executor started
// with its three delegates; then, as the movie pipeline reports back, one
// verdict, the success line only when every job succeeded and nothing fatal
// was reported, and quit_editor. A render that never reports gets no verdict
// and no quit (which is why the readme asks for a step timeout). Each case
// runs through the preset's own -execcmds import and through the current
// folder, so a way of finding params.json that does not work cannot hide
// behind one that does.
func TestPresetUnreal_ScriptDrivesUnreal(t *testing.T) {
	python := findPython3(t)
	if python == "" {
		t.Skip("no working Python 3 on PATH (tried python3, python)")
	}
	job, queue := unrealPresets[0], unrealPresets[1]
	const (
		seq    = "/Game/Cinematics/Hero/SEQ_Hero"
		mapA   = "/Game/Maps/Hero"
		cfg    = "/Game/Cinematics/MRQ_Final"
		queueA = "/Game/Cinematics/MRQ_HeroQueue"
	)
	quotedSeq := `/Game/Cinematics/it's the "hero" \ seq`
	jobParams := func(sequence, output string) map[string]string {
		p := map[string]string{"SceneFile": `C:\Projects\HeroFilm\HeroFilm.uproject`, "LevelSequence": sequence, "Map": mapA, "MovieRenderConfig": cfg}
		if output != "" {
			p["OutputDirectory"] = output
		}
		return p
	}
	queueParams := func(output string) map[string]string {
		p := map[string]string{"SceneFile": `C:\Projects\HeroFilm\HeroFilm.uproject`, "QueueAsset": queueA}
		if output != "" {
			p["OutputDirectory"] = output
		}
		return p
	}

	type call = []any
	start := []call{{"wait_for_completion"}, {"get_editor_subsystem", "MoviePipelineQueueSubsystem"}, {"get_queue"}, {"delete_all_jobs"}}
	built := func(sequence, name string) []call {
		return append(append([]call{}, start...),
			call{"load_asset", cfg}, call{"allocate_new_job", "MoviePipelineExecutorJob"},
			call{"set", "sequence", sequence}, call{"set", "map", mapA}, call{"set_configuration", cfg},
			call{"set", "job_name", name}, call{"get_jobs"})
	}
	copied := append(append([]call{}, start...), call{"load_asset", queueA}, call{"copy_from", queueA}, call{"get_jobs"})
	// output is filled in per run: the test moves the directory into its folder.
	const outputMarker = "OUTPUT_DIRECTORY"
	output := func(names ...string) []call {
		out := []call{{"set_editor_property", "path", outputMarker}}
		for _, n := range names {
			out = append(out, call{"find_or_add_setting_by_class", n, "MoviePipelineOutputSetting"}, call{"output_directory", n, outputMarker})
		}
		return out
	}
	started := []call{
		{"executor"},
		{"add_callable", "on_executor_errored_delegate", "on_errored"},
		{"add_callable", "on_executor_finished_delegate", "on_finished"},
		{"add_callable", "on_individual_job_work_finished_delegate", "on_job_finished"},
		{"render_queue_with_executor_instance"},
	}
	events := func(evs ...string) []call {
		out := make([]call, len(evs))
		for i, e := range evs {
			out[i] = call{"event", e}
		}
		return out
	}
	ok := []call{{"log", "SQI_RENDER: OK"}, {"quit_editor"}}
	failed := func(reason string) []call { return []call{{"log", "SQI_RENDER: FAILED: " + reason}, {"quit_editor"}} }
	join := func(parts ...[]call) []call {
		var out []call
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	twoJobs := `{"/Game/Cinematics/MRQ_HeroQueue": ["Hero A", "Hero B"]}`

	tests := []struct {
		name     string
		preset   string
		params   map[string]string
		assets   string
		events   []string
		fail     string
		runs     []unrealRun
		want     []call
		wantOK   bool
		wantFail string
		wantExit int
	}{
		{
			name: "job renders and quits", preset: job, params: jobParams(seq, `C:\Renders\HeroFilm`),
			events: []string{"job:1:SEQ_Hero", "finished:1"},
			want:   join(built(seq, "SEQ_Hero"), output("SEQ_Hero"), started, events("job:1:SEQ_Hero", "finished:1"), ok),
			wantOK: true,
		},
		{
			name: "blank output keeps the config output", preset: job, params: jobParams(seq, ""),
			events: []string{"finished:1"},
			want:   join(built(seq, "SEQ_Hero"), started, events("finished:1"), ok),
			wantOK: true,
		},
		{
			name: "sequence with quotes and a backslash", preset: job, params: jobParams(quotedSeq, ""),
			events: []string{"finished:1"},
			want:   join(built(quotedSeq, `it's the "hero" \ seq`), started, events("finished:1"), ok),
			wantOK: true,
		},
		{
			name: "queue renders every job to the output directory", preset: queue, params: queueParams(`C:\Renders\Queue`),
			assets: twoJobs, events: []string{"job:1:Hero A", "job:1:Hero B", "finished:1"},
			want:   join(copied, output("Hero A", "Hero B"), started, events("job:1:Hero A", "job:1:Hero B", "finished:1"), ok),
			wantOK: true,
		},
		{
			name: "a failed job fails the queue", preset: queue, params: queueParams(""),
			assets: twoJobs, events: []string{"job:1:Hero A", "job:0:Hero B", "finished:1"},
			want: join(copied, started, events("job:1:Hero A", "job:0:Hero B", "finished:1"),
				failed("job Hero B did not succeed")),
			wantFail: "job Hero B did not succeed", wantExit: 0,
		},
		{
			name: "an unsuccessful finish fails", preset: job, params: jobParams(seq, ""),
			events: []string{"finished:0"},
			want: join(built(seq, "SEQ_Hero"), started, events("finished:0"),
				failed("Movie Render Queue reported that the render did not succeed")),
			wantFail: "Movie Render Queue reported that the render did not succeed",
		},
		{
			name: "a fatal error ends the render once", preset: job, params: jobParams(seq, ""),
			events: []string{"error:1:the map failed to load", "finished:0"},
			want: join(built(seq, "SEQ_Hero"), started, events("error:1:the map failed to load"),
				failed("Movie Render Queue error: the map failed to load"), events("finished:0")),
			wantFail: "Movie Render Queue error: the map failed to load",
		},
		{
			name: "a non-fatal error does not fail the render", preset: job, params: jobParams(seq, ""),
			events: []string{"error:0:a texture streamed late", "finished:1"},
			want: join(built(seq, "SEQ_Hero"), started, events("error:0:a texture streamed late"),
				[]call{{"log", "sqi: Movie Render Queue reported a non-fatal error: a texture streamed late"}},
				events("finished:1"), ok),
			wantOK: true,
		},
		{
			name: "a render that never reports gets no verdict", preset: job, params: jobParams(seq, ""),
			want: join(built(seq, "SEQ_Hero"), started),
		},
		{
			name: "a missing config is refused", preset: job, params: jobParams(seq, ""),
			assets: `{"/Game/Cinematics/MRQ_Final": null}`,
			want: join(start, []call{{"load_asset", cfg}},
				failed("ValueError: no Movie Render Queue config asset at /Game/Cinematics/MRQ_Final")),
			wantFail: "ValueError: no Movie Render Queue config asset at /Game/Cinematics/MRQ_Final",
		},
		{
			name: "a missing queue is refused", preset: queue, params: queueParams(""),
			assets: `{"/Game/Cinematics/MRQ_HeroQueue": null}`,
			want: join(start, []call{{"load_asset", queueA}},
				failed("ValueError: no Movie Pipeline Queue asset at /Game/Cinematics/MRQ_HeroQueue")),
			wantFail: "ValueError: no Movie Pipeline Queue asset at /Game/Cinematics/MRQ_HeroQueue",
		},
		{
			name: "an empty queue is refused", preset: queue, params: queueParams(""),
			want:     join(copied, failed("ValueError: the queue holds no jobs")),
			wantFail: "ValueError: the queue holds no jobs",
		},
		{
			name: "a config Unreal rejects is refused", preset: job, params: jobParams(seq, ""),
			fail: "set_configuration",
			want: join(start, []call{
				{"load_asset", cfg},
				{"allocate_new_job", "MoviePipelineExecutorJob"},
				{"set", "sequence", seq},
				{"set", "map", mapA},
				{"set_configuration", cfg},
			}, failed("RuntimeError: the fake set_configuration failed")),
			wantFail: "RuntimeError: the fake set_configuration failed",
		},
		{
			name: "a render that cannot start fails", preset: job, params: jobParams(seq, ""),
			fail: "render", events: []string{"finished:0"},
			want:     join(built(seq, "SEQ_Hero"), started, failed("RuntimeError: the fake render failed"), events("finished:0")),
			wantFail: "RuntimeError: the fake render failed",
		},
		{
			name: "quit failing after a render exits 0", preset: job, params: jobParams(seq, ""),
			fail: "quit_editor", events: []string{"finished:1"},
			want:   join(built(seq, "SEQ_Hero"), started, events("finished:1"), ok),
			wantOK: true, wantExit: 0,
		},
		{
			name: "quit failing after an error exits 1", preset: job, params: jobParams(seq, ""),
			fail: "quit_editor", events: []string{"finished:0"},
			want: join(built(seq, "SEQ_Hero"), started, events("finished:0"),
				failed("Movie Render Queue reported that the render did not succeed")),
			wantFail: "Movie Render Queue reported that the render did not succeed", wantExit: 1,
		},
		{
			name: "params json is nowhere to be found", preset: job, params: jobParams(seq, ""),
			runs:     []unrealRun{"none"},
			want:     failed("OSError: params.json not found; looked at ..."),
			wantFail: "OSError: params.json not found; looked at",
		},
	}
	for _, tc := range tests {
		snap, err := captureUnreal(t, tc.preset, tc.params)
		if err != nil {
			t.Fatalf("%s: Capture: %v", tc.name, err)
		}
		task := snap.Steps[0].Tasks[0]
		runs := tc.runs
		if runs == nil {
			runs = unrealRuns
		}
		for _, run := range runs {
			t.Run(tc.name+"/"+string(run), func(t *testing.T) {
				calls, stdout, exit := runUnrealScript(t, python, task, run, strings.Join(tc.events, ";"), tc.fail, tc.assets)
				calls = unrealNotFound.ReplaceAllString(calls, "params.json not found; looked at ...")
				wantJSON, err := json.Marshal(tc.want)
				if err != nil {
					t.Fatal(err)
				}
				want := canonicalJSON(t, wantJSON)
				// The output directory the test moved into its own folder, as
				// the script hands it to Unreal: with forward slashes.
				if got := unrealCallArg(calls, "set_editor_property"); got != "" {
					want = strings.ReplaceAll(want, outputMarker, got)
					if !strings.HasSuffix(got, "/out dir") || strings.Contains(got, `\`) {
						t.Errorf("output directory handed to Unreal = %q, want the moved directory in forward slashes", got)
					}
				}
				if calls != want {
					t.Errorf("Unreal calls:\n  got  %s\n  want %s", calls, want)
				}
				if exit != tc.wantExit {
					t.Errorf("exit status = %d, want %d", exit, tc.wantExit)
				}
				if gotOK := strings.Contains(stdout, "SQI_RENDER: OK\n"); gotOK != tc.wantOK {
					t.Errorf("success line printed = %v, want %v; stdout:\n%s", gotOK, tc.wantOK, stdout)
				}
				if tc.wantFail == "" {
					if strings.Contains(stdout, "SQI_RENDER: FAILED:") || strings.Contains(stdout, "openjd_fail:") {
						t.Errorf("failure line printed without a failure; stdout:\n%s", stdout)
					}
					return
				}
				for _, line := range []string{"SQI_RENDER: FAILED: " + tc.wantFail, "openjd_fail: " + tc.wantFail} {
					if strings.Count(stdout, line) != 1 {
						t.Errorf("stdout must hold %q exactly once; stdout:\n%s", line, stdout)
					}
				}
			})
		}
	}
}

// unrealCallArg returns the value of the first recorded call named name (its
// last argument), or "" when there is none.
func unrealCallArg(calls, name string) string {
	var list [][]any
	if err := json.Unmarshal([]byte(calls), &list); err != nil {
		return ""
	}
	for _, c := range list {
		if len(c) > 0 && c[0] == name {
			if s, ok := c[len(c)-1].(string); ok {
				return s
			}
		}
	}
	return ""
}
