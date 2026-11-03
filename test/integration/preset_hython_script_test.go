// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/presettest"
)

// hythonRunner runs the shipped launcher under a stub `hou` module. The stub
// records each hipFile.load and setFrame call, and the runner prints them as a
// STATE line after the launcher (and so the user's script) returns. A real
// Python runs the shipped bytes; only Houdini itself is stubbed, since no
// Houdini is available to CI.
//
// It is a separate runner rather than a hou.py next to the launcher because
// `python -I` puts neither the script's directory nor the current directory on
// sys.path.
const hythonRunner = `import json, runpy, sys, types

state = {"loads": [], "frames": []}

hou = types.ModuleType("hou")
hou.hipFile = types.SimpleNamespace(
    load=lambda path, ignore_load_warnings=False: state["loads"].append(
        {"path": path, "ignore_load_warnings": ignore_load_warnings}
    )
)
hou.setFrame = lambda frame: state["frames"].append(frame)
sys.modules["hou"] = hou

launcher = sys.argv[1]
sys.argv = sys.argv[1:]
runpy.run_path(launcher, run_name="__main__")
print("STATE:" + json.dumps(state))
`

// hythonUserScript prints what the user's script can observe about how it was
// started.
const hythonUserScript = `import json, os, sys

print("USER:" + json.dumps({
    "name": __name__,
    "argv0": sys.argv[0],
    "argc": len(sys.argv),
    "env": {k: os.environ.get(k) for k in ("SQI_FRAME", "SQI_FRAME_START", "SQI_FRAME_END", "SQI_FRAMES")},
}))
`

// TestHythonScript_LauncherRunsTheScript pins what the embedded launcher does
// before and while it runs the user's script: the four frame variables, the hip
// load, the first setFrame, the script's own __main__ identity, and that an
// exception or a SystemExit reaches the process exit status unchanged.
func TestHythonScript_LauncherRunsTheScript(t *testing.T) {
	python := findPython3(t)
	if python == "" {
		t.Skip("no working Python 3 on PATH (tried python3, python)")
	}

	tmpl, err := presettest.PresetTemplate(presettest.SourcePresets, "hython-script")
	if err != nil {
		t.Fatalf("PresetTemplate: %v", err)
	}
	launcher := embeddedFileData(t, tmpl, "launcher")

	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "launcher.py")
	runnerPath := filepath.Join(dir, "runner.py")
	userPath := filepath.Join(dir, "user.py")
	if err := os.WriteFile(launcherPath, []byte(launcher), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runnerPath, []byte(hythonRunner), 0o600); err != nil {
		t.Fatal(err)
	}

	// run writes the user's script, runs the launcher over it and returns the
	// combined output with the process exit code (-1 when it did not start).
	run := func(t *testing.T, body string) (string, int) {
		t.Helper()
		if err := os.WriteFile(userPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.CommandContext(t.Context(), python, "-I", runnerPath, launcherPath,
			"scene.hip", userPath, "5", "8", "5-8").CombinedOutput()
		if err == nil {
			return string(out), 0
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run launcher: %v", err)
		}
		return string(out), exitErr.ExitCode()
	}

	t.Run("runs the script", func(t *testing.T) {
		out, code := run(t, hythonUserScript)
		if code != 0 {
			t.Fatalf("launcher exited %d, want 0:\n%s", code, out)
		}
		var state struct {
			Loads []struct {
				Path               string `json:"path"`
				IgnoreLoadWarnings bool   `json:"ignore_load_warnings"`
			} `json:"loads"`
			Frames []int `json:"frames"`
		}
		var user struct {
			Name  string            `json:"name"`
			Argv0 string            `json:"argv0"`
			Argc  int               `json:"argc"`
			Env   map[string]string `json:"env"`
		}
		for line := range strings.SplitSeq(out, "\n") {
			line = strings.TrimSpace(line)
			if rest, ok := strings.CutPrefix(line, "STATE:"); ok {
				if err := json.Unmarshal([]byte(rest), &state); err != nil {
					t.Fatalf("STATE line %q: %v", line, err)
				}
			}
			if rest, ok := strings.CutPrefix(line, "USER:"); ok {
				if err := json.Unmarshal([]byte(rest), &user); err != nil {
					t.Fatalf("USER line %q: %v", line, err)
				}
			}
		}
		if len(state.Loads) != 1 || state.Loads[0].Path != "scene.hip" || !state.Loads[0].IgnoreLoadWarnings {
			t.Errorf("hip loads = %+v, want one load of scene.hip with ignore_load_warnings=True", state.Loads)
		}
		if !reflect.DeepEqual(state.Frames, []int{5}) {
			t.Errorf("setFrame calls = %v, want [5]", state.Frames)
		}
		if user.Name != "__main__" {
			t.Errorf("script __name__ = %q, want __main__", user.Name)
		}
		if !strings.HasSuffix(user.Argv0, "user.py") || user.Argc != 1 {
			t.Errorf("script sys.argv = argv0 %q, argc %d, want its own path alone", user.Argv0, user.Argc)
		}
		wantEnv := map[string]string{
			"SQI_FRAME": "5", "SQI_FRAME_START": "5", "SQI_FRAME_END": "8", "SQI_FRAMES": "5-8",
		}
		if !reflect.DeepEqual(user.Env, wantEnv) {
			t.Errorf("script environment = %v, want %v", user.Env, wantEnv)
		}
	})

	for _, tc := range []struct {
		name     string
		body     string
		wantCode int // 0 means any non-zero code
		wantOut  string
	}{
		{"raises", `raise RuntimeError("boom")`, 0, "boom"},
		{"exit code", `raise SystemExit(3)`, 3, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, code := run(t, tc.body)
			if tc.wantCode == 0 && code == 0 {
				t.Errorf("exited 0, want a non-zero code:\n%s", out)
			}
			if tc.wantCode != 0 && code != tc.wantCode {
				t.Errorf("exited %d, want %d:\n%s", code, tc.wantCode, out)
			}
			if !strings.Contains(out, tc.wantOut) {
				t.Errorf("output does not contain %q:\n%s", tc.wantOut, out)
			}
		})
	}
}
