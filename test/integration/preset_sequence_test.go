// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/presettest"
)

// TestPresetPythonSequence_LauncherContract is python-sequence's Tier-2 claim:
// a real Python runs the product's launcher, the one part of the product no
// stub can stand in for. It pins the four variables per task, the child's exit
// code, (POSIX) a canceled child's own exit code, and that the script runs as
// __main__ with its own path in sys.argv[0], as under `python script.py`.
//
// Named TestPreset* so the harness's -run pattern (Makefile, ci.yml) selects
// it; registered in presets/validation-tiers.yaml as a tier2 case, so a skip
// on a required platform fails TestZZPresetTierRegistrySatisfied.
func TestPresetPythonSequence_LauncherContract(t *testing.T) {
	const caseName = "TestPresetPythonSequence_LauncherContract"
	trackOutcome(t, caseName)
	python := requirePython(t, caseName)

	tmpl, err := presettest.PresetTemplate(presettest.SourceBuiltins, "python-sequence")
	if err != nil {
		t.Fatalf("PresetTemplate: %v", err)
	}

	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	startRealWorkerAnyOS(t, ts, farmID, queueID)

	outDir := t.TempDir()
	envScript := fmt.Sprintf(`import os, sys
names = ("SQI_FRAME", "SQI_FRAME_START", "SQI_FRAME_END", "SQI_FRAMES")
line = ",".join(os.environ[n] for n in names)
line += "," + __name__ + "," + os.path.basename(sys.argv[0])
with open(os.path.join(r"%s", os.environ["SQI_FRAME_START"] + ".txt"), "w") as f:
    f.write(line)
`, filepath.ToSlash(outDir))

	envJob := submitPresetJob(t, ts, farmID, queueID, tmpl, map[string]string{
		"Interpreter": python, "Script": envScript, "Frames": "1-4", "FramesPerTask": "2",
	})
	exitJob := submitPresetJob(t, ts, farmID, queueID, tmpl, map[string]string{
		"Interpreter": python, "Script": "import sys\nsys.exit(7)\n", "Frames": "1",
	})

	t.Run("variables per task", func(t *testing.T) {
		if status := pollJobStatus(t, ts, envJob, []string{"completed", "failed", "canceled"}, presetJobTimeout); status != "completed" {
			t.Fatalf("job status = %q, want completed (failure_reason: %q)", status, firstTaskFailureReason(t, ts, envJob))
		}
		want := map[string]string{
			"1.txt": "1,1,2,1-2,__main__,script.py",
			"3.txt": "3,3,4,3-4,__main__,script.py",
		}
		for name, line := range want {
			got, err := os.ReadFile(filepath.Join(outDir, name))
			if err != nil {
				t.Fatalf("task output %s: %v", name, err)
			}
			if string(got) != line {
				t.Errorf("%s = %q, want %q", name, got, line)
			}
		}
	})

	t.Run("exit code is the script's", func(t *testing.T) {
		if status := pollJobStatus(t, ts, exitJob, []string{"completed", "failed", "canceled"}, presetJobTimeout); status != "failed" {
			t.Fatalf("job status = %q, want failed", status)
		}
		if reason := firstTaskFailureReason(t, ts, exitJob); !strings.Contains(reason, "exited with code 7") {
			t.Fatalf("failure_reason = %q, want it to report exit code 7", reason)
		}
	})

	t.Run("canceled child's exit code is reported", func(t *testing.T) {
		launcherForwardsSignalExit(t, python, pythonSequenceLauncher(t, tmpl))
	})
}

// requirePython returns the absolute path of a working Python 3, or skips --
// recording the skip against caseName, which the registry turns into a failure
// on a required platform. A path is only accepted once it has RUN: on Windows,
// `python3` commonly resolves to an App Execution Alias that opens the Store
// instead of running anything.
func requirePython(t *testing.T, caseName string) string {
	t.Helper()
	for _, name := range []string{"python3", "python"} {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		out, err := exec.CommandContext(t.Context(), path, "-c", "import sys; print(sys.version_info[0])").Output()
		if err == nil && strings.TrimSpace(string(out)) == "3" {
			abs, err := filepath.Abs(path)
			if err == nil {
				return abs
			}
		}
	}
	skipOutcome(t, caseName, "no working Python 3 on PATH (tried python3, python)")
	return ""
}

// pythonSequenceLauncher returns the product's launcher.py text, read from the
// parsed template so the test always runs the shipped bytes.
func pythonSequenceLauncher(t *testing.T, tmpl string) string {
	t.Helper()
	parsed, err := openjd.Parse([]byte(tmpl), openjd.FormatYAML)
	if err != nil {
		t.Fatalf("openjd.Parse: %v", err)
	}
	for _, f := range parsed.Steps[0].Script.EmbeddedFiles {
		if f.Name == "launcher" {
			return f.Data
		}
	}
	t.Fatal("python-sequence declares no embedded file named launcher")
	return ""
}
