// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// Integer template fields (minLength/maxLength, action timeout, chunk
// defaultTaskCount/targetRuntimeSeconds, cancelation notifyPeriodInSeconds) are
// typed as plain integers and hold no template references, so a non-integer
// value is malformed. Parsing must reject it rather than silently coercing it
// to 0 — a coerced zero passes validation and quietly drops the limit/timeout.
func TestParse_IntFields_RejectNonInteger(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string // substring expected in the parse error; "" => expect success
	}{
		{
			name: "minLength is a mapping",
			yaml: paramYAML(`minLength:
      bad: 1`),
			wantErr: "minLength",
		},
		{
			name:    "maxLength is a non-numeric string",
			yaml:    paramYAML(`maxLength: "abc"`),
			wantErr: "maxLength",
		},
		{
			name:    "minLength integer ok",
			yaml:    paramYAML(`minLength: 2`),
			wantErr: "",
		},
		{
			name:    "minLength quoted integer ok",
			yaml:    paramYAML(`minLength: "2"`),
			wantErr: "",
		},
		{
			name: "action timeout is a mapping",
			yaml: `
specificationVersion: jobtemplate-2023-09
name: TestJob
steps:
  - name: S
    script:
      actions:
        onRun:
          command: echo
          timeout:
            bad: 1
`,
			wantErr: "timeout",
		},
		{
			name: "action timeout integer ok",
			yaml: `
specificationVersion: jobtemplate-2023-09
name: TestJob
steps:
  - name: S
    script:
      actions:
        onRun:
          command: echo
          timeout: 3600
`,
			wantErr: "",
		},
		{
			name: "chunk defaultTaskCount is a sequence",
			yaml: `
specificationVersion: jobtemplate-2023-09
name: TestJob
extensions: [TASK_CHUNKING]
steps:
  - name: S
    parameterSpace:
      taskParameterDefinitions:
        - name: F
          type: CHUNK[INT]
          range: "1-10"
          chunks:
            defaultTaskCount:
              - 1
    script:
      actions:
        onRun:
          command: echo
`,
			wantErr: "defaultTaskCount",
		},
		{
			name: "cancelation notifyPeriodInSeconds is a mapping",
			yaml: `
specificationVersion: jobtemplate-2023-09
name: TestJob
steps:
  - name: S
    script:
      actions:
        onRun:
          command: echo
          cancelation:
            mode: NOTIFY_THEN_TERMINATE
            notifyPeriodInSeconds:
              bad: 1
`,
			wantErr: "notifyPeriodInSeconds",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := openjd.Parse([]byte(tc.yaml), openjd.FormatYAML)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected successful parse, got error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected parse error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected parse error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// paramYAML builds a minimal template with a single STRING job parameter whose
// body is the given YAML fragment (indented to sit under the parameter map).
func paramYAML(paramField string) string {
	return `
specificationVersion: jobtemplate-2023-09
name: TestJob
parameterDefinitions:
  - name: P
    type: STRING
    ` + paramField + `
steps:
  - name: S
    script:
      actions:
        onRun:
          command: echo
`
}
