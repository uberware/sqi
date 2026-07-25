// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

func TestValidate_OnRunCommandRequired(t *testing.T) {
	yaml := `
specificationVersion: jobtemplate-2023-09
name: NoCommandJob
steps:
  - name: Step1
    script:
      actions:
        onRun:
          args: ["nothing"]
`
	tmpl := mustParse(t, yaml)
	assertValidationContains(t, tmpl, "/steps/0/script/actions/onRun/command")
}

func TestValidate_EnvironmentActionCommandRequired(t *testing.T) {
	yaml := `
specificationVersion: jobtemplate-2023-09
name: EnvNoCommandJob
jobEnvironments:
  - name: Setup
    script:
      actions:
        onEnter:
          args: ["nothing"]
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`
	tmpl := mustParse(t, yaml)
	assertValidationContains(t, tmpl, "/jobEnvironments/0/script/actions/onEnter/command")
}

func TestValidate_StepScriptRequired(t *testing.T) {
	yaml := `
specificationVersion: jobtemplate-2023-09
name: NoScriptJob
steps:
  - name: Step1
    description: a step with no script at all
`
	tmpl := mustParse(t, yaml)
	assertValidationContains(t, tmpl, "/steps/0/script")
}

func TestValidate_EnvironmentOnEnterRequired(t *testing.T) {
	yaml := `
specificationVersion: jobtemplate-2023-09
name: OnExitOnlyJob
jobEnvironments:
  - name: Teardown
    script:
      actions:
        onExit:
          command: cleanup.sh
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`
	tmpl := mustParse(t, yaml)
	assertValidationContains(t, tmpl, "/jobEnvironments/0/script/actions/onEnter")
}

func TestValidate_EnvironmentNeedsScriptOrVariables(t *testing.T) {
	yaml := `
specificationVersion: jobtemplate-2023-09
name: EmptyEnvJob
jobEnvironments:
  - name: Nothing
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`
	tmpl := mustParse(t, yaml)
	assertValidationContains(t, tmpl, "at least one of script or variables")
}

func TestValidate_RangeConstraintRequired(t *testing.T) {
	yaml := `
specificationVersion: jobtemplate-2023-09
name: ChunkNoConstraintJob
extensions: [TASK_CHUNKING]
steps:
  - name: Step1
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: CHUNK[INT]
          range: "1-10"
          chunks:
            defaultTaskCount: 2
    script:
      actions:
        onRun:
          command: echo
`
	tmpl := mustParse(t, yaml)
	assertValidationContains(t, tmpl, "chunks/rangeConstraint")
}

func TestValidate_RangeConstraintValue(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		valid bool
	}{
		{"contiguous", "CONTIGUOUS", true},
		{"noncontiguous", "NONCONTIGUOUS", true},
		{"garbage", "FOO", false},
		{"lowercase", "contiguous", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			yaml := `
specificationVersion: jobtemplate-2023-09
name: ChunkConstraintJob
extensions: [TASK_CHUNKING]
steps:
  - name: Step1
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: CHUNK[INT]
          range: "1-10"
          chunks:
            defaultTaskCount: 2
            rangeConstraint: ` + tc.value + `
    script:
      actions:
        onRun:
          command: echo
`
			tmpl := mustParse(t, yaml)
			errs := openjd.Validate(tmpl)
			got := len(errs) == 0
			if got != tc.valid {
				t.Fatalf("valid = %v, want %v (errs: %v)", got, tc.valid, errs)
			}
		})
	}
}

// Structural host-requirement checks must survive EnforceLimits: false. Before
// this task they lived in validateHostRequirementLimits, reachable only from
// validateLimits, so disabling limits silently disabled correctness too.
func TestValidate_HostRequirementStructuralChecksSurviveDisabledLimits(t *testing.T) {
	for _, tc := range []struct {
		name string
		hr   string
		want string
	}{
		{
			name: "empty requirements block",
			hr:   "    hostRequirements: {}",
			want: "at least one amount or attribute",
		},
		{
			name: "attribute with neither anyOf nor allOf",
			hr: `    hostRequirements:
      attributes:
        - name: attr.worker.os.family`,
			want: "at least one of anyOf or allOf",
		},
		{
			name: "amount with a malformed capability name",
			hr: `    hostRequirements:
      amounts:
        - name: "not a valid name!"
          min: 1`,
			want: "amount",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			yaml := `
specificationVersion: jobtemplate-2023-09
name: HostReqJob
steps:
  - name: Step1
` + tc.hr + `
    script:
      actions:
        onRun:
          command: echo
`
			tmpl := mustParse(t, yaml)
			errs := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{EnforceLimits: false})
			found := false
			for _, e := range errs {
				if strings.Contains(e.Error(), tc.want) {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected an error containing %q with limits disabled; got: %v", tc.want, errs)
			}
		})
	}
}
