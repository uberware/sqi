// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

// preset_dcc_test.go — executes the Blender, Nuke and Houdini reference
// presets through a real sqi-server and a real sqi-worker against the REAL
// applications, then asserts on the files they render.
//
// Why this file exists. The argv goldens (Tier 1) prove a preset still expands
// to the command line someone reviewed, and the recording stub (Tier 3) proves
// sqi delivers that command line. Neither can show that the vendor application
// accepts it. These cases do, and each fixed a question the registry's caveats
// had to leave open: whether Blender pads the "####" in -o and appends its own
// extension, whether Nuke reads "-F 1-1" as the single frame 1, whether a view
// layer name containing quotes survives to Blender, and whether the hython
// driver renders exactly the frames of a stepped or listed range.
//
// Each case builds its own scene with the application (testdata/dcc/*.py), so no
// binary fixture lives in the repository.
//
// NOT tagged `integration`, like preset_ffmpeg_test.go, so the CI `test` job
// compiles, lints and runs it.
//
// They skip when the application is absent, which means A LOCAL PASS PROVES
// NOTHING ON ITS OWN. The registry (presets/validation-tiers.yaml) records each
// case with requires_app, and SQI_TEST_REQUIRE_APPS=blender,nuke,houdini (what
// `make test-dcc` sets) turns a skip into a failure. Look for the
// "--- PASS: TestPresetDCC" lines.
//
// "Installed" means what the worker means by it: the command is on PATH, or the
// built-in detector for that application finds an install
// (internal/worker/capabilities), so these tests run wherever a worker would
// advertise the tag. Nuke needs a license flag the detector does not supply:
// SQI_TEST_NUKE_ARGS (default "--nc", for the free Non-commercial license; set
// it empty for a commercial or Indie license).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/worker/capabilities"
)

// dccJobTimeout covers worker registration plus the render. Blender renders a
// frame of the test scene in about a second, but each task pays the
// application's start-up cost, and Houdini's is the largest.
const dccJobTimeout = 300 * time.Second

// ── Locating the application ──────────────────────────────────────────────────

// locateDCC finds the executable a worker would run for command: PATH first
// (the worker never shadows a command the host provides), then the built-in
// detector's default.
func locateDCC(t *testing.T, command string) (string, bool) {
	t.Helper()
	if p, err := exec.LookPath(command); err == nil {
		return p, true
	}
	found, err := capabilities.DetectCommands(capabilities.CapabilitiesConfig{}, capabilities.OSCheckEnv())
	if err != nil {
		t.Fatalf("DetectCommands: %v", err)
	}
	for _, c := range found {
		if c.Name == command {
			return c.Path, true
		}
	}
	return "", false
}

// requireDCC returns the executable for command, or records and skips with a
// reason that names how to get the application.
func requireDCC(t *testing.T, caseName, app, command string) string {
	t.Helper()
	exe, ok := locateDCC(t, command)
	if !ok {
		skipOutcome(t, caseName, fmt.Sprintf(
			"%s is not installed: no %q on PATH and none found by the worker's detector. "+
				"Install it, or put %q on PATH, to run this case", app, command, command,
		))
	}
	return exe
}

// nukeArgs are the flags every Nuke command needs for the installed license.
func nukeArgs() []string {
	if v, ok := os.LookupEnv("SQI_TEST_NUKE_ARGS"); ok {
		return strings.Fields(v)
	}
	return []string{"--nc"}
}

// dccTestdata resolves a file under testdata/dcc.
func dccTestdata(t *testing.T, name string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) returned no file path")
	}
	return filepath.Join(filepath.Dir(thisFile), "testdata", "dcc", name)
}

// runDCC runs an application and returns its combined output, failing the test
// on a non-zero exit.
func runDCC(t *testing.T, exe string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", exe, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// savedPath returns the path a scene script reported with its "SAVED" line.
func savedPath(t *testing.T, output string) string {
	t.Helper()
	for line := range strings.SplitSeq(output, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "SAVED "); ok {
			return rest
		}
	}
	t.Fatalf("scene script printed no SAVED line:\n%s", output)
	return ""
}

// ── Running a job ─────────────────────────────────────────────────────────────

// runDCCJob submits a preset and waits for it to complete, returning the job ID.
func runDCCJob(t *testing.T, ts *testServer, farmID, queueID, preset string, params map[string]string) string {
	t.Helper()
	jobID := submitPreset(t, ts, farmID, queueID, preset, params)
	got := pollJobStatus(t, ts, jobID, []string{"completed", "failed", "canceled"}, dccJobTimeout)
	if got != "completed" {
		t.Fatalf("preset %s: job status = %q, want completed%s", preset, got, reportFailedTasks(t, ts, jobID))
	}
	return jobID
}

// jobTaskCount returns how many tasks a job fanned out into.
func jobTaskCount(t *testing.T, ts *testServer, jobID string) int {
	t.Helper()
	var resp struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	mustDoJSON(t, http.MethodGet, "http://"+ts.HTTPAddr+"/api/v1/jobs/"+jobID+"/tasks",
		nil, "", http.StatusOK, &resp)
	return len(resp.Items)
}

// assertTaskCount fails unless the job fanned out into want tasks.
func assertTaskCount(t *testing.T, ts *testServer, jobID string, want int) {
	t.Helper()
	if got := jobTaskCount(t, ts, jobID); got != want {
		t.Errorf("job ran %d tasks, want %d", got, want)
	}
}

// assertExactFiles fails unless dir holds exactly the named files, each
// non-empty. "Exactly" is the point: a stepped range that rendered an extra
// frame, or a Write node that should not have run, shows up as a stray file.
func assertExactFiles(t *testing.T, dir string, want []string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read output dir: %v", err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
		if info, err := e.Info(); err == nil && info.Size() == 0 {
			t.Errorf("%s is empty", e.Name())
		}
	}
	slices.Sort(got)
	want = slices.Clone(want)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("rendered files = %v, want %v", got, want)
	}
}

// frameNames expands a printf-style pattern for each frame.
func frameNames(pattern string, frames ...int) []string {
	var names []string
	for _, f := range frames {
		names = append(names, fmt.Sprintf(pattern, f))
	}
	return names
}

// seq returns lo..hi inclusive.
func seq(lo, hi int) []int {
	var out []int
	for i := lo; i <= hi; i++ {
		out = append(out, i)
	}
	return out
}

// ── Blender ───────────────────────────────────────────────────────────────────

// blenderScene builds the test scene and returns the .blend path.
func blenderScene(t *testing.T, exe string) string {
	t.Helper()
	dir := t.TempDir()
	out := runDCC(t, exe, "-b", "--factory-startup", "--python-exit-code", "1",
		"--python", dccTestdata(t, "blender_scene.py"), "--",
		filepath.Join(dir, "scene with spaces"), filepath.Join(dir, "unused"))
	return savedPath(t, out)
}

var exrLayerStamp = regexp.MustCompile(`cycles\.([^\x00]+?)\.render_time\x00`)

// exrRenderedLayers returns the view layers Cycles stamped into an EXR header.
// Blender 5.2 writes only the first layer's pixels into a multilayer EXR even
// when every layer rendered, so the channel list cannot tell layers apart;
// Cycles stamps cycles.<layer>.render_time for each layer it rendered.
func exrRenderedLayers(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.HasPrefix(data, []byte{0x76, 0x2f, 0x31, 0x01}) {
		t.Fatalf("%s is not an OpenEXR file", path)
	}
	head := data[:min(len(data), 20000)]
	seen := map[string]bool{}
	var layers []string
	for _, m := range exrLayerStamp.FindAllSubmatch(head, -1) {
		if name := string(m[1]); !seen[name] {
			seen[name] = true
			layers = append(layers, name)
		}
	}
	slices.Sort(layers)
	return layers
}

// TestPresetDCC_Blender_BatchRender runs blender-batch-render under a real
// Blender. It answers what the argv goldens could not: "####" in -o is padded to
// the frame number, Blender appends the format's extension itself, a path with
// no "#" still gets the frame number, and a stepped range renders exactly its
// frames.
func TestPresetDCC_Blender_BatchRender(t *testing.T) {
	const caseName = "TestPresetDCC_Blender_BatchRender"
	trackOutcome(t, caseName)
	exe := requireDCC(t, caseName, "Blender", "blender")
	scene := blenderScene(t, exe)

	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	startRealWorkerAnyOS(t, ts, farmID, queueID)

	t.Run("padded output, one frame per task", func(t *testing.T) {
		out := t.TempDir()
		job := runDCCJob(t, ts, farmID, queueID, "blender-batch-render", map[string]string{
			"SceneFile": scene, "Frames": "1-4", "OutputPath": filepath.ToSlash(filepath.Join(out, "batch_####")),
		})
		assertTaskCount(t, ts, job, 4)
		assertExactFiles(t, out, frameNames("batch_%04d.exr", 1, 2, 3, 4))
	})

	t.Run("stepped range renders only its frames", func(t *testing.T) {
		out := t.TempDir()
		job := runDCCJob(t, ts, farmID, queueID, "blender-batch-render", map[string]string{
			"SceneFile": scene, "Frames": "1-4:3", "OutputPath": filepath.ToSlash(filepath.Join(out, "nopad")),
		})
		assertTaskCount(t, ts, job, 2)
		assertExactFiles(t, out, frameNames("nopad%04d.exr", 1, 4))
	})
}

// runDCCJobExpectingFailure submits a preset and waits for the job to fail,
// returning the failed tasks' report (their failure reasons and log tails).
func runDCCJobExpectingFailure(t *testing.T, ts *testServer, farmID, queueID, preset string, params map[string]string) string {
	t.Helper()
	jobID := submitPreset(t, ts, farmID, queueID, preset, params)
	got := pollJobStatus(t, ts, jobID, []string{"completed", "failed", "canceled"}, dccJobTimeout)
	if got != "failed" {
		t.Fatalf("preset %s: job status = %q, want failed", preset, got)
	}
	return reportFailedTasks(t, ts, jobID)
}

// TestPresetDCC_Blender_ViewLayerRender runs blender-view-layer-render under a
// real Blender. It answers what the argv goldens could not: -S selects the scene
// the one-line Python expression then reads, the layer name reaches Blender as
// a single argument even with quotes in it, `use` makes -a render only that
// layer, and a layer the scene lacks stops Blender with a non-zero status
// before anything renders.
func TestPresetDCC_Blender_ViewLayerRender(t *testing.T) {
	const caseName = "TestPresetDCC_Blender_ViewLayerRender"
	trackOutcome(t, caseName)
	exe := requireDCC(t, caseName, "Blender", "blender")
	scene := blenderScene(t, exe)

	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	startRealWorkerAnyOS(t, ts, farmID, queueID)

	params := func(out, sceneName, layer string) map[string]string {
		return map[string]string{
			"SceneFile": scene, "Frames": "1-2", "OutputPath": filepath.ToSlash(filepath.Join(out, "vl_####")),
			"Scene": sceneName, "ViewLayer": layer,
		}
	}
	renders := []struct{ scene, layer string }{
		{"Shot", "Bg 'quoted' layer"},
		{"Shot", "Fg"},
		{"Alt", "AltLayer"},
	}
	for _, tc := range renders {
		t.Run(tc.scene+"/"+tc.layer, func(t *testing.T) {
			out := t.TempDir()
			job := runDCCJob(t, ts, farmID, queueID, "blender-view-layer-render", params(out, tc.scene, tc.layer))
			assertTaskCount(t, ts, job, 2)
			assertExactFiles(t, out, frameNames("vl_%04d.exr", 1, 2))
			for _, f := range frameNames("vl_%04d.exr", 1, 2) {
				if got := exrRenderedLayers(t, filepath.Join(out, f)); !slices.Equal(got, []string{tc.layer}) {
					t.Errorf("%s rendered layers = %q, want only %q", f, got, tc.layer)
				}
			}
		})
	}

	// A layer the selected scene lacks must fail the job and render nothing. Fg
	// exists in Shot but not in Alt, so this also shows -S chose the scene. A
	// scene the file lacks must fail too: -S only prints "Can't find scene" and
	// carries on with the file's active scene (Shot, which has Fg), so without
	// the expression's own check that job would render the wrong scene and pass.
	for _, tc := range []struct{ name, scene, layer, want string }{
		{"no such layer", "Shot", "no such layer", "no view layer named 'no such layer' in scene 'Shot'"},
		{"layer of another scene", "Alt", "Fg", "no view layer named 'Fg' in scene 'Alt'"},
		{"no such scene", "Nope", "Fg", "no scene named 'Nope' in the file"},
	} {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			out := t.TempDir()
			report := runDCCJobExpectingFailure(t, ts, farmID, queueID, "blender-view-layer-render",
				params(out, tc.scene, tc.layer))
			if !strings.Contains(report, tc.want) {
				t.Errorf("failure report does not say %q:%s", tc.want, report)
			}
			assertExactFiles(t, out, nil)
		})
	}
}

// ── Nuke ──────────────────────────────────────────────────────────────────────

// nukeWorker starts a real worker whose nuke command carries the license flags:
// the detector supplies an executable but no arguments, because they depend on
// the license and belong to the operator.
func nukeWorker(t *testing.T, ts *testServer, farmID, queueID, exe string) {
	t.Helper()
	cfg := map[string]any{"worker": map[string]any{"commands": map[string]any{
		"nuke": map[string]any{"path": exe, "args": nukeArgs()},
	}}}
	data, err := json.Marshal(cfg) // JSON is valid YAML, and quotes a Windows path safely
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "worker.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	startRealWorkerCore(t, ts, farmID, queueID, []string{"--config", path}, nil, true)
}

// nukeScene builds the test script and returns its path.
func nukeScene(t *testing.T, exe, renders string) string {
	t.Helper()
	args := append(nukeArgs(), "-t", dccTestdata(t, "nuke_scene.py"),
		filepath.Join(t.TempDir(), "test shot"), filepath.ToSlash(renders))
	return savedPath(t, runDCC(t, exe, args...))
}

// assertPNGs checks each file decodes as a PNG of the test script's size.
func assertPNGs(t *testing.T, dir string, names []string) {
	t.Helper()
	for _, name := range names {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("open %s: %v", name, err)
			continue
		}
		cfg, err := png.DecodeConfig(f)
		_ = f.Close()
		if err != nil {
			t.Errorf("%s is not a PNG: %v", name, err)
		} else if cfg.Width != 320 || cfg.Height != 180 {
			t.Errorf("%s is %dx%d, want 320x180", name, cfg.Width, cfg.Height)
		}
	}
}

// TestPresetDCC_Nuke_WriteRender runs nuke-write-render under a real Nuke. A
// stepped range expands to one single-frame task per requested frame ("-F 1-1",
// "-F 4-4") because a CONTIGUOUS chunk never spans a gap; this is the case that
// shows Nuke reads "-F 1-1" as the single frame 1, and that -X renders only the
// named Write node.
func TestPresetDCC_Nuke_WriteRender(t *testing.T) {
	const caseName = "TestPresetDCC_Nuke_WriteRender"
	trackOutcome(t, caseName)
	exe := requireDCC(t, caseName, "Nuke", "nuke")

	renders := t.TempDir()
	script := nukeScene(t, exe, renders)
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	nukeWorker(t, ts, farmID, queueID, exe)

	job := runDCCJob(t, ts, farmID, queueID, "nuke-write-render", map[string]string{
		"SceneFile": script, "Frames": "1-4:3", "WriteNode": "Write2",
	})
	assertTaskCount(t, ts, job, 2)
	want := frameNames("wr2.%04d.png", 1, 4)
	assertExactFiles(t, renders, want)
	assertPNGs(t, renders, want)
}

// TestPresetDCC_Nuke_ScriptRender runs nuke-script-render under a real Nuke: with
// no -X, every enabled Write node renders every requested frame.
func TestPresetDCC_Nuke_ScriptRender(t *testing.T) {
	const caseName = "TestPresetDCC_Nuke_ScriptRender"
	trackOutcome(t, caseName)
	exe := requireDCC(t, caseName, "Nuke", "nuke")

	renders := t.TempDir()
	script := nukeScene(t, exe, renders)
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	nukeWorker(t, ts, farmID, queueID, exe)

	runDCCJob(t, ts, farmID, queueID, "nuke-script-render", map[string]string{
		"SceneFile": script, "Frames": "1-4",
	})
	want := append(frameNames("wr1.%04d.png", seq(1, 4)...), frameNames("wr2.%04d.png", seq(1, 4)...)...)
	assertExactFiles(t, renders, want)
	assertPNGs(t, renders, want)
}

// ── Houdini ───────────────────────────────────────────────────────────────────

// hythonOutputs reads each file back through hython and returns its box center,
// which the scene makes equal to the frame number.
func hythonOutputs(t *testing.T, hython, dir string, names []string) map[string]float64 {
	t.Helper()
	args := []string{dccTestdata(t, "houdini_check.py")}
	for _, n := range names {
		args = append(args, filepath.Join(dir, n))
	}
	got := map[string]float64{}
	for line := range strings.SplitSeq(runDCC(t, hython, args...), "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && strings.HasPrefix(f[0], "box.") {
			x, err := strconv.ParseFloat(f[1], 64)
			if err != nil {
				t.Fatalf("bad center in %q: %v", line, err)
			}
			got[f[0]] = x
		}
	}
	return got
}

// TestPresetDCC_Houdini_ROPRender runs houdini-rop-render under a real hython.
// The embedded driver turns a NONCONTIGUOUS chunk into one rop.render() call per
// run of frames, so the files on disk are the proof that a stepped range
// ("1-12:2") and a listed one ("1-3,7,8") render exactly their frames, and that
// a range longer than a chunk fans out and still covers every frame. Each file
// is read back, because a ROP can write a file for the wrong frame.
func TestPresetDCC_Houdini_ROPRender(t *testing.T) {
	const caseName = "TestPresetDCC_Houdini_ROPRender"
	trackOutcome(t, caseName)
	hython := requireDCC(t, caseName, "Houdini", "hython")

	dir := t.TempDir()
	out := filepath.ToSlash(filepath.Join(dir, "geo"))
	hip := savedPath(t, runDCC(t, hython, dccTestdata(t, "houdini_scene.py"),
		filepath.Join(dir, "test shot"), out))

	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	startRealWorkerAnyOS(t, ts, farmID, queueID)

	cases := []struct {
		name, frames string
		wantFrames   []int
		wantTasks    int
	}{
		{"stepped range", "1-12:2", []int{1, 3, 5, 7, 9, 11}, 1},
		{"listed range", "1-3,7,8", []int{1, 2, 3, 7, 8}, 1},
		{"more frames than one chunk", "1-24", seq(1, 24), 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.RemoveAll(out); err != nil {
				t.Fatal(err)
			}
			job := runDCCJob(t, ts, farmID, queueID, "houdini-rop-render", map[string]string{
				"SceneFile": hip, "Frames": tc.frames, "RopPath": "/out/geo1",
			})
			assertTaskCount(t, ts, job, tc.wantTasks)
			want := frameNames("box.%04d.bgeo.sc", tc.wantFrames...)
			assertExactFiles(t, out, want)
			centers := hythonOutputs(t, hython, out, want)
			for i, name := range want {
				if got, ok := centers[name]; !ok || got != float64(tc.wantFrames[i]) {
					t.Errorf("%s: box center x = %v (read back: %v), want %d", name, got, ok, tc.wantFrames[i])
				}
			}
		})
	}
}

// TestPresetDCC_Houdini_HythonScript runs hython-script under a real hython. The
// script each task runs writes one JSON file per frame of its chunk holding
// hou.frame(), the hip's name and SQI_FRAME_START, so the files on disk show
// that the launcher loaded the hip, that hou.frame() starts at the task's first
// frame, and that every frame of a range is covered by exactly one task. A
// script that raises must fail the job: that is the property the launcher
// relies on hython for.
func TestPresetDCC_Houdini_HythonScript(t *testing.T) {
	const caseName = "TestPresetDCC_Houdini_HythonScript"
	trackOutcome(t, caseName)
	hython := requireDCC(t, caseName, "Houdini", "hython")

	dir := t.TempDir()
	hip := savedPath(t, runDCC(t, hython, dccTestdata(t, "houdini_scene.py"),
		filepath.Join(dir, "test shot"), filepath.ToSlash(filepath.Join(dir, "geo"))))

	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	startRealWorkerAnyOS(t, ts, farmID, queueID)

	// scriptFor builds a script body that records, for each frame of the task's
	// chunk, what hou saw. The output directory is embedded with %q, which is a
	// valid Python string literal for any path.
	scriptFor := func(out string) string {
		return fmt.Sprintf(`import json, os
import hou

for f in range(int(os.environ["SQI_FRAME_START"]), int(os.environ["SQI_FRAME_END"]) + 1):
    with open(os.path.join(%q, "frame.%%d.json" %% f), "w") as fh:
        json.dump({
            "hou_frame": hou.frame(),
            "hip": hou.hipFile.basename(),
            "start": os.environ["SQI_FRAME_START"],
        }, fh)
`, filepath.ToSlash(out))
	}
	type frameFile struct {
		HouFrame float64 `json:"hou_frame"`
		Hip      string  `json:"hip"`
		Start    string  `json:"start"`
	}
	// readFrames reads back the file for each frame, keyed by frame.
	readFrames := func(t *testing.T, out string, frames []int) map[int]frameFile {
		t.Helper()
		got := map[int]frameFile{}
		for _, f := range frames {
			data, err := os.ReadFile(filepath.Join(out, fmt.Sprintf("frame.%d.json", f)))
			if err != nil {
				t.Fatalf("frame %d: %v", f, err)
			}
			var ff frameFile
			if err := json.Unmarshal(data, &ff); err != nil {
				t.Fatalf("frame %d: %v", f, err)
			}
			got[f] = ff
		}
		return got
	}
	wantHip := filepath.Base(hip)

	t.Run("one frame per task", func(t *testing.T) {
		out := t.TempDir()
		job := runDCCJob(t, ts, farmID, queueID, "hython-script", map[string]string{
			"SceneFile": hip, "Script": scriptFor(out), "Frames": "1-3", "FramesPerTask": "1",
		})
		assertTaskCount(t, ts, job, 3)
		assertExactFiles(t, out, frameNames("frame.%d.json", 1, 2, 3))
		for f, ff := range readFrames(t, out, seq(1, 3)) {
			if ff.HouFrame != float64(f) || ff.Hip != wantHip || ff.Start != strconv.Itoa(f) {
				t.Errorf("frame %d: saw %+v, want hou_frame %d, hip %q, start %d", f, ff, f, wantHip, f)
			}
		}
	})

	t.Run("chunks", func(t *testing.T) {
		out := t.TempDir()
		job := runDCCJob(t, ts, farmID, queueID, "hython-script", map[string]string{
			"SceneFile": hip, "Script": scriptFor(out), "Frames": "1-10", "FramesPerTask": "4",
		})
		assertTaskCount(t, ts, job, 3)
		assertExactFiles(t, out, frameNames("frame.%d.json", seq(1, 10)...))
		// 1-10 at 4 balances into 1-4, 5-7 and 8-10; hou.frame() is the first
		// frame of the chunk, whatever frame the script is on.
		chunkStart := func(f int) int {
			switch {
			case f <= 4:
				return 1
			case f <= 7:
				return 5
			default:
				return 8
			}
		}
		for f, ff := range readFrames(t, out, seq(1, 10)) {
			if want := chunkStart(f); ff.HouFrame != float64(want) || ff.Start != strconv.Itoa(want) || ff.Hip != wantHip {
				t.Errorf("frame %d: saw %+v, want hou_frame and start %d, hip %q", f, ff, want, wantHip)
			}
		}
	})

	t.Run("script error fails the job", func(t *testing.T) {
		report := runDCCJobExpectingFailure(t, ts, farmID, queueID, "hython-script", map[string]string{
			"SceneFile": hip, "Script": `raise RuntimeError("boom")`, "Frames": "1", "FramesPerTask": "1",
		})
		if !strings.Contains(report, "boom") {
			t.Errorf("failed task report does not show the script's exception:\n%s", report)
		}
	})
}

// TestPresetDCC_Houdini_USDRecord runs usdrecord-render under a real hython,
// which runs Houdini's usdrecord on a 20-frame stage. The stage is copied to a
// directory with a space in its name: that is what Houdini's own usdrecord.cmd
// wrapper cannot launch, and the reason the preset runs hython directly. The
// frame images are read back, so the files show that the `#` run is the frame
// padding, that each task covers exactly its frames, that the renderer name
// "Karma CPU" reaches usdrecord as one argument, that --imageWidth is
// applied, and that a range below zero is accepted.
func TestPresetDCC_Houdini_USDRecord(t *testing.T) {
	const caseName = "TestPresetDCC_Houdini_USDRecord"
	trackOutcome(t, caseName)
	requireDCC(t, caseName, "Houdini", "hython")

	dir := filepath.Join(t.TempDir(), "test shot")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(dir, "stage.usda")
	data, err := os.ReadFile(dccTestdata(t, "stage.usda"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stage, data, 0o644); err != nil {
		t.Fatal(err)
	}

	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	startRealWorkerAnyOS(t, ts, farmID, queueID)

	// assertPNGWidths checks each file decodes as a PNG of the given width.
	assertPNGWidths := func(t *testing.T, out string, names []string, width int) {
		t.Helper()
		for _, name := range names {
			f, err := os.Open(filepath.Join(out, name))
			if err != nil {
				t.Errorf("open %s: %v", name, err)
				continue
			}
			cfg, err := png.DecodeConfig(f)
			_ = f.Close()
			if err != nil {
				t.Errorf("%s is not a PNG: %v", name, err)
			} else if cfg.Width != width {
				t.Errorf("%s is %d pixels wide, want %d", name, cfg.Width, width)
			}
		}
	}

	t.Run("storm", func(t *testing.T) {
		out := filepath.Join(dir, "out", "review")
		job := runDCCJob(t, ts, farmID, queueID, "usdrecord-render", map[string]string{
			"SceneFile": stage, "OutputFile": filepath.Join(out, "preview.####.png"),
			"Frames": "1-12", "FramesPerTask": "10", "ImageWidth": "64",
		})
		assertTaskCount(t, ts, job, 2)
		names := frameNames("preview.%04d.png", seq(1, 12)...)
		assertExactFiles(t, out, names)
		assertPNGWidths(t, out, names, 64)
	})

	t.Run("karma cpu", func(t *testing.T) {
		out := filepath.Join(dir, "karma")
		job := runDCCJob(t, ts, farmID, queueID, "usdrecord-render", map[string]string{
			"SceneFile": stage, "OutputFile": filepath.Join(out, "karma.###.png"),
			"Frames": "3", "Renderer": "Karma CPU", "Camera": "cam", "ImageWidth": "32",
		})
		assertTaskCount(t, ts, job, 1)
		assertExactFiles(t, out, []string{"karma.003.png"})
		assertPNGWidths(t, out, []string{"karma.003.png"}, 32)
	})

	// A chunk that starts below zero reaches usdrecord as --frames=-2:-1, one
	// argument; as two, argparse reads -2:-1 as an option and the task fails.
	t.Run("negative frames", func(t *testing.T) {
		out := filepath.Join(dir, "neg")
		job := runDCCJob(t, ts, farmID, queueID, "usdrecord-render", map[string]string{
			"SceneFile": stage, "OutputFile": filepath.Join(out, "neg.####.png"),
			"Frames": "-2--1", "ImageWidth": "16",
		})
		assertTaskCount(t, ts, job, 1)
		names := []string{"neg.-002.png", "neg.-001.png"}
		assertExactFiles(t, out, names)
		assertPNGWidths(t, out, names, 16)
	})
}
