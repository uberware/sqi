// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// Value-bearing template fields whose contents flow into a running task —
// STRING/PATH task-parameter range values, environment variable values,
// embedded-file data, job-parameter defaults, and action args — must reject a
// non-scalar element rather than silently stringifying a nested mapping into
// "map[...]" and substituting that garbage into the task's command, script, or
// environment.
func TestParse_ValueFields_RejectNonScalar(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string // substring expected in the parse error; "" => expect success
	}{
		{
			name: "STRING range list element is a mapping",
			yaml: `
specificationVersion: jobtemplate-2023-09
name: TestJob
steps:
  - name: S
    parameterSpace:
      taskParameterDefinitions:
        - name: P
          type: STRING
          range:
            - good
            - bad: 1
    script:
      actions:
        onRun:
          command: echo
`,
			wantErr: "range",
		},
		{
			name: "environment variable value is a mapping",
			yaml: `
specificationVersion: jobtemplate-2023-09
name: TestJob
jobEnvironments:
  - name: Env1
    variables:
      KEY:
        nested: 1
steps:
  - name: S
    script:
      actions:
        onRun:
          command: echo
`,
			wantErr: "KEY",
		},
		{
			name: "embedded file data is a mapping",
			yaml: `
specificationVersion: jobtemplate-2023-09
name: TestJob
steps:
  - name: S
    script:
      embeddedFiles:
        - name: main
          type: TEXT
          data:
            nested: 1
      actions:
        onRun:
          command: echo
`,
			wantErr: "data",
		},
		{
			name: "job parameter default is a mapping",
			yaml: `
specificationVersion: jobtemplate-2023-09
name: TestJob
parameterDefinitions:
  - name: P
    type: STRING
    default:
      nested: 1
steps:
  - name: S
    script:
      actions:
        onRun:
          command: echo
`,
			wantErr: "default",
		},
		{
			name: "action args element is a mapping",
			yaml: `
specificationVersion: jobtemplate-2023-09
name: TestJob
steps:
  - name: S
    script:
      actions:
        onRun:
          command: echo
          args:
            - ok
            - bad: 1
`,
			wantErr: "args",
		},
		{
			name: "scalar values parse successfully",
			yaml: `
specificationVersion: jobtemplate-2023-09
name: TestJob
parameterDefinitions:
  - name: P
    type: STRING
    default: hello
jobEnvironments:
  - name: Env1
    variables:
      STR: value
      NUM: 7
steps:
  - name: S
    parameterSpace:
      taskParameterDefinitions:
        - name: F
          type: STRING
          range:
            - a
            - b
    script:
      embeddedFiles:
        - name: main
          type: TEXT
          data: |
            #!/usr/bin/env bash
            echo hi
      actions:
        onRun:
          command: "{{Task.File.main}}"
          args:
            - "--frame"
            - "{{Task.Param.F}}"
`,
			wantErr: "",
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
