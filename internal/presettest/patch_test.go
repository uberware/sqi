// SPDX-License-Identifier: AGPL-3.0-or-later

package presettest_test

import (
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/presettest"
	"github.com/uberware/sqi/internal/store"
)

const chunkTemplate = `
specificationVersion: jobtemplate-2023-09
name: Chunk Job
extensions: [TASK_CHUNKING, SQI_CHUNK_BOUNDS]
parameterDefinitions:
  - name: Frames
    type: STRING
    default: "1-10"
steps:
  - name: Render
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: CHUNK[INT]
          range: "{{Param.Frames}}"
          chunks:
            defaultTaskCount: 1
            rangeConstraint: CONTIGUOUS
    script:
      actions:
        onRun:
          command: renderer
          args: ["-s", "{{Task.Param.Frame.Start}}", "-e", "{{Task.Param.Frame.End}}"]
`

func TestApplyChunkPatch_RaisesTaskCount(t *testing.T) {
	patched, err := presettest.ApplyChunkPatch(chunkTemplate, presettest.ChunkPatch{
		Step: "Render", ChunksDefaultTaskCount: 5,
	})
	if err != nil {
		t.Fatalf("ApplyChunkPatch: %v", err)
	}
	snap, err := presettest.Capture(t.Context(), patched, presettest.Options{
		Params: map[string]string{"Frames": "1-10"},
		Format: store.TemplateFormatYAML,
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	tasks := snap.Steps[0].Tasks
	if len(tasks) != 2 {
		t.Fatalf("tasks = %d, want 2 (10 frames in chunks of 5)", len(tasks))
	}
	// The whole point of the patch: Start and End differ, so the -s/-e
	// substitution is actually exercised.
	if tasks[0].Args[1] != "1" || tasks[0].Args[3] != "5" {
		t.Errorf("task 1 bounds = %v, want -s 1 -e 5", tasks[0].Args)
	}
	if tasks[1].Args[1] != "6" || tasks[1].Args[3] != "10" {
		t.Errorf("task 2 bounds = %v, want -s 6 -e 10", tasks[1].Args)
	}
}

func TestApplyChunkPatch_UnknownStepIsAnError(t *testing.T) {
	_, err := presettest.ApplyChunkPatch(chunkTemplate, presettest.ChunkPatch{
		Step: "Nope", ChunksDefaultTaskCount: 5,
	})
	if err == nil {
		t.Fatal("ApplyChunkPatch: want error for unknown step, got nil")
	}
	if !strings.Contains(err.Error(), "Nope") {
		t.Errorf("error should name the step: %v", err)
	}
}

func TestApplyChunkPatch_StepWithoutChunksIsAnError(t *testing.T) {
	const noChunks = `
specificationVersion: jobtemplate-2023-09
name: List Job
steps:
  - name: Render
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: INT
          range: "1-3"
    script:
      actions:
        onRun:
          command: renderer
`
	_, err := presettest.ApplyChunkPatch(noChunks, presettest.ChunkPatch{
		Step: "Render", ChunksDefaultTaskCount: 5,
	})
	if err == nil {
		t.Fatal("ApplyChunkPatch: want error for a step with no chunks: block, got nil")
	}
}

// TestStripOSFamilyGate_KeepsEverythingButTheGate runs the strip over every
// shipped preset that declares an os.family gate: the parsed template must
// equal the original with only its os.family requirements removed, so each
// embedded file survives the YAML round trip byte for byte (a PowerShell
// wrapper's leading BOM and a sh wrapper's line breaks included).
func TestStripOSFamilyGate_KeepsEverythingButTheGate(t *testing.T) {
	root, err := presettest.RepoRoot()
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	paths, err := filepath.Glob(filepath.Join(root, "presets", "sqi", "*.yaml"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	gated := 0
	for _, p := range paths {
		name := strings.TrimSuffix(filepath.Base(p), ".yaml")
		raw, err := presettest.PresetTemplate(presettest.SourcePresets, name)
		if err != nil {
			t.Fatalf("PresetTemplate(%s): %v", name, err)
		}
		if !strings.Contains(raw, "attr.worker.os.family") {
			continue
		}
		gated++
		t.Run(name, func(t *testing.T) {
			want, err := openjd.Parse([]byte(raw), openjd.FormatYAML)
			if err != nil {
				t.Fatalf("parse shipped template: %v", err)
			}
			stripped, err := presettest.StripOSFamilyGate(raw)
			if err != nil {
				t.Fatalf("StripOSFamilyGate: %v", err)
			}
			got, err := openjd.Parse([]byte(stripped), openjd.FormatYAML)
			if err != nil {
				t.Fatalf("parse stripped template: %v", err)
			}
			for i := range want.Steps {
				want.Steps[i].HostRequirements = withoutOSFamily(want.Steps[i].HostRequirements)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("the strip changed more than the os.family requirements\n got: %+v\nwant: %+v", got, want)
			}
		})
	}
	if gated == 0 {
		t.Fatal("no shipped preset declares an os.family gate; the test checked nothing")
	}
}

func TestStripOSFamilyGate_UngatedTemplateIsAnError(t *testing.T) {
	if _, err := presettest.StripOSFamilyGate(chunkTemplate); err == nil {
		t.Fatal("StripOSFamilyGate: want error for a template with no os.family requirement, got nil")
	}
}

// withoutOSFamily returns hr with its os.family attributes removed, or nil when
// nothing is left: the strip drops an emptied hostRequirements, as OpenJD
// rejects one.
func withoutOSFamily(hr *openjd.HostRequirements) *openjd.HostRequirements {
	if hr == nil {
		return nil
	}
	out := *hr
	out.Attributes = slices.DeleteFunc(slices.Clone(hr.Attributes), func(a openjd.AttributeRequirement) bool {
		return a.Name == "attr.worker.os.family"
	})
	if len(out.Attributes) == 0 {
		out.Attributes = nil
		if len(out.Amounts) == 0 {
			return nil
		}
	}
	return &out
}
