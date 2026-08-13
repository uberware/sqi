// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/openjd/expr"
)

// exprDeadlineTemplate declares EXPR and enough expression work for the
// deadline check to be reached.
const exprDeadlineTemplate = `
specificationVersion: jobtemplate-2023-09
extensions:
- EXPR
name: T
parameterDefinitions:
- name: N
  type: INT
  default: "3"
steps:
- name: S
  script:
    actions:
      onRun:
        command: echo
        args: ["{{ [x * 2 for x in range(100000)] }}"]
`

// exprDeadlineLetTemplate puts the expression work inside a let: block, which
// is the one position whose failure path APPENDS and CONTINUES rather than
// returning -- see checkLetBindings. A deadline recorded there must stop the
// block, not run every remaining binding past an expired deadline.
const exprDeadlineLetTemplate = `
specificationVersion: jobtemplate-2023-09
extensions:
- EXPR
name: T
steps:
- name: S
  let:
  - "a = [x * 2 for x in range(100000)]"
  - "b = [x * 2 for x in range(100000)]"
  - "c = [x * 2 for x in range(100000)]"
  script:
    actions:
      onRun:
        command: echo
        args: ["hi"]
`

// exprDeadlineSegmentTemplate puts the same work in a reference EMBEDDED in
// surrounding text, which is the checker's other format-string shape and its
// own conversion site: fmtstring.LoneRef is false, so checkFormatString walks
// segments and evaluates each reference in a loop rather than once.
const exprDeadlineSegmentTemplate = `
specificationVersion: jobtemplate-2023-09
extensions:
- EXPR
name: T
steps:
- name: S
  script:
    actions:
      onRun:
        command: echo
        args: ["pre {{ [x * 2 for x in range(100000)] }} post"]
`

// exprDeadlineResolveTemplate is the phase-2 RESOLVER's fixture: a step with
// both a let: block (which the resolver evaluates through the same
// checkLetBindings the checker uses) and a parameterSpace to resolve.
const exprDeadlineResolveTemplate = `
specificationVersion: jobtemplate-2023-09
extensions:
- EXPR
name: T
steps:
- name: S
  let:
  - "a = [x * 2 for x in range(100000)]"
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

// TestValidateWithBudget_DeadlineIsNotAValidationError is the central contract
// of sub-project H1: a wall-clock stop is the SERVER giving up, not a verdict
// that the template is invalid.
//
// A budget error is a ValidationError and becomes a 422. A deadline must not
// be, because the same body would validate on an idle machine — encoding it as
// "invalid" would make acceptance depend on machine load.
func TestValidateWithBudget_DeadlineIsNotAValidationError(t *testing.T) {
	tmpl, err := Parse([]byte(exprDeadlineTemplate), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	errs, ferr := ValidateWithBudget(tmpl, ValidateOptions{
		EnforceLimits:                        true,
		CheckEXPRExpressionsWhileUnsupported: true,
		Deadline:                             time.Now().Add(-time.Second), // expired
	})

	if ferr == nil {
		t.Fatal("ValidateWithBudget returned no error with an expired deadline; " +
			"the deadline is probably not reaching the evaluator")
	}
	if !errors.Is(ferr, expr.ErrDeadlineExceeded) {
		t.Fatalf("error = %v, want it to wrap expr.ErrDeadlineExceeded", ferr)
	}
	// And it must not ALSO have been reported as a validation error: that is
	// the half of the contract a caller turning errors into status codes
	// depends on.
	for _, e := range errs {
		if strings.Contains(strings.ToLower(e.Message), "deadline") {
			t.Errorf("deadline reported as a ValidationError at %s: %s", e.Pointer, e.Message)
		}
	}
}

// TestValidateWithBudget_DeadlineInEmbeddedSegmentIsNotAValidationError covers
// the second of checkFormatString's two conversion sites.
//
// The lone-reference path returns on the first failure; this one collects an
// error per reference and keeps walking the remaining segments, so it needs the
// same diversion for the same reason [checkLetBindings] does. A test that only
// exercised the lone-reference shape would leave this site free to convert a
// deadline into a 422.
func TestValidateWithBudget_DeadlineInEmbeddedSegmentIsNotAValidationError(t *testing.T) {
	tmpl, err := Parse([]byte(exprDeadlineSegmentTemplate), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	errs, ferr := ValidateWithBudget(tmpl, ValidateOptions{
		EnforceLimits:                        true,
		CheckEXPRExpressionsWhileUnsupported: true,
		Deadline:                             time.Now().Add(-time.Second), // expired
	})

	if ferr == nil {
		t.Fatal("ValidateWithBudget returned no error with an expired deadline; " +
			"checkFormatString's segment loop is probably still converting it")
	}
	if !errors.Is(ferr, expr.ErrDeadlineExceeded) {
		t.Fatalf("error = %v, want it to wrap expr.ErrDeadlineExceeded", ferr)
	}
	for _, e := range errs {
		if strings.Contains(strings.ToLower(e.Message), "deadline") {
			t.Errorf("deadline reported as a ValidationError at %s: %s", e.Pointer, e.Message)
		}
	}
}

// TestValidateWithBudget_DeadlineInLetBindingsStopsTheBlock covers the one
// conversion site that appends and continues.
//
// checkLetBindings evaluates each binding in turn and, on failure, records an
// error and moves to the next one — deliberately, so a single malformed line
// does not hide the rest of the block. For a deadline that behavior is exactly
// wrong: the whole point of a backstop is to stop, and running the remaining
// bindings past an expired deadline spends the time the deadline said had run
// out. So the block must yield the deadline through the budget and produce no
// per-binding validation errors at all.
func TestValidateWithBudget_DeadlineInLetBindingsStopsTheBlock(t *testing.T) {
	tmpl, err := Parse([]byte(exprDeadlineLetTemplate), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	errs, ferr := ValidateWithBudget(tmpl, ValidateOptions{
		EnforceLimits:                        true,
		CheckEXPRExpressionsWhileUnsupported: true,
		Deadline:                             time.Now().Add(-time.Second), // expired
	})

	if ferr == nil {
		t.Fatal("ValidateWithBudget returned no error with an expired deadline; " +
			"checkLetBindings is probably still converting it to a ValidationError")
	}
	if !errors.Is(ferr, expr.ErrDeadlineExceeded) {
		t.Fatalf("error = %v, want it to wrap expr.ErrDeadlineExceeded", ferr)
	}
	for _, e := range errs {
		if strings.HasPrefix(e.Pointer, "/steps/0/let") {
			t.Errorf("let binding reported a ValidationError past the deadline at %s: %s",
				e.Pointer, e.Message)
		}
	}
}

// TestValidateWithBudget_NoDeadlineNeverErrors pins that the second return
// value is reserved for deadlines alone: an ordinary invalid template produces
// ValidationErrors and a nil error, which is what makes ValidateWithOptions
// safe as a discarding wrapper.
func TestValidateWithBudget_NoDeadlineNeverErrors(t *testing.T) {
	tmpl, err := Parse([]byte(`
specificationVersion: jobtemplate-2023-09
name: T
parameterDefinitions:
- name: P
  type: NONSENSE
steps:
- name: S
  script:
    actions:
      onRun:
        command: echo
`), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	errs, ferr := ValidateWithBudget(tmpl, ValidateOptions{EnforceLimits: true})
	if ferr != nil {
		t.Fatalf("error = %v, want nil: only a deadline may produce one", ferr)
	}
	if len(errs) == 0 {
		t.Fatal("an unknown parameter type produced no ValidationErrors")
	}
}

// TestValidateWithOptions_UnchangedByBudget pins that the existing entry point
// keeps behaving exactly as before, since every caller but the submission path
// still uses it.
func TestValidateWithOptions_UnchangedByBudget(t *testing.T) {
	tmpl, err := Parse([]byte(exprDeadlineTemplate), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// No deadline set, so this must behave exactly as it did before H1.
	errs := ValidateWithOptions(tmpl, ValidateOptions{
		EnforceLimits:                        true,
		CheckEXPRExpressionsWhileUnsupported: true,
	})
	// The template declares EXPR, which is StatusInProgress, so it is rejected
	// by the status gate — the point is that it returns errors, not a panic or
	// a hang.
	if len(errs) == 0 {
		t.Fatal("an EXPR template was accepted while the extension is StatusInProgress")
	}
}

// TestCheckExpressionsAtSubmit_DeadlineIsNotASubmitValidationError covers
// phase 2 of the same contract.
//
// checkExpressionsAtSubmit turns the walk's ValidationErrors into a
// *SubmitValidationError — the CLIENT-FAULT channel, a 4xx. A deadline adds no
// ValidationError at all, so without an explicit check this function would see
// an empty error slice and return nil: a walk that stopped early reported as a
// clean re-check, after which submission proceeds on expressions nobody
// finished checking.
func TestCheckExpressionsAtSubmit_DeadlineIsNotASubmitValidationError(t *testing.T) {
	tmpl, err := Parse([]byte(exprDeadlineTemplate), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	budget := newTemplateBudget(ExprLimits{Deadline: time.Now().Add(-time.Second)})
	serr := checkExpressionsAtSubmit(tmpl, map[string]string{"N": "3"}, budget)
	if serr == nil {
		t.Fatal("checkExpressionsAtSubmit returned nil with an expired deadline; " +
			"a walk that stopped early was reported as a clean re-check")
	}
	if !errors.Is(serr, expr.ErrDeadlineExceeded) {
		t.Fatalf("error = %v, want it to wrap expr.ErrDeadlineExceeded", serr)
	}
	var sve *SubmitValidationError
	if errors.As(serr, &sve) {
		t.Errorf("error = %v, want NOT a *SubmitValidationError: a wall-clock stop "+
			"is not the submitter's fault", serr)
	}
}

// TestResolveParameterSpaceParams_DeadlineYieldsNoParameterSpace pins the third
// observation point.
//
// The resolver has no error channel — it returns (*StepParameterSpace,
// ValidationErrors) — and a deadline produces neither a ValidationError nor a
// complete space: the definition loop breaks on !b.ok(), leaving the rest of
// newDefs zero-valued. Returning that would be indistinguishable from success,
// and its caller would go on to expand a parameter space the resolver never
// finished building. So the space must be nil, and the breach must be readable
// from the shared budget.
func TestResolveParameterSpaceParams_DeadlineYieldsNoParameterSpace(t *testing.T) {
	tmpl, err := Parse([]byte(exprDeadlineResolveTemplate), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(tmpl.Steps) != 1 || tmpl.Steps[0].ParameterSpace == nil {
		t.Fatalf("fixture has no step parameter space to resolve")
	}
	step := tmpl.Steps[0]

	budget := newTemplateBudget(ExprLimits{Deadline: time.Now().Add(-time.Second)})
	ps, errs := ResolveParameterSpaceParams(tmpl, &step, step.ParameterSpace, nil, budget)

	if !errors.Is(budget.deadline(), expr.ErrDeadlineExceeded) {
		t.Fatalf("budget deadline = %v, want expr.ErrDeadlineExceeded", budget.deadline())
	}
	if ps != nil {
		t.Errorf("resolved parameter space = %+v, want nil: the walk stopped early", ps)
	}
	for _, e := range errs {
		if strings.Contains(strings.ToLower(e.Message), "deadline") {
			t.Errorf("deadline reported as a ValidationError at %s: %s", e.Pointer, e.Message)
		}
	}
}

// TestExprLimits_DeadlineSurvivesNormalization pins the failure the carrier
// choice invites: ExprLimits is normalized ([ExprLimits.orDefaults]) before any
// evaluation reads it, so a normalization that rebuilt the struct field by
// field rather than patching a copy would silently drop the deadline and every
// test above would still pass on a template that finishes quickly.
func TestExprLimits_DeadlineSurvivesNormalization(t *testing.T) {
	want := time.Unix(1_700_000_000, 0)
	got := ExprLimits{Deadline: want}.orDefaults()
	if !got.Deadline.Equal(want) {
		t.Fatalf("Deadline after orDefaults = %v, want %v", got.Deadline, want)
	}

	// It must also survive the budget that carries it to every metered
	// position, since that is where orDefaults is actually applied.
	if b := newTemplateBudget(ExprLimits{Deadline: want}); !b.limits.Deadline.Equal(want) {
		t.Fatalf("Deadline on a fresh budget = %v, want %v", b.limits.Deadline, want)
	}
	// And it must be handed to the evaluator as an option rather than merely
	// sitting on the struct. Options are opaque closures, so the count is what
	// is observable here; the deadline REACHING the meter is what
	// TestValidateWithBudget_DeadlineIsNotAValidationError proves.
	set := ExprLimits{Deadline: want}.orDefaults().evalOptions()
	unset := ExprLimits{}.orDefaults().evalOptions()
	if len(set) != len(unset)+1 {
		t.Fatalf("evalOptions returned %d options with a deadline and %d without; "+
			"want exactly one more", len(set), len(unset))
	}
}
