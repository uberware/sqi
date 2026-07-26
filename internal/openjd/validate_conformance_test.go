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

// TestValidate_ReservedCapabilityValueChecksSurviveDisabledLimits guards the
// reserved-value half of the host-requirement split: validateReservedAmounts
// and validateReservedAttributes are value-domain checks with no size or
// count component (a template asking for vcpu: 0 is malformed, not
// oversized), so they must run even when EnforceLimits is false. Before this
// split they were only reachable through the gated validateHostRequirementLimits.
func TestValidate_ReservedCapabilityValueChecksSurviveDisabledLimits(t *testing.T) {
	for _, tc := range []struct {
		name string
		hr   string
		want string
	}{
		{
			// min: 0 is VALID (see validateAmountBounds) — the structural check
			// that must survive limits being disabled is the non-negative bound.
			name: "amount with a negative min",
			hr: `    hostRequirements:
      amounts:
        - name: amount.worker.vcpu
          min: -1`,
			want: "must be non-negative",
		},
		{
			name: "reserved attribute with a disallowed value",
			hr: `    hostRequirements:
      attributes:
        - name: attr.worker.os.family
          anyOf: [plan9]`,
			want: "not allowed for reserved attribute",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			yaml := `
specificationVersion: jobtemplate-2023-09
name: ReservedValueJob
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

// TestValidate_ReservedCapabilityValueChecksNotDoubleReported is the
// double-reporting guard: under EnforceLimits: true, a reserved amount below
// its minimum and a reserved attribute with a disallowed value must each be
// reported exactly once, at their specific pointer -- not twice from both
// the always-run structural path and the gated limits path.
func TestValidate_ReservedCapabilityValueChecksNotDoubleReported(t *testing.T) {
	yaml := `
specificationVersion: jobtemplate-2023-09
name: ReservedDoubleReportJob
steps:
  - name: Step1
    hostRequirements:
      amounts:
        - name: amount.worker.vcpu
          min: -1
      attributes:
        - name: attr.worker.os.family
          anyOf: [plan9]
    script:
      actions:
        onRun:
          command: echo
`
	tmpl := mustParse(t, yaml)
	errs := openjd.Validate(tmpl) // EnforceLimits: true

	countAt := func(pointer string) int {
		n := 0
		for _, e := range errs {
			if e.Pointer == pointer {
				n++
			}
		}
		return n
	}

	const amountPtr = "/steps/0/hostRequirements/amounts/0/min"
	const attrPtr = "/steps/0/hostRequirements/attributes/0/anyOf/0"

	if n := countAt(amountPtr); n != 1 {
		t.Errorf("expected exactly 1 error at %s, got %d; errs: %v", amountPtr, n, errs)
	}
	if n := countAt(attrPtr); n != 1 {
		t.Errorf("expected exactly 1 error at %s, got %d; errs: %v", attrPtr, n, errs)
	}
}

// TestValidate_HostRequirementCapabilityNameLengthStaysGated proves the move
// took only the reserved-value checks and left a genuinely quantitative cap
// (capability name length) behind the EnforceLimits gate. The name here is
// not a reserved capability name, so only the length cap could fire.
func TestValidate_HostRequirementCapabilityNameLengthStaysGated(t *testing.T) {
	longName := "amount." + strings.Repeat("x", 101)
	yaml := `
specificationVersion: jobtemplate-2023-09
name: CapNameLenJob
steps:
  - name: Step1
    hostRequirements:
      amounts:
        - name: ` + longName + `
          min: 5
    script:
      actions:
        onRun:
          command: echo
`
	tmpl := mustParse(t, yaml)

	errs := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{EnforceLimits: false})
	for _, e := range errs {
		if strings.Contains(e.Error(), "at most 100 characters") {
			t.Fatalf("expected the capability name length cap to stay gated with limits disabled; got: %v", errs)
		}
	}

	errs = openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{EnforceLimits: true})
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "at most 100 characters") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the capability name length cap to fire with limits enabled; got: %v", errs)
	}
}
