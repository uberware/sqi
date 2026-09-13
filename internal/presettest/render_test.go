// SPDX-License-Identifier: AGPL-3.0-or-later

package presettest_test

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/presettest"
	"github.com/uberware/sqi/internal/store"
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
