// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

// Tests for the wiring of checkTemplateExpressions into ValidateWithOptions,
// and the three format-string positions it covers beyond the base-spec ones
// (host requirement values, task-parameter range entries, action timeout).
//
// exprcheck_test.go (package openjd) covers checkTemplateExpressions and its
// helpers directly, at the unresolved-symbol-table level. This file covers
// the same ground through the public API (openjd.Validate /
// openjd.ValidateWithOptions), end to end from YAML, including that host
// requirements and range entries get format-string scope validation for
// base-spec templates as well as EXPR ones.

import (
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// The EXPR-declaring tests below call plain openjd.Validate, which runs the
// walk as production does, so they pin the wiring on the production path.

// TestValidate_HostRequirements_BaseSpec_OutOfScopeRejected pins that a
// base-spec template (no extensions declared) referencing
// {{Session.WorkingDirectory}} in a host requirement's attribute value is
// rejected at submission. Without format-string scope validation on host
// requirements, the reference would be accepted and resolve to nothing at
// run time.
func TestValidate_HostRequirements_BaseSpec_OutOfScopeRejected(t *testing.T) {
	tmpl := mustParse(t, `
specificationVersion: jobtemplate-2023-09
name: TestJob
steps:
  - name: Step1
    hostRequirements:
      attributes:
        - name: attr.custom.dir
          anyOf:
            - "{{Session.WorkingDirectory}}"
    script:
      actions:
        onRun:
          command: echo
`)
	errs := openjd.Validate(tmpl)
	if !containsMessage(errs, "Session.WorkingDirectory") {
		t.Fatalf("expected an out-of-scope error mentioning Session.WorkingDirectory; got: %v", errs)
	}
}

// TestValidate_HostRequirements_BaseSpec_InScopeAccepted is the positive
// counterpart: a Param. reference (in scope at ScopeJob) in the same
// position is accepted. Conformance fixtures
// 3.3.2--format-string-in-anyof.yaml and 3.3.2--format-string-in-allof.yaml
// pin the same shape against the full conformance suite; this is the
// unit-level version.
func TestValidate_HostRequirements_BaseSpec_InScopeAccepted(t *testing.T) {
	tmpl := mustParse(t, `
specificationVersion: jobtemplate-2023-09
name: TestJob
parameterDefinitions:
  - name: Software
    type: STRING
    default: maya
steps:
  - name: Step1
    hostRequirements:
      attributes:
        - name: attr.custom.software
          anyOf:
            - "{{Param.Software}}"
    script:
      actions:
        onRun:
          command: echo
`)
	errs := openjd.Validate(tmpl)
	if len(errs) != 0 {
		t.Fatalf("an in-scope Param. reference in a host requirement value must be accepted; got: %v", errs)
	}
}

// TestValidate_RangeEntries_BaseSpec_OutOfScopeRejected pins the same
// base-spec coverage for the "task-parameter range entries" position: a base-spec
// template with an out-of-scope reference in a literal range entry is
// rejected.
func TestValidate_RangeEntries_BaseSpec_OutOfScopeRejected(t *testing.T) {
	tmpl := mustParse(t, `
specificationVersion: jobtemplate-2023-09
name: TestJob
steps:
  - name: Step1
    parameterSpace:
      taskParameterDefinitions:
        - name: Shot
          type: STRING
          range: ["{{Session.WorkingDirectory}}"]
    script:
      actions:
        onRun:
          command: echo
`)
	errs := openjd.Validate(tmpl)
	if !containsMessage(errs, "Session.WorkingDirectory") {
		t.Fatalf("expected an out-of-scope error mentioning Session.WorkingDirectory; got: %v", errs)
	}
}

// TestValidate_RangeExpr_BaseSpec_OutOfScopeRejected pins the whole-field
// RangeExpr form of the range position: a base-spec template with
// range: "{{Session.WorkingDirectory}}" (a string scalar, not an array) is
// rejected. Checking only RangeList (the array form) would leave the same
// unresolvable reference open at RangeExpr (the scalar form).
func TestValidate_RangeExpr_BaseSpec_OutOfScopeRejected(t *testing.T) {
	tmpl := mustParse(t, `
specificationVersion: jobtemplate-2023-09
name: TestJob
steps:
  - name: Step1
    parameterSpace:
      taskParameterDefinitions:
        - name: Shot
          type: STRING
          range: "{{Session.WorkingDirectory}}"
    script:
      actions:
        onRun:
          command: echo
`)
	errs := openjd.Validate(tmpl)
	if !containsMessage(errs, "Session.WorkingDirectory") {
		t.Fatalf("expected an out-of-scope error mentioning Session.WorkingDirectory; got: %v", errs)
	}
}

// TestValidate_RangeExpr_BaseSpec_InScopeAccepted is the positive
// counterpart, using the shape sqi's render presets use
// (presets/sqi/*-render.yaml set range to "{{Param.Frames}}"): the check
// accepts it.
func TestValidate_RangeExpr_BaseSpec_InScopeAccepted(t *testing.T) {
	tmpl := mustParse(t, `
specificationVersion: jobtemplate-2023-09
name: TestJob
parameterDefinitions:
  - name: Frames
    type: STRING
    default: "1-10"
steps:
  - name: Step1
    parameterSpace:
      taskParameterDefinitions:
        - name: Shot
          type: INT
          range: "{{Param.Frames}}"
    script:
      actions:
        onRun:
          command: echo
`)
	errs := openjd.Validate(tmpl)
	if len(errs) != 0 {
		t.Fatalf("an in-scope Param. reference in a RangeExpr must be accepted; got: %v", errs)
	}
}

// TestValidate_EXPR_HostRequirements_OutOfScopeRejected is the EXPR-template
// counterpart of TestValidate_HostRequirements_BaseSpec_OutOfScopeRejected,
// exercised end to end through openjd.Parse and openjd.Validate rather than
// by constructing a *JobTemplate directly (as exprcheck_test.go's
// TestCheckTemplateExpressions_HostRequirements does). It checks that
// checkTemplateExpressions, not validateFormatString, rejects it once EXPR is
// declared.
func TestValidate_EXPR_HostRequirements_OutOfScopeRejected(t *testing.T) {
	tmpl := mustParse(t, `
specificationVersion: jobtemplate-2023-09
extensions:
- EXPR
name: TestJob
steps:
  - name: Step1
    hostRequirements:
      attributes:
        - name: attr.custom.dir
          anyOf:
            - "{{ Session.WorkingDirectory }}"
    script:
      actions:
        onRun:
          command: echo
`)
	errs := openjd.Validate(tmpl)
	if !containsMessage(errs, "Session") {
		t.Fatalf("expected an out-of-scope error mentioning Session; got: %v", errs)
	}
}

// TestValidate_EXPR_SkipsBaseSpecFormatStringPath pins the routing rule:
// once a template declares EXPR, the base-spec single-dotted-identifier
// reader (validateFormatString) does not also run against the same
// command/args/name positions, or a valid EXPR expression (arithmetic, here)
// would be rejected as "not a valid dotted identifier" although
// checkTemplateExpressions accepts it.
func TestValidate_EXPR_SkipsBaseSpecFormatStringPath(t *testing.T) {
	tmpl := mustParse(t, `
specificationVersion: jobtemplate-2023-09
extensions:
- EXPR
name: TestJob
parameterDefinitions:
- name: X
  type: INT
  default: 5
steps:
- name: Step1
  script:
    actions:
      onRun:
        command: python
        args:
        - "-c"
        - "print(r'RESULT:{{ Param.X + 1 }}')"
`)
	errs := openjd.Validate(tmpl)
	if containsMessage(errs, "not a valid dotted identifier") {
		t.Fatalf("the base-spec dotted-identifier reader ran against an EXPR template; got: %v", errs)
	}
}
