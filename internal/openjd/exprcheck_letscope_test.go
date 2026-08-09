// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"fmt"
	"testing"
)

// ─── section 3.6.2's scope table, transcribed ──────────────────────────────
//
// Template Schemas section 3.6.2 gives, per let: location, the set of symbols
// a binding's own expression MAY reference. Design spec section 2 transcribes
// the table (docs/superpowers/specs/2026-08-08-expr-let-bindings-design.md);
// this file is that table turned into assertions, direct against the
// primary source (third_party/openjd-specifications/wiki/2023-09-Template-
// Schemas.md section 3.6.2), not the transcription.
//
// Each location gets one test with one table: a row per symbol the row
// GRANTS, asserting it resolves with no error, and a row per symbol the row
// WITHHOLDS, asserting it is rejected at the binding's own pointer -- the
// half most easily skipped, and the half that catches a scope wired too
// wide. Every withheld symbol is also DECLARED somewhere else in the same
// template (a sibling step, a sibling environment, or a different scope of
// the same element) so a withhold failure is provably about SCOPE, not about
// the symbol simply not existing anywhere in the document.

// letScopeCase is one row of the table: a symbol reference tried verbatim as
// a let binding's right-hand side, and whether that location's scope grants
// or withholds it.
type letScopeCase struct {
	// name identifies the subtest; it is always the symbol under test.
	name string
	// binding is the let binding's right-hand side, e.g. "Param.X".
	binding string
	// wantErr is true for a withheld symbol (must be rejected at the
	// binding's own pointer) and false for a granted one (must resolve).
	wantErr bool
}

// runLetScopeCases builds a template from tmplFmt -- exactly one "%s",
// filled with each case's binding -- for every case in cases, and asserts
// checkTemplateExpressions accepts a granted symbol with zero errors and
// rejects a withheld one with exactly one error at wantPtr, the let
// binding's own pointer (checkLetBindings' doc comment: a failed binding
// "reports at its own pointer").
func runLetScopeCases(t *testing.T, tmplFmt, wantPtr string, cases []letScopeCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tmpl := mustParseEXPR(t, fmt.Sprintf(tmplFmt, c.binding))
			errs := checkTemplateExpressions(tmpl, nil)
			if c.wantErr {
				if len(errs) != 1 {
					t.Fatalf("checkTemplateExpressions(%s) = %v, want exactly one error",
						c.binding, errs)
				}
				if errs[0].Pointer != wantPtr {
					t.Errorf("error at %q, want %q", errs[0].Pointer, wantPtr)
				}
				return
			}
			if len(errs) != 0 {
				t.Errorf("checkTemplateExpressions(%s) = %v, want no errors", c.binding, errs)
			}
		})
	}
}

// TestLetScope_StepTemplateLet is section 3.6.2's <StepTemplate>.let row
// (ScopeStepTemplate, exprcheck.go's checkStepExpressions -- the step's own
// let: block, evaluated before hostRequirements/parameterSpace/script/
// stepEnvironments).
//
// Grants: Param.*, RawParam.*, Job.Name, Step.Name.
// Withholds: Task.Param.*, Task.RawParam.* (no task exists yet at
// submission), Task.File.* (the step's own script declares one, unreachable
// here anyway), Session.* (no host exists yet), Env.File.* (a sibling job
// environment declares one, unreachable from a step-template position).
func TestLetScope_StepTemplateLet(t *testing.T) {
	const tmplFmt = `specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: TestJob
parameterDefinitions:
- name: X
  type: STRING
  default: hello
jobEnvironments:
- name: JobEnv1
  script:
    embeddedFiles:
    - name: f
      type: TEXT
      data: "env file"
    actions:
      onEnter:
        command: echo
steps:
- name: Step1
  let:
  - result = %s
  parameterSpace:
    taskParameterDefinitions:
    - name: F
      type: INT
      range: "1-3"
  script:
    embeddedFiles:
    - name: f
      type: TEXT
      data: "step file"
    actions:
      onRun:
        command: echo
        args: ["hi"]
`
	runLetScopeCases(t, tmplFmt, "/steps/0/let/0", []letScopeCase{
		{name: "grant/Param.X", binding: "Param.X"},
		{name: "grant/RawParam.X", binding: "RawParam.X"},
		{name: "grant/Job.Name", binding: "Job.Name"},
		{name: "grant/Step.Name", binding: "Step.Name"},
		{name: "withhold/Task.Param.F", binding: "Task.Param.F", wantErr: true},
		{name: "withhold/Task.RawParam.F", binding: "Task.RawParam.F", wantErr: true},
		{name: "withhold/Task.File.f", binding: "Task.File.f", wantErr: true},
		{name: "withhold/Session.WorkingDirectory", binding: "Session.WorkingDirectory", wantErr: true},
		{name: "withhold/Env.File.f", binding: "Env.File.f", wantErr: true},
	})
}

// TestLetScope_StepScriptLet is section 3.6.2's <StepScript>.let row
// (ScopeStepScript, checkStepExpressions -- the step SCRIPT's own let:
// block).
//
// Grants everything <StepTemplate>.let does, plus Task.Param.*,
// Task.RawParam.*, Task.File.* (the task now exists) and Session.* (a host
// now exists).
// Withholds: Env.File.* -- a sibling job environment declares "f", but
// Env.File. is a different environment's own embedded-file family and is
// never in a step script's scopeFamilies.
func TestLetScope_StepScriptLet(t *testing.T) {
	const tmplFmt = `specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: TestJob
parameterDefinitions:
- name: X
  type: STRING
  default: hello
jobEnvironments:
- name: JobEnv1
  script:
    embeddedFiles:
    - name: f
      type: TEXT
      data: "env file"
    actions:
      onEnter:
        command: echo
steps:
- name: Step1
  parameterSpace:
    taskParameterDefinitions:
    - name: F
      type: INT
      range: "1-3"
  script:
    let:
    - result = %s
    embeddedFiles:
    - name: f
      type: TEXT
      data: "step file"
    actions:
      onRun:
        command: echo
        args: ["hi"]
`
	runLetScopeCases(t, tmplFmt, "/steps/0/script/let/0", []letScopeCase{
		{name: "grant/Param.X", binding: "Param.X"},
		{name: "grant/RawParam.X", binding: "RawParam.X"},
		{name: "grant/Job.Name", binding: "Job.Name"},
		{name: "grant/Step.Name", binding: "Step.Name"},
		{name: "grant/Task.Param.F", binding: "Task.Param.F"},
		{name: "grant/Task.RawParam.F", binding: "Task.RawParam.F"},
		{name: "grant/Task.File.f", binding: "Task.File.f"},
		{name: "grant/Session.WorkingDirectory", binding: "Session.WorkingDirectory"},
		{name: "withhold/Env.File.f", binding: "Env.File.f", wantErr: true},
	})
}

// TestLetScope_EnvironmentScriptLet_JobEnvironment is section 3.6.2's
// <EnvironmentScript>.let row (ScopeJobEnvironment, checkEnvironmentExpressions
// called for tmpl.JobEnvironments) at a JOB-level environment -- one with no
// enclosing step.
//
// Grants: Param.*, RawParam.*, Job.Name, Session.*, Env.File.* (this
// environment's own embedded files).
// Withholds: Step.Name (this is the row's own reason, per the wiki note: a
// job environment has no enclosing step) and Task.Param.* (a sibling step
// declares task parameter F, but no task exists from a job environment's
// point of view either).
func TestLetScope_EnvironmentScriptLet_JobEnvironment(t *testing.T) {
	const tmplFmt = `specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: TestJob
parameterDefinitions:
- name: X
  type: STRING
  default: hello
jobEnvironments:
- name: JobEnv1
  script:
    let:
    - result = %s
    embeddedFiles:
    - name: f
      type: TEXT
      data: "env file"
    actions:
      onEnter:
        command: echo
steps:
- name: Step1
  parameterSpace:
    taskParameterDefinitions:
    - name: F
      type: INT
      range: "1-3"
  script:
    actions:
      onRun:
        command: echo
        args: ["hi"]
`
	runLetScopeCases(t, tmplFmt, "/jobEnvironments/0/script/let/0", []letScopeCase{
		{name: "grant/Param.X", binding: "Param.X"},
		{name: "grant/RawParam.X", binding: "RawParam.X"},
		{name: "grant/Job.Name", binding: "Job.Name"},
		{name: "grant/Session.WorkingDirectory", binding: "Session.WorkingDirectory"},
		{name: "grant/Env.File.f", binding: "Env.File.f"},
		{name: "withhold/Step.Name", binding: "Step.Name", wantErr: true},
		{name: "withhold/Task.Param.F", binding: "Task.Param.F", wantErr: true},
	})
}

// TestLetScope_EnvironmentScriptLet_StepEnvironment is section 3.6.2's
// <EnvironmentScript>.let row again, this time at a STEP-level environment.
//
// The row itself does not list Step.Name among what it grants, and its note
// explains why: EnvironmentScript "may also appear in a Job-level
// environment or a standalone environment template where no enclosing Step
// exists" -- the row is written once for one element type used in three
// places, and the omission is there to cover the two placements that have no
// step, not to strip Step.Name from the one placement that does.
//
// RULING (design spec section 2.2): a step environment's own let: KEEPS
// Step.Name. Section 7.3.1, not 3.6.2's note, governs where Step.Name is
// legal, and E2 already ships ScopeStepEnvironment differing from
// ScopeJobEnvironment by exactly that symbol (scope.go's scopeFixed) for
// every OTHER position in this same element -- variables, actions,
// embeddedFiles data. Reading 3.6.2's omission as also stripping Step.Name
// from a step environment's let would make that one position disagree with
// every sibling position in the same element, for a reason (the note) that
// does not apply to it: a step environment's enclosing step unambiguously
// exists. The only fixture on this row,
// 7.3.1--step-name-in-job-environment-let.invalid.yaml, is a JOB
// environment and does not settle -- and is consistent with -- the step case
// this test pins.
//
// Grants: everything the job-environment row grants, plus Step.Name.
// Withholds: Task.Param.* -- a sibling task parameter (F) exists on the
// enclosing step, but this scope's family list still does not include it (a
// step environment's script does not run per-task).
func TestLetScope_EnvironmentScriptLet_StepEnvironment(t *testing.T) {
	const tmplFmt = `specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: TestJob
parameterDefinitions:
- name: X
  type: STRING
  default: hello
steps:
- name: Step1
  parameterSpace:
    taskParameterDefinitions:
    - name: F
      type: INT
      range: "1-3"
  stepEnvironments:
  - name: StepEnv1
    script:
      let:
      - result = %s
      embeddedFiles:
      - name: f
        type: TEXT
        data: "env file"
      actions:
        onEnter:
          command: echo
  script:
    actions:
      onRun:
        command: echo
        args: ["hi"]
`
	runLetScopeCases(t, tmplFmt, "/steps/0/stepEnvironments/0/script/let/0", []letScopeCase{
		{name: "grant/Param.X", binding: "Param.X"},
		{name: "grant/RawParam.X", binding: "RawParam.X"},
		{name: "grant/Job.Name", binding: "Job.Name"},
		{name: "grant/Session.WorkingDirectory", binding: "Session.WorkingDirectory"},
		{name: "grant/Env.File.f", binding: "Env.File.f"},
		{name: "grant/Step.Name", binding: "Step.Name"},
		{name: "withhold/Task.Param.F", binding: "Task.Param.F", wantErr: true},
	})
}

// TestLetScope_SimpleAction is section 3.6.2's third row, which sqi cannot
// test because SimpleAction -- a step's bash:/powershell: shorthand -- is a
// FEATURE_BUNDLE_1 element and is not modeled at all (design spec 1.1).
//
// The row is transcribed here so that whenever FEATURE_BUNDLE_1 is taken up,
// its rules are already written down rather than re-derived from the spec:
//
//	<SimpleAction>.let may reference: Param.*, RawParam.*, Task.Param.*,
//	Task.RawParam.*, Session.*, Job.Name, Step.Name, step-level bindings, and
//	earlier bindings in the same block.
//	It may NOT reference Task.File.* -- the one place it differs from
//	<StepScript>.let.
//	Bound names are visible in: script, args.
func TestLetScope_SimpleAction(t *testing.T) {
	t.Skip("SimpleAction is a FEATURE_BUNDLE_1 element and is not modeled; see the doc comment")
}
