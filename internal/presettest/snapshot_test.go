// SPDX-License-Identifier: AGPL-3.0-or-later

package presettest_test

import (
	"errors"
	"testing"

	"github.com/uberware/sqi/internal/presettest"
	"github.com/uberware/sqi/internal/store"
)

const rangeTemplate = `
specificationVersion: jobtemplate-2023-09
name: Range Job
parameterDefinitions:
  - name: Frames
    type: STRING
    default: "1-3"
steps:
  - name: Render
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: INT
          range: "{{Param.Frames}}"
    script:
      actions:
        onRun:
          command: renderer
          args: ["-f", "{{Task.Param.Frame}}", "-scene", "{{Param.Frames}}"]
`

// TestCapture_ExpandsAndResolvesEveryTask is the harness's central claim: for a
// stated parameter set it produces one fully resolved command line per task.
func TestCapture_ExpandsAndResolvesEveryTask(t *testing.T) {
	snap, err := presettest.Capture(t.Context(), rangeTemplate, presettest.Options{
		Params: map[string]string{"Frames": "1-3"},
		Format: store.TemplateFormatYAML,
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if len(snap.Steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(snap.Steps))
	}
	st := snap.Steps[0]
	if st.Name != "Render" {
		t.Errorf("step name = %q, want %q", st.Name, "Render")
	}
	if len(st.Tasks) != 3 {
		t.Fatalf("tasks = %d, want 3", len(st.Tasks))
	}
	for i, want := range []string{"1", "2", "3"} {
		task := st.Tasks[i]
		if task.Command != "renderer" {
			t.Errorf("task %d command = %q, want %q", i, task.Command, "renderer")
		}
		if len(task.Args) != 4 {
			t.Fatalf("task %d args = %v, want 4 entries", i, task.Args)
		}
		if task.Args[1] != want {
			t.Errorf("task %d args[1] = %q, want %q", i, task.Args[1], want)
		}
		if task.Args[3] != "1-3" {
			t.Errorf("task %d args[3] = %q, want job param %q", i, task.Args[3], "1-3")
		}
	}
}

// TestCapture_ResolvesEmbeddedFileBodies covers the presets whose whole content
// is a script: the argv is just the script path, so a snapshot that stopped at
// argv would review nothing.
func TestCapture_ResolvesEmbeddedFileBodies(t *testing.T) {
	const tmpl = `
specificationVersion: jobtemplate-2023-09
name: Script Job
steps:
  - name: Run
    script:
      embeddedFiles:
        - name: main
          type: TEXT
          runnable: true
          data: |
            #!/bin/sh
            echo "job {{Param.Label}}"
      actions:
        onRun:
          command: "{{Task.File.main}}"
parameterDefinitions:
  - name: Label
    type: STRING
    default: unset
`
	snap, err := presettest.Capture(t.Context(), tmpl, presettest.Options{
		Params: map[string]string{"Label": "alpha"},
		Format: store.TemplateFormatYAML,
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	task := snap.Steps[0].Tasks[0]
	if len(task.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(task.Files))
	}
	if got := task.Files[0].Data; !contains(got, `echo "job alpha"`) {
		t.Errorf("embedded file body not resolved: %q", got)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	}()
}

// TestCapture_RefusesTemplateWithEnvironments is the §3.4 guard.
// session.Manager.Create EXECUTES environment onEnter actions; a preset that
// declares one would silently run a vendor command inside a unit test. No
// shipped template declares environments today, so this must fail loudly the
// day one does rather than be discovered by a process starting.
func TestCapture_RefusesTemplateWithEnvironments(t *testing.T) {
	const tmpl = `
specificationVersion: jobtemplate-2023-09
name: Env Job
jobEnvironments:
  - name: Setup
    script:
      actions:
        onEnter:
          command: setup-the-farm
steps:
  - name: Run
    script:
      actions:
        onRun:
          command: work
`
	_, err := presettest.Capture(t.Context(), tmpl, presettest.Options{Format: store.TemplateFormatYAML})
	if !errors.Is(err, presettest.ErrTemplateHasEnvironments) {
		t.Fatalf("Capture error = %v, want ErrTemplateHasEnvironments", err)
	}
}
