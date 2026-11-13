// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

// preset_dcc_unreal_test.go — renders the three Unreal Movie Render Queue
// presets with a real Unreal Engine editor, through a real sqi-server and
// sqi-worker, and asserts on the frames it writes. Like the other
// TestPresetDCC_ cases it skips when the application is absent, so a local pass
// proves nothing unless SQI_TEST_REQUIRE_APPS lists unreal.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// unrealJobTimeout covers an editor start per task plus the render. A new
// project's first render also compiles its shaders, which can take many
// minutes on a cold shader cache.
const unrealJobTimeout = 30 * time.Minute

// unrealProject is the .uproject the test builds its content in: no code
// modules, and the plug-ins the presets require. %s is the engine version.
const unrealProject = `{
	"FileVersion": 3,
	"EngineAssociation": "%s",
	"Plugins": [
		{ "Name": "MovieRenderPipeline", "Enabled": true },
		{ "Name": "PythonScriptPlugin", "Enabled": true },
		{ "Name": "EditorScriptingUtilities", "Enabled": true },
		{ "Name": "SequencerScripting", "Enabled": true }
	]
}
`

// buildUnrealProject writes a project under dir and builds its content with
// testdata/dcc/unreal_project.py, returning the .uproject path.
func buildUnrealProject(t *testing.T, editor, dir string) string {
	t.Helper()
	project := filepath.Join(dir, "SqiTest", "SqiTest.uproject")
	if err := os.MkdirAll(filepath.Dir(project), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project, []byte(fmt.Sprintf(unrealProject, engineVersion(editor))), 0o644); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "build.log")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, editor, project, "-run=pythonscript",
		"-script="+dccTestdata(t, "unreal_project.py"), "-unattended", "-nullrhi", "-stdout", "-nosplash")
	cmd.Env = append(os.Environ(), "SQI_BUILD_LOG="+log)
	out, err := cmd.CombinedOutput()
	built, _ := os.ReadFile(log) //nolint:errcheck // a missing log is reported below
	if err != nil || !strings.Contains(string(built), "BUILD OK") {
		t.Fatalf("building the Unreal project: %v\n%s\neditor output:\n%s", err, built, tail(string(out), 40))
	}
	return project
}

// engineVersionRE finds the engine version in a launcher install's folder name.
var engineVersionRE = regexp.MustCompile(`UE_(\d+\.\d+)`)

// engineVersion returns the engine version of the editor at path, such as
// "5.8" for a launcher install in UE_5.8, or "" for an install whose folder
// names no version, which the editor reads as its own version.
func engineVersion(path string) string {
	if m := engineVersionRE.FindStringSubmatch(path); m != nil {
		return m[1]
	}
	return ""
}

// tail returns the last n lines of s.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// TestPresetDCC_Unreal_MRQRender renders the three Unreal presets with a real
// UnrealEditor-Cmd, found on PATH or through the unreal detector's command
// default, on a project the test builds in a folder whose name has a space.
// The frames on disk show that the job preset renders a sequence named by
// package paths into the Output Directory; that the queue preset renders a
// saved queue; that the shot preset runs one task per shot, and that a task
// renders only its own shot; and that a shot the sequence lacks fails its task,
// naming the shots the sequence has.
func TestPresetDCC_Unreal_MRQRender(t *testing.T) {
	const caseName = "TestPresetDCC_Unreal_MRQRender"
	trackOutcome(t, caseName)
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		skipOutcome(t, caseName, "the Unreal presets run on Windows and macOS workers only")
	}
	editor := requireDCC(t, caseName, "Unreal Engine", "UnrealEditor-Cmd")

	dir := filepath.Join(t.TempDir(), "unreal project")
	project := buildUnrealProject(t, editor, dir)

	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	startRealWorkerAnyOS(t, ts, farmID, queueID)

	job := func(seq string) map[string]string {
		return map[string]string{
			"SceneFile": project, "LevelSequence": "/Game/Cinematics/" + seq,
			"Map": "/Game/Maps/Test", "MovieRenderConfig": "/Game/Cinematics/MRQ_Test",
		}
	}

	t.Run("job", func(t *testing.T) {
		out := filepath.Join(dir, "renders", "job")
		params := job("SEQ_Single")
		params["OutputDirectory"] = out
		id, _ := runPresetJobWithin(t, ts, farmID, queueID, "unreal-mrq-job-render", "completed", unrealJobTimeout, params)
		assertTaskCount(t, ts, id, 1)
		names := frameNames("SEQ_Single.no shot.%04d.png", 0, 1, 2)
		assertExactFiles(t, out, names)
		assertPNGSize(t, out, names, 64, 36)
	})

	t.Run("queue", func(t *testing.T) {
		out := filepath.Join(dir, "renders", "queue")
		id, _ := runPresetJobWithin(t, ts, farmID, queueID, "unreal-mrq-queue-render", "completed", unrealJobTimeout, map[string]string{
			"SceneFile": project, "QueueAsset": "/Game/Cinematics/Q_Test", "OutputDirectory": out,
		})
		assertTaskCount(t, ts, id, 1)
		names := frameNames("SEQ_Single.no shot.%04d.png", 0, 1, 2)
		assertExactFiles(t, out, names)
		assertPNGSize(t, out, names, 64, 36)
	})

	t.Run("shots", func(t *testing.T) {
		out := filepath.Join(dir, "renders", "shots")
		params := job("SEQ_Shots")
		params["Shots"] = `["SH010","SH020"]`
		params["OutputDirectory"] = out
		id, _ := runPresetJobWithin(t, ts, farmID, queueID, "unreal-mrq-shot-render", "completed", unrealJobTimeout, params)
		assertTaskCount(t, ts, id, 2)
		names := append(frameNames("SEQ_Shots.SH010.%04d.png", 0, 1), frameNames("SEQ_Shots.SH020.%04d.png", 2, 3)...)
		assertExactFiles(t, out, names)
		assertPNGSize(t, out, names, 64, 36)
	})

	// One shot of two: its task disables the other, so only its frames appear.
	t.Run("one shot", func(t *testing.T) {
		out := filepath.Join(dir, "renders", "one shot")
		params := job("SEQ_Shots")
		params["Shots"] = `["SH020"]`
		params["OutputDirectory"] = out
		id, _ := runPresetJobWithin(t, ts, farmID, queueID, "unreal-mrq-shot-render", "completed", unrealJobTimeout, params)
		assertTaskCount(t, ts, id, 1)
		names := frameNames("SEQ_Shots.SH020.%04d.png", 2, 3)
		assertExactFiles(t, out, names)
		assertPNGSize(t, out, names, 64, 36)
	})

	t.Run("missing shot fails", func(t *testing.T) {
		params := job("SEQ_Shots")
		params["Shots"] = `["SH999"]`
		params["OutputDirectory"] = filepath.Join(dir, "renders", "missing")
		_, report := runPresetJobWithin(t, ts, farmID, queueID, "unreal-mrq-shot-render", "failed", unrealJobTimeout, params)
		const want = `has no shot named "SH999"; its shots are "SH010", "SH020"`
		if !strings.Contains(report, want) {
			t.Errorf("failed task report does not contain %q:\n%s", want, report)
		}
	})
}
