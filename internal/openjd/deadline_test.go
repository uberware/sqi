// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/openjd/expr"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
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
//
// THE LATER BINDINGS ARE SYNTACTICALLY BROKEN ON PURPOSE, and that is what makes
// the test's assertion able to tell "stopped" from "kept going quietly". A
// binding whose expression fails to PARSE appends a ValidationError before any
// evaluation happens, so recordDeadline never sees it and cannot swallow it: if
// checkLetBindings continued past the deadline instead of returning, bindings 1
// and 2 would each report at /steps/0/let/N. With three heavy-but-valid
// bindings -- the shape this fixture had before -- every later binding would
// trip the deadline again, be swallowed, and report nothing, so the assertion
// held either way and proved nothing.
const exprDeadlineLetTemplate = `
specificationVersion: jobtemplate-2023-09
extensions:
- EXPR
name: T
steps:
- name: S
  let:
  - "a = [x * 2 for x in range(100000)]"
  - "b = 1 +"
  - "c = 1 +"
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
//
// The SECOND reference is a syntax error for the same reason the let fixture's
// later bindings are: a parse failure is appended before any evaluation, so it
// is the one thing a swallowed deadline cannot hide. With a single reference --
// the shape this fixture had before -- checkFormatString's "return errs" and a
// "continue" were indistinguishable, because there was no later segment to keep
// going to.
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
        args: ["pre {{ [x * 2 for x in range(100000)] }} mid {{ 1 + }} post"]
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

// exprDeadlineResolveRangeExprTemplate is the resolver fixture that reaches
// resolveTaskParamDefinition, and it deliberately has NO let: block.
//
// exprDeadlineResolveTemplate (above) gives its step a let: block, so the
// walk trips inside stepLetSymbols and ResolveParameterSpaceParams' definition
// loop breaks before resolveTaskParamDefinition is ever entered. That fixture
// therefore proves nothing about the resolver's OWN two conversion sites. This
// one has no let: at all and a whole-field expression range, so the FIRST
// evaluation of the whole call happens inside resolveRangeExprField.
//
// The second definition's range is a syntax error on purpose: it is what makes
// "stopped" distinguishable from "kept going quietly". Reached, it appends a
// parse ValidationError at /parameterSpace/taskParameterDefinitions/1/range;
// not reached, it appends nothing.
const exprDeadlineResolveRangeExprTemplate = `
specificationVersion: jobtemplate-2023-09
extensions:
- EXPR
name: T
steps:
- name: S
  parameterSpace:
    taskParameterDefinitions:
    - name: F
      type: INT
      range: "{{ range(1, 4) }}"
    - name: G
      type: INT
      range: "{{ 1 + }}"
  script:
    actions:
      onRun:
        command: echo
        args: ["hi"]
`

// exprDeadlineResolveRangeListTemplate reaches the resolver's OTHER conversion
// site, resolveRangeListDefinition's per-entry append, which
// exprDeadlineResolveRangeExprTemplate cannot: a whole-field RangeExpr and a
// RangeList are separate branches of resolveTaskParamDefinition.
//
// The second entry is a syntax error for the same reason, one level down: it
// distinguishes a loop that stopped at the deadline from one that kept
// evaluating entries past it.
const exprDeadlineResolveRangeListTemplate = `
specificationVersion: jobtemplate-2023-09
extensions:
- EXPR
name: T
steps:
- name: S
  parameterSpace:
    taskParameterDefinitions:
    - name: F
      type: INT
      range:
      - "{{ 1 + 1 }}"
      - "{{ 1 + }}"
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
//
// It also asserts that the segment loop STOPPED, not merely that it stopped
// reporting -- see exprDeadlineSegmentTemplate for how its second, deliberately
// unparseable reference makes the difference observable.
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
		if strings.Contains(e.Pointer, "/args/") {
			t.Errorf("a later segment was evaluated past the deadline (%s: %s); "+
				"checkFormatString's segment loop must return, not continue", e.Pointer, e.Message)
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
//
// The "no /steps/0/let error" assertion below is only load-bearing because of
// how exprDeadlineLetTemplate is built -- bindings 1 and 2 fail to PARSE, which
// is reported before any evaluation and so cannot be swallowed by
// recordDeadline. Read that fixture's comment before changing it: with heavy but
// valid later bindings this assertion holds whether checkLetBindings returns or
// continues, and the test silently stops testing its own name.
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

// TestResolveParameterSpaceParams_DeadlineAtRangeExprPosition covers the
// resolver's whole-field conversion site (resolveTaskParamDefinition), which
// TestResolveParameterSpaceParams_DeadlineYieldsNoParameterSpace does not reach:
// its fixture's let: block trips the deadline first, so the definition loop
// breaks before that site runs at all.
//
// Two things must hold. The deadline must not become a ValidationError — the
// whole 503-not-422 contract — and the walk must STOP, which the second
// definition's deliberate syntax error makes observable: converted-and-continued,
// it reports a parse error at definition 1.
func TestResolveParameterSpaceParams_DeadlineAtRangeExprPosition(t *testing.T) {
	tmpl, err := Parse([]byte(exprDeadlineResolveRangeExprTemplate), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	step := tmpl.Steps[0]
	if len(step.Let) != 0 {
		t.Fatal("fixture has a let: block; it must not, or the walk trips before " +
			"resolveTaskParamDefinition is entered")
	}

	budget := newTemplateBudget(ExprLimits{Deadline: time.Now().Add(-time.Second)})
	ps, errs := ResolveParameterSpaceParams(tmpl, &step, step.ParameterSpace, nil, budget)

	if !errors.Is(budget.deadline(), expr.ErrDeadlineExceeded) {
		t.Fatalf("budget deadline = %v, want expr.ErrDeadlineExceeded: the resolver's "+
			"whole-field conversion site is not diverting it", budget.deadline())
	}
	if ps != nil {
		t.Errorf("resolved parameter space = %+v, want nil: the walk stopped early", ps)
	}
	for _, e := range errs {
		if strings.Contains(strings.ToLower(e.Message), "deadline") {
			t.Errorf("deadline reported as a ValidationError at %s: %s", e.Pointer, e.Message)
		}
		if strings.HasPrefix(e.Pointer, "/parameterSpace/taskParameterDefinitions/1") {
			t.Errorf("definition 1 was resolved past the deadline (%s: %s); the backstop "+
				"must stop the walk, not merely stop reporting", e.Pointer, e.Message)
		}
	}
}

// TestResolveParameterSpaceParams_DeadlineAtRangeListPosition covers the
// resolver's per-entry conversion site (resolveRangeListDefinition), the other
// branch of resolveTaskParamDefinition, with the same two assertions.
func TestResolveParameterSpaceParams_DeadlineAtRangeListPosition(t *testing.T) {
	tmpl, err := Parse([]byte(exprDeadlineResolveRangeListTemplate), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	step := tmpl.Steps[0]
	if n := len(step.ParameterSpace.TaskParameterDefinitions[0].RangeList); n != 2 {
		t.Fatalf("fixture definition 0 has %d range list entries, want 2", n)
	}

	budget := newTemplateBudget(ExprLimits{Deadline: time.Now().Add(-time.Second)})
	ps, errs := ResolveParameterSpaceParams(tmpl, &step, step.ParameterSpace, nil, budget)

	if !errors.Is(budget.deadline(), expr.ErrDeadlineExceeded) {
		t.Fatalf("budget deadline = %v, want expr.ErrDeadlineExceeded: the resolver's "+
			"per-entry conversion site is not diverting it", budget.deadline())
	}
	if ps != nil {
		t.Errorf("resolved parameter space = %+v, want nil: the walk stopped early", ps)
	}
	for _, e := range errs {
		if strings.Contains(strings.ToLower(e.Message), "deadline") {
			t.Errorf("deadline reported as a ValidationError at %s: %s", e.Pointer, e.Message)
		}
		if strings.HasPrefix(e.Pointer, "/parameterSpace/taskParameterDefinitions/0/range/1") {
			t.Errorf("range entry 1 was resolved past the deadline (%s: %s); the backstop "+
				"must stop the loop, not merely stop reporting", e.Pointer, e.Message)
		}
	}
}

// exprDeadlineSubmitTemplate is the fixture for the Submit-level tests: a
// valid EXPR template whose one expression is a CALL.
//
// The call matters. A bare literal such as "{{ [1, 2, 3] }}" performs no
// meter.charge at all, so the first-charge clock sample never fires and the
// position resolves successfully however long ago the deadline passed -- a
// fixture built on one proves nothing while looking like it proves everything.
const exprDeadlineSubmitTemplate = `
specificationVersion: jobtemplate-2023-09
extensions:
- EXPR
name: DeadlineSubmitJob
steps:
- name: S
  script:
    actions:
      onRun:
        command: echo
        args: ["{{ len([1, 2, 3]) }}"]
`

// TestSubmit_DeadlineIsNotASubmitValidationError drives H1's contract through
// the public [Submitter.Submit] API, which is the only place the whole chain is
// observable: the configured instant reaching both phase-2 budgets, phase 1
// calling [ValidateWithBudget] rather than [ValidateWithOptions], and the
// breach coming back as a plain error rather than as the client-fault type.
//
// It flips the EXPR registry entry to StatusSupported for the duration of the
// test, exactly as TestSubmit_PhaseDistinction_ThroughRealSubmit does and for
// the same reason: while EXPR is StatusInProgress, validateExtensions rejects
// every EXPR-declaring template before a single expression is evaluated, so no
// submission can reach the meter at all. That is also why H1's deadline is
// INERT in production today -- it becomes live when sub-project H2 flips that
// status, which is the whole point of landing the bound first.
//
// The message assertion is the load-bearing half. Both phase 1 and phase 2 walk
// the same positions, so a deadline trips in whichever runs first: if phase 1
// still called ValidateWithOptions -- discarding the channel the breach arrives
// on -- the walk would stop SILENTLY, phase 1 would report success, and phase 2
// would then produce a deadline error of its own. The test would still see an
// ErrDeadlineExceeded and pass while the exact defect it exists to catch was
// present. The phase-1 wording is what distinguishes them.
func TestSubmit_DeadlineIsNotASubmitValidationError(t *testing.T) {
	prevEXPR := registry["EXPR"]
	supported := prevEXPR
	supported.Status = StatusSupported
	registry["EXPR"] = supported
	t.Cleanup(func() { registry["EXPR"] = prevEXPR })

	ctx := t.Context()
	st := fake.New()
	farm, err := st.CreateFarm(ctx, store.Farm{ID: uuid.NewString(), Name: "h1-farm"})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err := st.CreateQueue(ctx, store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: "h1-queue"})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}

	sub := NewSubmitter(st)
	_, err = sub.Submit(ctx, exprDeadlineSubmitTemplate, store.TemplateFormatYAML, SubmitOptions{
		FarmID:   farm.ID,
		QueueID:  queue.ID,
		Deadline: time.Now().Add(-time.Second), // already expired
	})

	if err == nil {
		t.Fatal("Submit accepted a template with an already-expired deadline; " +
			"SubmitOptions.Deadline is probably not reaching either budget")
	}
	if !errors.Is(err, expr.ErrDeadlineExceeded) {
		t.Fatalf("error = %v, want it to wrap expr.ErrDeadlineExceeded", err)
	}
	var sve *SubmitValidationError
	if errors.As(err, &sve) {
		t.Errorf("error = %v, want NOT a *SubmitValidationError: a wall-clock stop is "+
			"the server giving up, not the submitter's fault, and that type is what "+
			"internal/api turns into a 4xx", err)
	}
	if !strings.Contains(err.Error(), "openjd: submit: validation:") {
		t.Errorf("error = %q, want phase 1's wording: the breach must surface from "+
			"prepareTemplate's ValidateWithBudget call, not from the phase-2 re-check "+
			"that runs only because phase 1 discarded it", err)
	}
}

// TestSubmit_NoDeadlineIsUnchanged pins the other direction at the same level:
// with SubmitOptions.Deadline left zero -- every caller in this repo that is
// not a submission handler -- the pipeline behaves exactly as it did before H1.
func TestSubmit_NoDeadlineIsUnchanged(t *testing.T) {
	prevEXPR := registry["EXPR"]
	supported := prevEXPR
	supported.Status = StatusSupported
	registry["EXPR"] = supported
	t.Cleanup(func() { registry["EXPR"] = prevEXPR })

	ctx := t.Context()
	st := fake.New()
	farm, err := st.CreateFarm(ctx, store.Farm{ID: uuid.NewString(), Name: "h1-farm"})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err := st.CreateQueue(ctx, store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: "h1-queue"})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}

	sub := NewSubmitter(st)
	res, err := sub.Submit(ctx, exprDeadlineSubmitTemplate, store.TemplateFormatYAML, SubmitOptions{
		FarmID: farm.ID, QueueID: queue.ID,
	})
	if err != nil {
		t.Fatalf("Submit with no deadline: %v", err)
	}
	if res == nil || res.Job.ID == "" {
		t.Fatal("Submit with no deadline returned no job")
	}
}

// TestPrepareTemplate_DeadlineReachesTheResolverBudget covers the budget the
// Submit-level test above cannot observe.
//
// Phase 1 trips first for an EXPR template, so nothing downstream of it is
// exercised there. The resolver budget is handed to every step's
// ResolveParameterSpaceParams call in Submit's step loop, where a deadline is
// the difference between stopping and resolving a parameter space nobody
// finished checking (see TestResolveParameterSpaceParams_DeadlineYieldsNoParameterSpace).
// prepareTemplate returns it, so the copy can be asserted directly with a
// deadline that is nowhere near expiring.
func TestPrepareTemplate_DeadlineReachesTheResolverBudget(t *testing.T) {
	const plainTemplate = `
specificationVersion: jobtemplate-2023-09
name: PlainJob
steps:
- name: S
  script:
    actions:
      onRun:
        command: echo
        args: ["hi"]
`
	want := time.Now().Add(time.Hour)
	sub := NewSubmitter(fake.New())
	_, _, resolverBudget, err := sub.prepareTemplate(
		t.Context(), plainTemplate, store.TemplateFormatYAML, nil, want,
	)
	if err != nil {
		t.Fatalf("prepareTemplate: %v", err)
	}
	if !resolverBudget.limits.Deadline.Equal(want) {
		t.Fatalf("the resolver budget carries deadline %v, want %v",
			resolverBudget.limits.Deadline, want)
	}
}

// TestStepLetSymbols_NilBudget pins the nil contract H1 made non-uniform.
//
// checkFormatString and checkLetBindings both document their new *templateBudget
// first parameter as nil-able and handle it. stepLetSymbols took the
// same-shaped parameter in the same change and read b.limits unconditionally,
// so stepLetSymbols(nil, ...) panicked. No production caller passes nil -- which
// is exactly why nothing caught it -- so this test is the only thing standing
// between the three siblings and three different unwritten nil contracts.
func TestStepLetSymbols_NilBudget(t *testing.T) {
	tmpl, err := Parse([]byte(exprDeadlineLetTemplate), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	step := tmpl.Steps[0]

	syms, errs := stepLetSymbols(nil, tmpl, &step, nil, "")
	if syms == nil {
		t.Error("stepLetSymbols(nil, ...) returned a nil symbol table")
	}
	// One error per binding: binding 0 exceeds the default operation limit, and
	// bindings 1 and 2 do not parse. A nil budget diverts nothing, so the block
	// runs to the end and reports all three exactly as it did before H1.
	if len(errs) != 3 {
		t.Errorf("stepLetSymbols(nil, ...) returned %d errors, want 3: %v", len(errs), errs)
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
