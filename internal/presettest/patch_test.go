// SPDX-License-Identifier: AGPL-3.0-or-later

package presettest_test

import (
	"strings"
	"testing"

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
