// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"fmt"
	"strings"
	"testing"
)

// chunkTemplate is a one-step CHUNK[INT] template whose chunks block is the
// given YAML flow-mapping body, with ChunkSize and Runtime declared as INT job
// parameters so a {{Param.*}} reference is in scope.
func chunkTemplate(chunks string) string {
	return fmt.Sprintf(`
specificationVersion: jobtemplate-2023-09
name: J
extensions: [TASK_CHUNKING]
parameterDefinitions:
  - { name: ChunkSize, type: INT, default: 3 }
  - { name: Runtime, type: INT, default: 0 }
steps:
  - name: S
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: CHUNK[INT]
          range: "1-10"
          chunks: { %s }
    script: { actions: { onRun: { command: r } } }
`, chunks)
}

// The spec types both sizing fields `<integer> | <intstring> # @fmtstring`
// (Template Schemas, chunks); sqi decoded them as plain integers and rejected
// the spec's own tutorial template.
func TestDecodeTaskChunks_SizingFieldsAcceptFormatStrings(t *testing.T) {
	tests := []struct {
		name        string
		chunks      string
		wantErr     string
		wantCount   int
		wantCountEx string
		wantTRS     *int
		wantTRSEx   string
	}{
		{name: "integer", chunks: "defaultTaskCount: 10, rangeConstraint: CONTIGUOUS", wantCount: 10},
		{name: "intstring", chunks: `defaultTaskCount: "10", rangeConstraint: CONTIGUOUS`, wantCount: 10},
		{name: "format string", chunks: `defaultTaskCount: "{{Param.ChunkSize}}", rangeConstraint: CONTIGUOUS`,
			wantCountEx: "{{Param.ChunkSize}}"},
		{name: "garbage", chunks: `defaultTaskCount: "ten", rangeConstraint: CONTIGUOUS`, wantErr: "defaultTaskCount"},
		{name: "runtime zero", chunks: "defaultTaskCount: 1, targetRuntimeSeconds: 0, rangeConstraint: CONTIGUOUS",
			wantCount: 1, wantTRS: new(0)},
		{name: "runtime format string",
			chunks:    `defaultTaskCount: 1, targetRuntimeSeconds: "{{Param.Runtime}}", rangeConstraint: CONTIGUOUS`,
			wantCount: 1, wantTRSEx: "{{Param.Runtime}}"},
		{name: "runtime garbage",
			chunks:  `defaultTaskCount: 1, targetRuntimeSeconds: "soon", rangeConstraint: CONTIGUOUS`,
			wantErr: "targetRuntimeSeconds"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpl, err := Parse([]byte(chunkTemplate(tc.chunks)), FormatYAML)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Parse err = %v, want one mentioning %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			c := tmpl.Steps[0].ParameterSpace.TaskParameterDefinitions[0].Chunks
			if c.DefaultTaskCount != tc.wantCount {
				t.Errorf("DefaultTaskCount = %d, want %d", c.DefaultTaskCount, tc.wantCount)
			}
			if got := deref(c.DefaultTaskCountExpr); got != tc.wantCountEx {
				t.Errorf("DefaultTaskCountExpr = %q, want %q", got, tc.wantCountEx)
			}
			if (c.TargetRuntimeSeconds == nil) != (tc.wantTRS == nil) ||
				(c.TargetRuntimeSeconds != nil && *c.TargetRuntimeSeconds != *tc.wantTRS) {
				t.Errorf("TargetRuntimeSeconds = %v, want %v", c.TargetRuntimeSeconds, tc.wantTRS)
			}
			if got := deref(c.TargetRuntimeSecondsExpr); got != tc.wantTRSEx {
				t.Errorf("TargetRuntimeSecondsExpr = %q, want %q", got, tc.wantTRSEx)
			}
		})
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func TestValidateChunks_FormatStringsAndBounds(t *testing.T) {
	tests := []struct {
		name    string
		chunks  string
		wantPtr string // "" => expect no errors
	}{
		{"job-scope reference", `defaultTaskCount: "{{Param.ChunkSize}}", rangeConstraint: CONTIGUOUS`, ""},
		{"host-scope reference", `defaultTaskCount: "{{Session.WorkingDirectory}}", rangeConstraint: CONTIGUOUS`,
			"/chunks/defaultTaskCount"},
		{"runtime zero is valid", "defaultTaskCount: 1, targetRuntimeSeconds: 0, rangeConstraint: CONTIGUOUS", ""},
		{"runtime negative", "defaultTaskCount: 1, targetRuntimeSeconds: -1, rangeConstraint: CONTIGUOUS",
			"/chunks/targetRuntimeSeconds"},
		{"runtime host-scope reference",
			`defaultTaskCount: 1, targetRuntimeSeconds: "{{Task.Param.Frame}}", rangeConstraint: CONTIGUOUS`,
			"/chunks/targetRuntimeSeconds"},
		{"literal zero count", "defaultTaskCount: 0, rangeConstraint: CONTIGUOUS", "/chunks/defaultTaskCount"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateYAML(t, chunkTemplate(tc.chunks))
			if tc.wantPtr == "" {
				if len(errs) != 0 {
					t.Fatalf("errors = %v, want none", errs)
				}
				return
			}
			if !strings.Contains(errs.Error(), tc.wantPtr) {
				t.Fatalf("errors = %v, want one at %s", errs, tc.wantPtr)
			}
		})
	}
}

// expandChunkInt must refuse a count it was never given, rather than
// defaulting to 1 and silently ignoring the user's chunk size.
func TestExpandChunkInt_RefusesAnUnresolvedCount(t *testing.T) {
	ps := &StepParameterSpace{TaskParameterDefinitions: []TaskParamDefinition{{
		Name: "Frame", Type: TaskParamTypeChunkInt, RangeExpr: new("1-10"),
		Chunks: &TaskChunks{DefaultTaskCountExpr: new("{{Param.ChunkSize}}"), RangeConstraint: "CONTIGUOUS"},
	}}}
	if _, err := ExpandParameterSpace(ps); err == nil || !strings.Contains(err.Error(), "not resolved") {
		t.Fatalf("ExpandParameterSpace err = %v, want a not-resolved error", err)
	}
}
