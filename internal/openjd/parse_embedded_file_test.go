// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

// Tests for embedded-file type parsing.
//
// Covered here:
//   - type: TEXT is decoded into EmbeddedFile.Type
//   - absent type leaves EmbeddedFile.Type as the zero value (empty string)
//   - both step-script and environment-script paths are exercised

import (
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

func TestParse_EmbeddedFile(t *testing.T) {
	type tc struct {
		name     string
		yaml     string
		wantType openjd.EmbeddedFileType
		getEF    func(*testing.T, *openjd.JobTemplate) openjd.EmbeddedFile
	}
	cases := []tc{
		{
			name: "type TEXT is parsed for step-script",
			yaml: `
specificationVersion: jobtemplate-2023-09
name: Job
steps:
  - name: Step1
    script:
      embeddedFiles:
        - name: script.sh
          type: TEXT
          data: "echo hi"
      actions:
        onRun:
          command: ./script.sh
`,
			wantType: openjd.EmbeddedFileTypeText,
			getEF: func(t *testing.T, tmpl *openjd.JobTemplate) openjd.EmbeddedFile {
				if len(tmpl.Steps) == 0 || tmpl.Steps[0].Script == nil {
					t.Fatal("expected step with script")
				}
				files := tmpl.Steps[0].Script.EmbeddedFiles
				if len(files) == 0 {
					t.Fatal("expected at least one embedded file")
				}
				return files[0]
			},
		},
		{
			name: "type absent leaves Type empty for step-script",
			yaml: `
specificationVersion: jobtemplate-2023-09
name: Job
steps:
  - name: Step1
    script:
      embeddedFiles:
        - name: script.sh
          data: "echo hi"
      actions:
        onRun:
          command: ./script.sh
`,
			wantType: "",
			getEF: func(t *testing.T, tmpl *openjd.JobTemplate) openjd.EmbeddedFile {
				if len(tmpl.Steps) == 0 || tmpl.Steps[0].Script == nil {
					t.Fatal("expected step with script")
				}
				files := tmpl.Steps[0].Script.EmbeddedFiles
				if len(files) == 0 {
					t.Fatal("expected at least one embedded file")
				}
				return files[0]
			},
		},
		{
			name: "type TEXT is parsed for job-environment-script",
			yaml: `
specificationVersion: jobtemplate-2023-09
name: Job
jobEnvironments:
  - name: Setup
    script:
      embeddedFiles:
        - name: config.txt
          type: TEXT
          data: "key=value"
      actions:
        onEnter:
          command: echo
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`,
			wantType: openjd.EmbeddedFileTypeText,
			getEF: func(t *testing.T, tmpl *openjd.JobTemplate) openjd.EmbeddedFile {
				if len(tmpl.JobEnvironments) == 0 || tmpl.JobEnvironments[0].Script == nil {
					t.Fatal("expected job environment with script")
				}
				files := tmpl.JobEnvironments[0].Script.EmbeddedFiles
				if len(files) == 0 {
					t.Fatal("expected at least one embedded file in environment script")
				}
				return files[0]
			},
		},
		{
			name: "type absent leaves Type empty for job-environment-script",
			yaml: `
specificationVersion: jobtemplate-2023-09
name: Job
jobEnvironments:
  - name: Setup
    script:
      embeddedFiles:
        - name: config.txt
          data: "key=value"
      actions:
        onEnter:
          command: echo
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`,
			wantType: "",
			getEF: func(t *testing.T, tmpl *openjd.JobTemplate) openjd.EmbeddedFile {
				if len(tmpl.JobEnvironments) == 0 || tmpl.JobEnvironments[0].Script == nil {
					t.Fatal("expected job environment with script")
				}
				files := tmpl.JobEnvironments[0].Script.EmbeddedFiles
				if len(files) == 0 {
					t.Fatal("expected at least one embedded file in environment script")
				}
				return files[0]
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := mustParse(t, tc.yaml)
			ef := tc.getEF(t, tmpl)
			if ef.Type != tc.wantType {
				t.Errorf("EmbeddedFile.Type = %q, want %q", ef.Type, tc.wantType)
			}
		})
	}
}
