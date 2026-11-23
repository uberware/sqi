// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

// Tests for validateEnvironments' duplicate-name check, in jobEnvironments and
// in a step's stepEnvironments. The major validation paths are covered in
// openjd_test.go.

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// assertValidationContains fails the test if none of the validation errors
// contain wantSubstr in their string representation.
func assertValidationContains(t *testing.T, tmpl *openjd.JobTemplate, wantSubstr string) {
	t.Helper()
	errs := openjd.Validate(tmpl)
	for _, e := range errs {
		if strings.Contains(e.Error(), wantSubstr) {
			return
		}
	}
	t.Fatalf("expected a validation error containing %q; got: %v", wantSubstr, errs)
}

func TestValidate_DuplicateJobEnvironmentName(t *testing.T) {
	yaml := `
specificationVersion: jobtemplate-2023-09
name: DupEnvJob
jobEnvironments:
  - name: Setup
    script:
      actions:
        onEnter:
          command: setup.sh
  - name: Setup
    script:
      actions:
        onEnter:
          command: setup2.sh
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`
	tmpl := mustParse(t, yaml)
	assertValidationContains(t, tmpl, "duplicate environment name")
}

func TestValidate_DuplicateStepEnvironmentName(t *testing.T) {
	yaml := `
specificationVersion: jobtemplate-2023-09
name: DupStepEnvJob
steps:
  - name: Step1
    stepEnvironments:
      - name: EnvA
        script:
          actions:
            onEnter:
              command: a.sh
      - name: EnvA
        script:
          actions:
            onEnter:
              command: a2.sh
    script:
      actions:
        onRun:
          command: echo
`
	tmpl := mustParse(t, yaml)
	assertValidationContains(t, tmpl, "duplicate environment name")
}
