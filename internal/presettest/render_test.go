// SPDX-License-Identifier: AGPL-3.0-or-later

package presettest_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/presettest"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// TestRender_IsStableAcrossRuns is the property that makes a golden reviewable.
// Two Captures of the same template must render byte-identically: session work
// dirs, IDs and any other per-run value must be normalized away. A golden that
// churns is a golden nobody reads.
func TestRender_IsStableAcrossRuns(t *testing.T) {
	const tmpl = `
specificationVersion: jobtemplate-2023-09
name: Stable Job
steps:
  - name: Run
    script:
      embeddedFiles:
        - name: main
          type: TEXT
          runnable: true
          data: "echo hi"
      actions:
        onRun:
          command: "{{Task.File.main}}"
          args: ["{{Session.WorkingDirectory}}"]
`
	first, err := presettest.Capture(t.Context(), tmpl, presettest.Options{Format: store.TemplateFormatYAML})
	if err != nil {
		t.Fatalf("Capture 1: %v", err)
	}
	second, err := presettest.Capture(t.Context(), tmpl, presettest.Options{Format: store.TemplateFormatYAML})
	if err != nil {
		t.Fatalf("Capture 2: %v", err)
	}
	a, b := presettest.Render(first), presettest.Render(second)
	if a != b {
		t.Errorf("render is not stable across runs:\n--- first ---\n%s\n--- second ---\n%s", a, b)
	}
	if strings.Contains(a, "presettest-sessions-") {
		t.Errorf("render leaked a real session path:\n%s", a)
	}
	if !strings.Contains(a, "<WORKDIR>") {
		t.Errorf("render did not normalize the session working directory:\n%s", a)
	}
}

// TestRender_ShowsEveryArgAndFileBody pins the golden format itself: one
// indexed line per argument and the full resolved body of every embedded file.
func TestRender_ShowsEveryArgAndFileBody(t *testing.T) {
	snap := presettest.Snapshot{
		Preset: "demo",
		Case:   "default",
		Params: map[string]string{"Beta": "2", "Alpha": "1"},
		Steps: []presettest.StepSnapshot{{
			Name: "Render",
			Tasks: []presettest.TaskSnapshot{{
				Name:    "Render[Frame=1]",
				Params:  map[string]string{"Frame": "1"},
				Command: "renderer",
				Args:    []string{"-f", "1"},
				Files:   []presettest.FileSnapshot{{Name: "main", Filename: "main.sh", Data: "echo hi\n"}},
			}},
		}},
	}
	got := presettest.Render(snap)
	for _, want := range []string{
		"preset: demo",
		"case:   default",
		"  Alpha=1",
		"  Beta=2",
		`step "Render" — 1 task`,
		`task 1/1  name="Render[Frame=1]"`,
		"  command: renderer",
		"    [0]  -f",
		"    [1]  1",
		`  embedded file "main" (main.sh):`,
		"    echo hi",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q:\n%s", want, got)
		}
	}
	// Params must render sorted, so a map's iteration order cannot churn a golden.
	if strings.Index(got, "Alpha=1") > strings.Index(got, "Beta=2") {
		t.Errorf("params not sorted:\n%s", got)
	}
}

// TestRender_GoldensAreHostNeutral pins the property that lets ONE reviewed
// golden per case serve every host: a path value that phase 3 rendered in the
// host's own flavor comes back in the POSIX spelling the goldens are written
// in, while text that merely happens to contain a backslash does not.
//
// Phase 3 is a host context, so Expression-Language section 1.2.1 gives it the
// host operating system's path semantics and an EXPR preset resolving
// "/mnt/show/a.mov" on a Windows worker really does produce "\mnt\show\a.mov".
// The inputs here are therefore built with filepath.FromSlash, which is what
// the worker itself applies: on Windows that makes these assertions real, and
// on POSIX it is the identity, so the same assertions hold for the untouched
// text. What must NOT hold on either is a blanket separator transform -- the
// last two cases are the values such a transform would corrupt.
func TestRender_GoldensAreHostNeutral(t *testing.T) {
	const (
		dir   = "/mnt/show/seq010/movies"
		src   = "/mnt/show/seq010/movies/shot_source.mov"
		winFx = `C:\show\seq010\movies\shot_source.mov`
		shell = `case "$out" in [A-Za-z]:[\/]*) out="${out//\//}" ;; esac`
	)
	snap := presettest.Snapshot{
		Preset: "demo",
		Case:   "default",
		// SourceFile is a full path; OutputDir is a directory the preset joins a
		// derived filename onto; WindowsSource is a literal Windows fixture, which
		// is what ffmpeg-segment-transcode-powershell's cases really bind.
		Params: map[string]string{"OutputDir": dir, "SourceFile": src, "WindowsSource": winFx},
		Steps: []presettest.StepSnapshot{{
			Name: "Encode",
			Tasks: []presettest.TaskSnapshot{{
				Name:    "Encode",
				Command: "ffmpeg",
				Args: []string{
					filepath.FromSlash(src),                       // the parameter itself
					filepath.FromSlash(dir + "/shot_seg_000.mp4"), // derived in its directory
					winFx, // never a POSIX path to begin with
				},
				Files: []presettest.FileSnapshot{{
					Name:     "join",
					Filename: "join.sh",
					// Both shapes in one body: a path the preset generated, and shell
					// syntax whose backslashes are not separators at all.
					Data: "file '" + filepath.FromSlash(dir+"/shot_seg_000.mp4") + "'\n" + shell + "\n",
				}},
			}},
		}},
	}

	got := presettest.Render(snap)
	for _, want := range []string{
		"    [0]  " + src,
		"    [1]  " + dir + "/shot_seg_000.mp4",
		"    [2]  " + winFx,
		"    file '" + dir + "/shot_seg_000.mp4'",
		"    " + shell,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q:\n%s", want, got)
		}
	}
	// The whole point: no rendered text may carry the host's separator for a
	// path the goldens spell with "/".
	if strings.Contains(got, `\mnt`) {
		t.Errorf("render leaked a host-flavored path:\n%s", got)
	}
}

// TestRender_LogChecks pins how a step's SQI_LOG_CHECKS block reaches a golden:
// one line per pattern, directly under the step header, quoted with %q so a
// pattern's own backslashes and quotes are unambiguous. A step with no checks,
// or an empty block, renders no log checks line.
func TestRender_LogChecks(t *testing.T) {
	task := presettest.TaskSnapshot{Name: "Render", Command: "renderer"}
	snap := presettest.Snapshot{
		Preset: "demo",
		Case:   "default",
		Steps: []presettest.StepSnapshot{{
			Name: "Render",
			LogChecks: &protocol.LogChecks{
				FailOn:      []string{`ERROR +\|`, `Traceback "x"`},
				RequireLine: []string{"SQI_RENDER: OK"},
			},
			Tasks: []presettest.TaskSnapshot{task},
		}},
	}
	got := presettest.Render(snap)
	want := "step \"Render\" — 1 task\n" +
		"  log checks: failOn \"ERROR +\\\\|\"\n" +
		"  log checks: failOn \"Traceback \\\"x\\\"\"\n" +
		"  log checks: requireLine \"SQI_RENDER: OK\"\n" +
		"\ntask 1/1"
	if !strings.Contains(got, want) {
		t.Errorf("render missing the log checks block:\nwant substring:\n%s\ngot:\n%s", want, got)
	}

	snap.Steps[0].LogChecks = nil
	if got := presettest.Render(snap); strings.Contains(got, "log checks") {
		t.Errorf("a step with no checks rendered a log checks line:\n%s", got)
	}
	snap.Steps[0].LogChecks = &protocol.LogChecks{}
	if got := presettest.Render(snap); strings.Contains(got, "log checks") {
		t.Errorf("an empty checks block rendered a log checks line:\n%s", got)
	}
}

// TestCapture_CarriesLogChecks asserts the snapshot takes the checks from the
// real assignment rather than from the harness's own reading of the template.
func TestCapture_CarriesLogChecks(t *testing.T) {
	const tmpl = `
specificationVersion: jobtemplate-2023-09
extensions: [SQI_LOG_CHECKS]
name: Checked Job
steps:
  - name: Render
    SQI_LOG_CHECKS:
      failOn: ['ERROR +\|']
      requireLine: ['SQI_RENDER: OK']
    script:
      actions:
        onRun:
          command: renderer
`
	snap, err := presettest.Capture(t.Context(), tmpl, presettest.Options{Format: store.TemplateFormatYAML})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if len(snap.Steps) != 1 || snap.Steps[0].LogChecks == nil {
		t.Fatalf("snapshot carries no log checks: %+v", snap.Steps)
	}
	lc := snap.Steps[0].LogChecks
	if len(lc.FailOn) != 1 || lc.FailOn[0] != `ERROR +\|` || len(lc.RequireLine) != 1 || lc.RequireLine[0] != "SQI_RENDER: OK" {
		t.Errorf("LogChecks = %+v", lc)
	}
	if got := presettest.Render(snap); !strings.Contains(got, `  log checks: failOn "ERROR +\\|"`) {
		t.Errorf("render missing the failOn line:\n%s", got)
	}
}

// TestHostPathReplacer_EscapedForms covers the spellings a host script's data
// takes. repr_json and repr_py write a Windows path with every backslash
// doubled (into params.json, VRED's -postpython, Unreal's -execcmds), and the
// replacer converts both that and the single-backslash form. On POSIX
// filepath.FromSlash is the identity, so the host spellings equal the golden's
// own and the same assertions hold for the untouched text.
func TestHostPathReplacer_EscapedForms(t *testing.T) {
	const (
		scene = "/proj/a b/scene.vpb"
		dir   = "/proj/a b/"
	)
	host := filepath.FromSlash(scene)
	hostDir := filepath.FromSlash(dir)
	esc := func(s string) string { return strings.ReplaceAll(s, `\`, `\\`) }

	snap := presettest.Snapshot{
		Preset: "demo",
		Case:   "default",
		Params: map[string]string{"SceneFile": scene},
		Steps: []presettest.StepSnapshot{{
			Name: "Render",
			Tasks: []presettest.TaskSnapshot{{
				Name:    "Render",
				Command: "renderer",
				Args: []string{
					host, // single-backslash form
					"-postpython=exec(open(r'" + esc(host) + "').read())", // repr_py spelling
					"--out=" + esc(hostDir) + "frame.png",                 // derived sibling, escaped
					"--plain=" + hostDir + "frame.png",                    // derived sibling, single
				},
				Files: []presettest.FileSnapshot{{
					Name:     "params",
					Filename: "params.json",
					Data:     `{"SceneFile": "` + esc(host) + `", "Dir": "` + esc(hostDir) + `"}` + "\n",
				}},
			}},
		}},
	}
	got := presettest.Render(snap)
	for _, want := range []string{
		"    [0]  " + scene,
		"    [1]  -postpython=exec(open(r'" + scene + "').read())",
		"    [2]  --out=" + dir + "frame.png",
		"    [3]  --plain=" + dir + "frame.png",
		`    {"SceneFile": "` + scene + `", "Dir": "` + dir + `"}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, `\`) {
		t.Errorf("render leaked a host-flavored or escaped path:\n%s", got)
	}
}
