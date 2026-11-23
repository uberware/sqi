// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/worker/fmtres"
)

// This file tests the cumulative template-wide budget
// (checkTemplateExpressions' templateBudget, exprcheck.go): a bound on what
// one template's expressions may cost in total, across every position and
// every let: block. Per-evaluation bounds alone do not bound the walk, a
// let: block's retained memory, or the resolver.
//
// Two dimensions are asserted here, in separate tests so that removing
// either bound fails exactly one of them. The third, operations, is derived
// (defaultTemplatePositions * defaultSubmissionOperations; see exprcheck.go)
// and has nothing to assert here: no operation counter crosses into
// internal/openjd from internal/openjd/expr.
//
// To mutation-test these, set the knob: construct the walk's budget with
// newTemplateBudget(ExprLimits{TemplatePositions: n}) (or
// TemplateRetainedBytes) and observe the verdict move, as exprlimits_test.go
// does at the exact boundary. Neutering the comparison inside
// templateBudget.chargePositions/chargeRetainedBytes (exprcheck.go,
// "if b.positions > b.limits.TemplatePositions") checks that the error path
// itself is live.
//
// Do not raise defaultTemplatePositions/defaultTemplateRetainedBytes to
// remove the bound: three of the five tests below size their construction
// from the constant (manyArgs(int(defaultTemplatePositions) + 50), and
// int(defaultTemplatePositions) - 10 in FreshPerCall), so a huge value makes
// them try to allocate billions of entries and hang. Setting the knob on a
// budget has no such hazard.

// manyArgs returns n trivial, cheap-to-evaluate EXPR args entries: no
// retained bytes (checkFormatString discards every result) and negligible
// per-position operation/memory cost, so a construction built from these
// isolates the POSITIONS dimension from the retained-bytes dimension.
func manyArgs(n int) []string {
	args := make([]string, n)
	for i := range args {
		args[i] = "{{ 'a' }}"
	}
	return args
}

// TestCheckTemplateExpressions_TemplateWideBudget_CatchesWhatThePreWalkGuardAdmits reproduces
// a construction of 16 task-parameter definitions x 1024 RangeList entries in
// one step. With an expensive payload such as `{{ ("x" * 900000).upper() }}`
// in every entry, it stays inside every per-evaluation budget while costing
// the resolver a long run.
//
// The payload here is a trivial `{{ 'a' }}`: the test proves that the
// position count trips the budget, and the budget charges one position per
// checkFormatString call whatever the call evaluates. An expensive payload
// would make a broken position bound run for minutes, which reads as a hang
// on CI, instead of failing fast.
//
// It is within the two count caps parameterSpaceOverCaps checks
// (maxTaskParameterDefinitions = 16, maxTaskParamValues = 1024, both "at
// most"), so parameterSpaceOverCaps(tmpl), checked below, reports false: the
// pre-walk guard does not reject this construction, and neither does
// maxSteps (one step). The template-wide budget is the only thing that
// catches it.
func TestCheckTemplateExpressions_TemplateWideBudget_CatchesWhatThePreWalkGuardAdmits(t *testing.T) {
	const numDefs = maxTaskParameterDefinitions // 16
	const numValues = maxTaskParamValues        // 1024

	defs := make([]TaskParamDefinition, numDefs)
	for i := range defs {
		rangeList := make([]string, numValues)
		for j := range rangeList {
			rangeList[j] = "{{ 'a' }}" // trivial -- see the doc comment above for why
		}
		defs[i] = TaskParamDefinition{
			Name:      fmt.Sprintf("P%d", i),
			Type:      TaskParamTypeString,
			RangeList: rangeList,
		}
	}
	tmpl := &JobTemplate{
		Name:       "T",
		Extensions: []string{"EXPR"},
		Steps: []StepTemplate{{
			Name:           "Step1",
			ParameterSpace: &StepParameterSpace{TaskParameterDefinitions: defs},
		}},
	}

	if parameterSpaceOverCaps(tmpl) {
		t.Fatal("test setup is wrong: this construction must sit WITHIN maxTaskParameterDefinitions " +
			"and maxTaskParamValues (16 and 1024 are 'at most', not 'fewer than'), or it would already " +
			"be rejected by the pre-walk guard and prove nothing about the template-wide budget")
	}

	errs := checkTemplateExpressions(tmpl, nil)
	if len(errs) == 0 {
		t.Fatal("16 x 1024 range positions, within every existing structural cap, " +
			"must be rejected by the template-wide budget on position COUNT alone")
	}

	var found bool
	for _, e := range errs {
		if strings.Contains(e.Message, "template-wide expression budget exceeded") &&
			strings.Contains(e.Message, "expression positions") &&
			strings.Contains(e.Message, strconv.FormatInt(defaultTemplatePositions, 10)) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("want an error naming the positions dimension and its %d-position limit; got %v",
			defaultTemplatePositions, errs)
	}
}

// TestCheckTemplateExpressions_TemplateWideBudget_PositionsDimension isolates
// the POSITIONS dimension: many CHEAP positions (manyArgs -- no retained
// bytes, negligible per-position cost), well over defaultTemplatePositions.
// Mutation target: commenting out chargePositions' cap check (or raising
// defaultTemplatePositions past this construction's size) must make this
// test -- and ONLY this dimension's tests -- start passing/accepting.
func TestCheckTemplateExpressions_TemplateWideBudget_PositionsDimension(t *testing.T) {
	n := int(defaultTemplatePositions) + 50
	tmpl := &JobTemplate{
		Name:       "T",
		Extensions: []string{"EXPR"},
		Steps: []StepTemplate{{
			Name: "Step1",
			Script: &StepScript{Actions: StepActions{OnRun: Action{
				Command: "echo",
				Args:    manyArgs(n),
				ArgsSet: true,
			}}},
		}},
	}

	errs := checkTemplateExpressions(tmpl, nil)
	if len(errs) == 0 {
		t.Fatalf("%d cheap args entries, over the %d-position budget, must be rejected", n, defaultTemplatePositions)
	}

	var found bool
	for _, e := range errs {
		if strings.Contains(e.Message, "template-wide expression budget exceeded") &&
			strings.Contains(e.Message, "expression positions") {
			found = true
			if strings.Contains(e.Message, "retain") {
				t.Errorf("the positions-dimension error must not also read like a retained-bytes error: %q", e.Message)
			}
		}
	}
	if !found {
		t.Errorf("want an error naming the positions dimension; got %v", errs)
	}
}

// TestCheckTemplateExpressions_TemplateWideBudget_RetainedBytesDimension
// isolates the RETAINED-BYTES dimension: many let: blocks, each
// INDIVIDUALLY well within every per-block bound (maxLetBindings = 50
// bindings; each binding's own Eval under defaultSubmissionMemoryBytes =
// 1,000,000 bytes), that are cumulatively rejected only because the
// template-wide budget sums bytes ACROSS blocks.
//
// Without that sum, nothing bounds it: checkLetBindings caps a single
// block's BINDING COUNT but not the BYTES those bindings retain, so a
// template with many individually-compliant blocks could retain tens of
// megabytes with no guard seeing it -- the server-side counterpart of the
// worker's let: retained-bytes limit (internal/worker/fmtres).
//
// Each binding retains 900,064 bytes (64-byte header + a 900,000-byte
// string, expr.SizeOf). 3 bindings/step x 900,064 = 2,700,192 bytes/step --
// comfortably under the 10,000,000-byte budget on its own (the first
// sub-test), so a single block is never the thing that trips it. 4 steps'
// worth (10,800,768 bytes) crosses it (the second sub-test): the ONLY
// difference between the two is how many equally-compliant blocks exist.
func TestCheckTemplateExpressions_TemplateWideBudget_RetainedBytesDimension(t *testing.T) {
	const bindingsPerStep = 3
	const bytesPerBinding = 900_000 // + 64-byte header = 900,064 via expr.SizeOf

	buildTmpl := func(steps int) *JobTemplate {
		stepTmpls := make([]StepTemplate, steps)
		for i := range stepTmpls {
			lets := make([]string, bindingsPerStep)
			for j := range lets {
				lets[j] = fmt.Sprintf("a%d = \"x\" * %d", j, bytesPerBinding)
			}
			stepTmpls[i] = StepTemplate{
				Name:   fmt.Sprintf("Step%d", i),
				Let:    lets,
				LetSet: true,
			}
		}
		return &JobTemplate{Name: "T", Extensions: []string{"EXPR"}, Steps: stepTmpls}
	}

	t.Run("one compliant block alone is accepted", func(t *testing.T) {
		errs := checkTemplateExpressions(buildTmpl(1), nil)
		if len(errs) != 0 {
			t.Fatalf("one step's %d lets (~%d bytes) must fit comfortably under the %d-byte "+
				"template-wide budget on its own: %v",
				bindingsPerStep, bindingsPerStep*(bytesPerBinding+64), defaultTemplateRetainedBytes, errs)
		}
	})

	t.Run("many compliant blocks cumulatively exceed the budget", func(t *testing.T) {
		const steps = 4
		tmpl := buildTmpl(steps)
		errs := checkTemplateExpressions(tmpl, nil)
		if len(errs) == 0 {
			t.Fatalf("%d steps x %d lets of ~%d bytes each (~%d bytes total) must exceed the "+
				"%d-byte template-wide retained-bytes budget, even though every individual block "+
				"is within maxLetBindings and every individual binding is within defaultSubmissionMemoryBytes",
				steps, bindingsPerStep, bytesPerBinding+64, steps*bindingsPerStep*(bytesPerBinding+64),
				defaultTemplateRetainedBytes)
		}

		var found bool
		for _, e := range errs {
			if strings.Contains(e.Message, "template-wide expression budget exceeded") &&
				strings.Contains(e.Message, "let bindings may retain") &&
				strings.Contains(e.Message, strconv.FormatInt(defaultTemplateRetainedBytes, 10)) {
				found = true
				if strings.Contains(e.Message, "expression positions") {
					t.Errorf("the retained-bytes error must not also read like a positions error: %q", e.Message)
				}
			}
		}
		if !found {
			t.Errorf("want an error naming the retained-bytes dimension and its %d-byte limit; got %v",
				defaultTemplateRetainedBytes, errs)
		}
	})
}

// TestCheckTemplateExpressions_TemplateWideBudget_FreshPerCall pins one
// budget per phase: checkTemplateExpressions allocates a
// NEW templateBudget every call (it is a local variable, not package state),
// so back-to-back calls on the SAME template each get their own full
// allowance -- exactly what phase 1 (ValidateWithOptions) and phase 2
// (checkExpressionsAtSubmit) rely on, since they are two separate calls to
// this same function. A template sized to land just under the position cap
// must be accepted on every call, not merely the first.
func TestCheckTemplateExpressions_TemplateWideBudget_FreshPerCall(t *testing.T) {
	n := int(defaultTemplatePositions) - 10 // comfortably under the cap
	tmpl := &JobTemplate{
		Name:       "T",
		Extensions: []string{"EXPR"},
		Steps: []StepTemplate{{
			Name: "Step1",
			Script: &StepScript{Actions: StepActions{OnRun: Action{
				Command: "echo",
				Args:    manyArgs(n),
				ArgsSet: true,
			}}},
		}},
	}

	for i := range 2 {
		if errs := checkTemplateExpressions(tmpl, nil); len(errs) != 0 {
			t.Fatalf("call %d: a template under the budget must be accepted on every independent "+
				"call, not just the first (a shared/leaked budget would make later calls stricter "+
				"than earlier ones): %v", i, errs)
		}
	}
}

// TestCheckTemplateExpressions_TemplateWideBudget_BaseSpecUnaffected pins
// the base-spec cost floor: a template without extensions: [EXPR] never
// enters the walk, so the budget costs it nothing. A
// construction that WOULD trip the positions budget if the walk ever ran
// over it must still report zero errors when the template does not declare
// the extension -- proving the hasExtension("EXPR") gate at the top of
// checkTemplateExpressions, not the budget, is what decides this.
func TestCheckTemplateExpressions_TemplateWideBudget_BaseSpecUnaffected(t *testing.T) {
	n := int(defaultTemplatePositions) + 50
	tmpl := &JobTemplate{
		Name: "T", // no Extensions: EXPR is not declared
		Steps: []StepTemplate{{
			Name: "Step1",
			Script: &StepScript{Actions: StepActions{OnRun: Action{
				Command: "echo",
				Args:    manyArgs(n),
				ArgsSet: true,
			}}},
		}},
	}
	if errs := checkTemplateExpressions(tmpl, nil); len(errs) != 0 {
		t.Fatalf("a non-EXPR template must never enter the walk at all, regardless of size: %v", errs)
	}
}

// TestTemplateBudget_WorkerCapIsNotTighter pins the relation between this
// package's template-wide position budget and the worker's per-assignment one
// (internal/worker/fmtres, [fmtres.DefaultAssignmentPositions]).
//
// If the worker's cap were tighter (say 10,000 here and 5,000 there), a
// template with one job environment declaring 5,000 variables would charge
// ~5,001 positions here, be accepted and persisted, and then trip the
// worker's 5,000-position budget inside session.Manager.Create, failing every
// task in the job after submission, naming a budget the submitter never saw.
//
// The positions one assignment resolves on the worker (its step action and
// embedded files, plus every job and step environment the session enters)
// are a subset of the positions this package's walk charged for the whole
// template, so the worker's cap must be at least this one. The worker's cap
// is raised to match rather than this one lowered: this one is sized not to
// reject legitimate templates, and charging a per-assignment sub-budget at
// submit would need a partition of the walk that nothing in this package
// computes.
//
// It imports internal/worker/fmtres, which no production file in this package
// does or should: the two constants live in different packages and
// processes, and a test-only import is a place that sees both and runs under
// `make test`.
//
// This compares the two defaults, not the enforced values. The server's cap
// is openjd.ExprLimits.TemplatePositions (openjd.expr_template_positions) and
// the worker's is fmtres.ExprLimits.AssignmentPositions (the worker config's
// expr.assignment_positions); the constants below are what each falls back
// to. A farm configured with one above the other is handled at run time: a
// worker advertises the caps it enforces in its registration message, the
// server persists them (store.WorkerExprLimits) and does not dispatch an EXPR
// job to a worker tighter than the limits the template was accepted under
// (internal/scheduler/exprcaps.go,
// TestExprCaps_ViolationThroughConfigurationIsCaught). This test covers the
// defaults, so a fresh install cannot violate the relation out of the box.
//
// The relation is also satisfiable at every legal setting:
// fmtres.MaxExprAssignmentPositions is >= MaxExprTemplatePositions, so no
// server value is beyond what a worker can legally match. internal/scheduler's
// TestExprCaps_RelationIsSatisfiableAtEveryLegalServerSetting extends that
// check to all four related dimensions.
func TestTemplateBudget_WorkerCapIsNotTighter(t *testing.T) {
	if fmtres.DefaultAssignmentPositions < defaultTemplatePositions {
		t.Fatalf("the worker's per-assignment position cap (%d) is tighter than the server's "+
			"template-wide cap (%d).\n"+
			"An assignment's positions are a subset of its template's, so this makes a job the "+
			"server accepts fail on the worker -- per task, after submission, naming a budget the "+
			"submitter was never shown. Raise fmtres.DefaultAssignmentPositions, or give the server "+
			"a per-assignment sub-budget so the rejection happens at submit.",
			fmtres.DefaultAssignmentPositions, defaultTemplatePositions)
	}
	// The same relation, one level up: an operator may raise this package's
	// cap as far as MaxExprTemplatePositions, so the worker's configurable
	// ceiling must reach at least that far, or some legal server setting
	// would have no matching legal worker setting.
	//
	// The assertion is worker ceiling >= server ceiling, not equality: only
	// lowering the worker's ceiling or raising this one past it fails here.
	if fmtres.MaxExprAssignmentPositions < MaxExprTemplatePositions {
		t.Fatalf("the worker's configurable position ceiling (%d) is below the server's (%d): "+
			"an operator could set openjd.expr_template_positions to a value no worker's "+
			"expr.assignment_positions is allowed to match, making the subset relation above "+
			"unsatisfiable rather than merely breakable.",
			fmtres.MaxExprAssignmentPositions, MaxExprTemplatePositions)
	}
}

// TestExprWalkApplies_BaseSpecTemplateShortCircuitsTheCostGuard pins that a
// template that declares no extensions: [EXPR] does not pay for
// parameterSpaceOverCaps, the O(n) pre-walk cost guard.
//
// The guard decides whether to skip the expression walk. For a base-spec
// template the walk is a no-op, so running the guard would be wasted work on
// a large INT parameter, and EXPR support must cost such a template nothing.
// The call site in ValidateWithOptions is
// "exprDeclared && !parameterSpaceOverCaps(t)", and Go's && short-circuits, so
// asserting the declaration term is false for a base-spec template asserts
// that the guard never runs. That term is a direct hasExtension call, so this
// test asserts on that call.
func TestExprWalkApplies_BaseSpecTemplateShortCircuitsTheCostGuard(t *testing.T) {
	base := &JobTemplate{Name: "T"} // no Extensions
	if base.hasExtension("EXPR") {
		t.Error("a template with no extensions: [EXPR] must short-circuit before " +
			"parameterSpaceOverCaps -- the base-spec cost floor")
	}

	withExpr := &JobTemplate{Name: "T", Extensions: []string{"EXPR"}}
	if !withExpr.hasExtension("EXPR") {
		t.Error("an EXPR-declaring template must reach the walk; the extension term " +
			"must narrow the gate, not close it")
	}
}
