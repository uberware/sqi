// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// tutorialChunkTemplate reproduces the chunks block of the specification's own
// tutorial (wiki/Job-Intro-03-Creating-a-Job-Template.md, lines 1006-1027): a
// range built from two parameters, and both sizing fields given as format
// strings. sqi rejected this at upload before P2.
const tutorialChunkTemplate = `
specificationVersion: jobtemplate-2023-09
name: Tutorial Chunks
extensions: [TASK_CHUNKING]
parameterDefinitions:
  - { name: FrameStart, type: INT, default: 1 }
  - { name: FrameEnd, type: INT, default: 10 }
  - { name: ChunkSize, type: INT, minValue: 1, default: 3 }
  - { name: ChunkTargetRuntimeSeconds, type: INT, minValue: 0, default: 0 }
steps:
  - name: Render
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: CHUNK[INT]
          range: "{{Param.FrameStart}}-{{Param.FrameEnd}}"
          chunks:
            defaultTaskCount: "{{Param.ChunkSize}}"
            targetRuntimeSeconds: "{{Param.ChunkTargetRuntimeSeconds}}"
            rangeConstraint: CONTIGUOUS
    script: { actions: { onRun: { command: render, args: ["{{Task.Param.Frame}}"] } } }
`

func submitChunks(t *testing.T, tmpl string, params map[string]string) (*openjd.SubmitResult, error) {
	t.Helper()
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)
	return openjd.NewSubmitter(st).Submit(t.Context(), tmpl, store.TemplateFormatYAML, openjd.SubmitOptions{
		FarmID: farmID, QueueID: queueID, Parameters: params,
	})
}

func chunkValuesOf(res *openjd.SubmitResult) []string {
	var out []string
	for _, task := range res.Tasks {
		out = append(out, task.Parameters["Frame"])
	}
	slices.Sort(out)
	return out
}

func TestSubmit_SpecTutorialChunkTemplate(t *testing.T) {
	tests := []struct {
		name   string
		params map[string]string
		want   []string
	}{
		{"defaults", nil, []string{"1-3", "10-10", "4-6", "7-9"}},
		{"chunk size from a parameter", map[string]string{"ChunkSize": "5"}, []string{"1-5", "6-10"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := submitChunks(t, tutorialChunkTemplate, tc.params)
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			if got := chunkValuesOf(res); strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("chunks = %v, want %v", got, tc.want)
			}
		})
	}
}

// A chunk count that resolves below 1 must be a 422 naming the field. The
// template here declares no minValue, so binding cannot catch it first.
func TestSubmit_ChunkSizeResolvingBelowOneRejected(t *testing.T) {
	tmpl := strings.Replace(tutorialChunkTemplate, "minValue: 1, default: 3", "default: 3", 1)
	_, err := submitChunks(t, tmpl, map[string]string{"ChunkSize": "0"})
	var ve *openjd.SubmitValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want a SubmitValidationError", err)
	}
	if !strings.Contains(err.Error(), "/steps/0/parameterSpace/taskParameterDefinitions/0/chunks/defaultTaskCount") {
		t.Fatalf("err = %v, want it to point at chunks/defaultTaskCount", err)
	}
}

// Review Focus 5: a non-integer reaching an INT chunk-size parameter is a 422
// naming the parameter -- never a 500, never a silent chunk size of 1.
func TestSubmit_ChunkSizeNonIntegerParameterRejected(t *testing.T) {
	_, err := submitChunks(t, tutorialChunkTemplate, map[string]string{"ChunkSize": "abc"})
	var ve *openjd.SubmitValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want a SubmitValidationError", err)
	}
	if !strings.Contains(err.Error(), "ChunkSize") {
		t.Fatalf("err = %v, want it to name ChunkSize", err)
	}
}

// A STRING parameter is not range-checked at binding, so a non-integer value
// reaches resolution, which must reject it rather than chunk by 1.
func TestSubmit_ChunkSizeResolvingToTextRejected(t *testing.T) {
	tmpl := strings.Replace(tutorialChunkTemplate,
		"{ name: ChunkSize, type: INT, minValue: 1, default: 3 }",
		"{ name: ChunkSize, type: STRING, default: \"3\" }", 1)
	_, err := submitChunks(t, tmpl, map[string]string{"ChunkSize": "three"})
	if err == nil || !strings.Contains(err.Error(), "not an integer") {
		t.Fatalf("err = %v, want a not-an-integer error", err)
	}
}

const exprChunkTemplate = `
specificationVersion: jobtemplate-2023-09
name: Expr Chunks
extensions: [EXPR, TASK_CHUNKING]
parameterDefinitions:
  - { name: N, type: INT, default: 3 }
steps:
  - name: Render
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: CHUNK[INT]
          range: "1-12"
          chunks: { defaultTaskCount: "%s", rangeConstraint: CONTIGUOUS }
    script: { actions: { onRun: { command: render } } }
`

func TestSubmit_ChunkSizeExpressionUnderEXPR(t *testing.T) {
	res, err := submitChunks(t, strings.Replace(exprChunkTemplate, "%s", "{{ Param.N * 2 }}", 1), nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if got := chunkValuesOf(res); strings.Join(got, " ") != "1-6 7-12" {
		t.Fatalf("chunks = %v, want [1-6 7-12]", got)
	}
}

func TestValidate_ChunkSizeHostScopeRejectedUnderEXPR(t *testing.T) {
	tmpl, err := openjd.Parse(
		[]byte(strings.Replace(exprChunkTemplate, "%s", "{{ Session.WorkingDirectory }}", 1)), openjd.FormatYAML,
	)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	errs := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{EnforceLimits: true})
	if !strings.Contains(errs.Error(), "/chunks/defaultTaskCount") {
		t.Fatalf("errors = %v, want one at chunks/defaultTaskCount", errs)
	}
}
