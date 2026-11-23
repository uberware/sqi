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

// exprDeadlineLetTemplate puts the expression work inside a let: block, the
// one position whose failure path appends an error and continues instead of
// returning (see checkLetBindings). A deadline recorded there must stop the
// block instead of running the remaining bindings.
//
// The later bindings must stay syntactically broken: that is what lets the
// test tell a block that stopped from one that kept going. A binding that
// fails to parse appends a ValidationError before any evaluation, so
// recordDeadline cannot swallow it: if checkLetBindings continued past the
// deadline, bindings 1 and 2 would each report at /steps/0/let/N. With valid
// later bindings, each would trip the deadline again, be swallowed and report
// nothing, so the assertion would hold either way.
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

// exprDeadlineSegmentTemplate puts the same work in a reference embedded in
// surrounding text, the checker's other format-string shape and its own
// conversion site: fmtstring.LoneRef is false, so checkFormatString walks
// segments and evaluates each reference in a loop.
//
// The second reference is a syntax error for the same reason the let
// fixture's later bindings are: a parse failure is appended before any
// evaluation, so a swallowed deadline cannot hide it. With a single reference,
// checkFormatString returning and continuing would be indistinguishable.
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

// exprDeadlineResolveTemplate is the phase-2 resolver's fixture: a step with
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
// resolveTaskParamDefinition, and it has no let: block.
//
// exprDeadlineResolveTemplate gives its step a let: block, so the walk trips
// inside stepLetSymbols and ResolveParameterSpaceParams' definition loop
// breaks before resolveTaskParamDefinition is entered; it does not exercise
// the resolver's own two conversion sites. This fixture has no let: and a
// whole-field expression range, so the first evaluation of the call happens
// inside resolveRangeExprField.
//
// The second definition's range is a syntax error so that a walk that stopped
// can be told from one that kept going: reached, it appends a parse
// ValidationError at /parameterSpace/taskParameterDefinitions/1/range; not
// reached, it appends nothing.
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

// exprDeadlineResolveRangeListTemplate reaches the resolver's other conversion
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

// TestValidateWithBudget_DeadlineIsNotAValidationError pins the contract of
// the wall-clock submission deadline: a wall-clock stop means the server gave
// up, and is not a verdict that the template is invalid.
//
// A budget error is a ValidationError and becomes a 422. A deadline must not
// be one, because the same body would validate on an idle machine; reporting
// it as invalid would make acceptance depend on machine load.
func TestValidateWithBudget_DeadlineIsNotAValidationError(t *testing.T) {
	tmpl, err := Parse([]byte(exprDeadlineTemplate), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	errs, ferr := ValidateWithBudget(tmpl, ValidateOptions{
		EnforceLimits: true,
		Deadline:      time.Now().Add(-time.Second), // expired
	})

	if ferr == nil {
		t.Fatal("ValidateWithBudget returned no error with an expired deadline; " +
			"the deadline is probably not reaching the evaluator")
	}
	if !errors.Is(ferr, expr.ErrDeadlineExceeded) {
		t.Fatalf("error = %v, want it to wrap expr.ErrDeadlineExceeded", ferr)
	}
	// It must not also be reported as a validation error: a caller turning
	// errors into status codes depends on that.
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
// same diversion [checkLetBindings] does.
//
// It also asserts that the segment loop stopped, and did not only stop
// reporting; see exprDeadlineSegmentTemplate for how its second, unparseable
// reference makes the difference observable.
func TestValidateWithBudget_DeadlineInEmbeddedSegmentIsNotAValidationError(t *testing.T) {
	tmpl, err := Parse([]byte(exprDeadlineSegmentTemplate), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	errs, ferr := ValidateWithBudget(tmpl, ValidateOptions{
		EnforceLimits: true,
		Deadline:      time.Now().Add(-time.Second), // expired
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
// error and moves to the next one, so a single malformed line does not hide
// the rest of the block. A deadline must instead stop the block: running the
// remaining bindings spends time the deadline says has run out. The block
// yields the deadline through the budget and produces no per-binding
// validation errors.
//
// The "no /steps/0/let error" assertion depends on how
// exprDeadlineLetTemplate is built: bindings 1 and 2 fail to parse, which is
// reported before any evaluation and so cannot be swallowed by
// recordDeadline. With valid later bindings the assertion would hold whether
// checkLetBindings returns or continues; see that fixture's comment.
func TestValidateWithBudget_DeadlineInLetBindingsStopsTheBlock(t *testing.T) {
	tmpl, err := Parse([]byte(exprDeadlineLetTemplate), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	errs, ferr := ValidateWithBudget(tmpl, ValidateOptions{
		EnforceLimits: true,
		Deadline:      time.Now().Add(-time.Second), // expired
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

// TestValidateWithOptions_UnchangedByBudget pins that the deadline-free entry
// point is unaffected by the deadline plumbing, since every caller but the
// submission path uses it.
//
// Only the expression walk can reject this fixture: it is 100,000
// comprehension iterations against a 10,000-operation budget, so with no
// deadline set the verdict is the deterministic operation limit and never the
// wall-clock deadline.
func TestValidateWithOptions_UnchangedByBudget(t *testing.T) {
	tmpl, err := Parse([]byte(exprDeadlineTemplate), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// No deadline set, so only the deterministic limits apply.
	errs := ValidateWithOptions(tmpl, ValidateOptions{EnforceLimits: true})
	if len(errs) == 0 {
		t.Fatal("the walk did not run: an over-budget EXPR template was accepted")
	}
	for _, e := range errs {
		if strings.Contains(strings.ToLower(e.Message), "deadline") {
			t.Errorf("deadline reported at %s with no deadline set: %s", e.Pointer, e.Message)
		}
	}
	if !containsMessageSubstring(errs, "operation limit exceeded") {
		t.Errorf("errs = %v, want the deterministic operation-limit verdict: the walk "+
			"is the only thing that can reject this fixture", errs)
	}
}

// containsMessageSubstring reports whether any of errs carries want in its
// message.
func containsMessageSubstring(errs ValidationErrors, want string) bool {
	for _, e := range errs {
		if strings.Contains(e.Message, want) {
			return true
		}
	}
	return false
}

// TestCheckExpressionsAtSubmit_DeadlineIsNotASubmitValidationError covers
// phase 2 of the same contract.
//
// checkExpressionsAtSubmit turns the walk's ValidationErrors into a
// *SubmitValidationError, the client-fault channel (a 4xx). A deadline adds no
// ValidationError, so without an explicit check this function would see an
// empty error slice and return nil, reporting a walk that stopped early as a
// clean re-check and letting submission proceed on unchecked expressions.
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
	if _, ok := errors.AsType[*SubmitValidationError](serr); ok {
		t.Errorf("error = %v, want NOT a *SubmitValidationError: a wall-clock stop "+
			"is not the submitter's fault", serr)
	}
}

// TestResolveParameterSpaceParams_DeadlineYieldsNoParameterSpace pins the third
// observation point.
//
// The resolver returns (*StepParameterSpace, ValidationErrors) and has no
// error channel. A deadline produces neither a ValidationError nor a complete
// space: the definition loop breaks on !b.ok(), leaving the rest of newDefs
// zero-valued. Returning that would look like success, and the caller would
// expand an unfinished parameter space. So the space must be nil, and the
// breach must be readable from the shared budget.
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
// Two things must hold: the deadline does not become a ValidationError (it is
// a 503, never a 422), and the walk stops. The second definition's syntax
// error makes stopping observable: a walk that continued would report a parse
// error at definition 1.
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
// valid EXPR template whose one expression is a call.
//
// It must be a call: a bare literal such as "{{ [1, 2, 3] }}" performs no
// meter.charge, so the clock is never sampled and the position resolves
// however long ago the deadline passed.
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

// TestSubmit_DeadlineIsNotASubmitValidationError drives the deadline contract
// through the public [Submitter.Submit] API, the only place the whole chain is
// observable: the configured instant reaching both phase-2 budgets, phase 1
// calling [ValidateWithBudget] rather than [ValidateWithOptions], and the
// breach coming back as a plain error rather than as the client-fault type.
//
// The message assertion is required. Phase 1 and phase 2 walk the same
// positions, so a deadline trips in whichever runs first. If phase 1 called
// ValidateWithOptions, which discards the channel the breach arrives on, the
// walk would stop without an error, phase 1 would report success, and phase 2
// would produce a deadline error of its own; the ErrDeadlineExceeded check
// alone would still pass. The phase-1 wording tells the two apart.
func TestSubmit_DeadlineIsNotASubmitValidationError(t *testing.T) {
	ctx := t.Context()
	st := fake.New()
	farm, err := st.CreateFarm(ctx, store.Farm{ID: uuid.NewString(), Name: "deadline-farm"})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err := st.CreateQueue(ctx, store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: "deadline-queue"})
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
	if _, ok := errors.AsType[*SubmitValidationError](err); ok {
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
// with SubmitOptions.Deadline left zero, as every caller that is not a
// submission handler leaves it, the pipeline has no wall-clock bound and
// submits normally.
func TestSubmit_NoDeadlineIsUnchanged(t *testing.T) {
	ctx := t.Context()
	st := fake.New()
	farm, err := st.CreateFarm(ctx, store.Farm{ID: uuid.NewString(), Name: "deadline-farm"})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err := st.CreateQueue(ctx, store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: "deadline-queue"})
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
// ResolveParameterSpaceParams call in Submit's step loop, where a deadline
// stops resolution of an unfinished parameter space (see
// TestResolveParameterSpaceParams_DeadlineYieldsNoParameterSpace).
// prepareTemplate returns it, so its deadline can be asserted directly with an
// instant an hour away.
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
	_, _, resolverBudget, _, err := sub.prepareTemplate(
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

// TestStepLetSymbols_NilBudget pins that stepLetSymbols accepts a nil budget.
//
// checkFormatString and checkLetBindings both document their *templateBudget
// first parameter as nil-able and handle it. stepLetSymbols takes the same
// parameter, and reading b.limits unconditionally would make
// stepLetSymbols(nil, ...) panic. No production caller passes nil, so this
// test keeps the three functions on one nil contract.
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
	// runs to the end and reports all three.
	if len(errs) != 3 {
		t.Errorf("stepLetSymbols(nil, ...) returned %d errors, want 3: %v", len(errs), errs)
	}
}

// TestExprLimits_DeadlineSurvivesNormalization pins that the deadline carried
// on ExprLimits survives normalization: ExprLimits is normalized
// ([ExprLimits.orDefaults]) before any evaluation reads it, and a
// normalization that rebuilt the struct field by field instead of patching a
// copy would drop the deadline while the tests above still passed on a
// template that finishes quickly.
func TestExprLimits_DeadlineSurvivesNormalization(t *testing.T) {
	want := time.Unix(1_700_000_000, 0)
	got := ExprLimits{Deadline: want}.orDefaults()
	if !got.Deadline.Equal(want) {
		t.Fatalf("Deadline after orDefaults = %v, want %v", got.Deadline, want)
	}

	// It must also survive the budget that carries it to every metered
	// position, since that is where orDefaults is applied.
	if b := newTemplateBudget(ExprLimits{Deadline: want}); !b.limits.Deadline.Equal(want) {
		t.Fatalf("Deadline on a fresh budget = %v, want %v", b.limits.Deadline, want)
	}
	// It must also be handed to the evaluator as an option. Options are
	// opaque closures, so only the count is observable here;
	// TestValidateWithBudget_DeadlineIsNotAValidationError covers the deadline
	// reaching the meter.
	set := ExprLimits{Deadline: want}.orDefaults().evalOptions()
	unset := ExprLimits{}.orDefaults().evalOptions()
	if len(set) != len(unset)+1 {
		t.Fatalf("evalOptions returned %d options with a deadline and %d without; "+
			"want exactly one more", len(set), len(unset))
	}
}

// exprDeadlineResolveChunkTemplate reaches the resolver's chunk-sizing site,
// resolveChunkInt: definition 0's range is literal text, so nothing before its
// defaultTaskCount evaluates, and definition 1's syntax error makes a walk
// that kept going past the deadline observable. The count is "1 + 1" because
// the deadline is checked when an operation is charged, and a bare literal
// charges none.
const exprDeadlineResolveChunkTemplate = `
specificationVersion: jobtemplate-2023-09
extensions: [EXPR, TASK_CHUNKING]
name: T
steps:
- name: S
  parameterSpace:
    taskParameterDefinitions:
    - name: F
      type: CHUNK[INT]
      range: "1-12"
      chunks: { defaultTaskCount: "{{ 1 + 1 }}", rangeConstraint: CONTIGUOUS }
    - name: G
      type: INT
      range: "{{ 1 + }}"
  script:
    actions:
      onRun:
        command: echo
        args: ["hi"]
`

// TestResolveParameterSpaceParams_DeadlineAtChunkPosition covers resolveChunkInt
// with the two assertions its range siblings make: the deadline is diverted onto
// the budget rather than reported as a ValidationError, and the walk stops.
func TestResolveParameterSpaceParams_DeadlineAtChunkPosition(t *testing.T) {
	tmpl, err := Parse([]byte(exprDeadlineResolveChunkTemplate), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	step := tmpl.Steps[0]

	budget := newTemplateBudget(ExprLimits{Deadline: time.Now().Add(-time.Second)})
	ps, errs := ResolveParameterSpaceParams(tmpl, &step, step.ParameterSpace, nil, budget)

	if !errors.Is(budget.deadline(), expr.ErrDeadlineExceeded) {
		t.Fatalf("budget deadline = %v, want expr.ErrDeadlineExceeded: the resolver's "+
			"chunk-sizing site is not diverting it", budget.deadline())
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
