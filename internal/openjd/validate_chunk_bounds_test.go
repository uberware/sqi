// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"strings"
	"testing"
)

func validateYAML(t *testing.T, tmpl string) ValidationErrors {
	t.Helper()
	parsed, err := Parse([]byte(tmpl), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return ValidateWithOptions(parsed, ValidateOptions{EnforceLimits: true})
}

func TestValidateChunkBounds(t *testing.T) {
	const contiguous = `
specificationVersion: jobtemplate-2023-09
name: J
extensions: [TASK_CHUNKING, SQI_CHUNK_BOUNDS]
steps:
  - name: Render
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: CHUNK[INT]
          range: "1-10"
          chunks: { defaultTaskCount: 10, rangeConstraint: CONTIGUOUS }
    script: { actions: { onRun: { command: r } } }
`
	const noncontiguous = `
specificationVersion: jobtemplate-2023-09
name: J
extensions: [TASK_CHUNKING, SQI_CHUNK_BOUNDS]
steps:
  - name: Render
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: CHUNK[INT]
          range: "1-10"
          chunks: { defaultTaskCount: 10, rangeConstraint: NONCONTIGUOUS }
    script: { actions: { onRun: { command: r } } }
`
	const missingChunking = `
specificationVersion: jobtemplate-2023-09
name: J
extensions: [SQI_CHUNK_BOUNDS]
steps:
  - name: Render
    script: { actions: { onRun: { command: r } } }
`
	tests := []struct {
		name      string
		tmpl      string
		wantErr   bool
		errSubstr string
	}{
		{name: "contiguous ok", tmpl: contiguous, wantErr: false},
		{name: "noncontiguous rejected", tmpl: noncontiguous, wantErr: true, errSubstr: "CONTIGUOUS"},
		{name: "requires task chunking", tmpl: missingChunking, wantErr: true, errSubstr: "TASK_CHUNKING"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateYAML(t, tc.tmpl)
			if tc.wantErr && len(errs) == 0 {
				t.Fatal("expected validation error, got none")
			}
			if !tc.wantErr && len(errs) != 0 {
				t.Fatalf("expected no error, got %v", errs)
			}
			if tc.wantErr && !strings.Contains(errs.Error(), tc.errSubstr) {
				t.Errorf("error %q does not contain %q", errs.Error(), tc.errSubstr)
			}
		})
	}
}
