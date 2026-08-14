// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/worker/fmtres"
)

// This file proves EXPR sub-project E4c's template-wide budget
// (checkTemplateExpressions' templateBudget, exprcheck.go): a bound on what
// ONE template's expressions may cost IN TOTAL, across every position and
// every let: block, closing the gap three earlier sub-projects each found
// and each fixed only locally (design spec §1.1's table: E2's ~9-minute
// walk, E3's 6.9 GB let: block, E4a's 472 MB worker table, E4b's 96-second
// resolver run).
//
// Two dimensions are asserted here, DELIBERATELY through separate tests so
// that removing either bound in isolation fails exactly one of them --
// "two dimensions that share one test are one dimension" (this task's
// brief). The third dimension, operations, is derived rather than measured
// (defaultTemplatePositions * defaultSubmissionOperations, exprcheck.go's own
// doc comment) and has nothing to assert here: no operation counter crosses
// into internal/openjd/expr for this file to observe.
//
// HOW TO MUTATION-TEST THESE, updated for E4d: since the bounds became
// configurable, the natural mutation is to SET THE KNOB -- construct the
// walk's budget with newTemplateBudget(ExprLimits{TemplatePositions: n}) (or
// TemplateRetainedBytes) and observe the verdict move. That is what
// exprlimits_test.go does, at the exact boundary. Neutering the comparison
// inside templateBudget.chargePositions/chargeRetainedBytes (exprcheck.go,
// now "if b.positions > b.limits.TemplatePositions") still works and is what
// to reach for when checking that the ERROR PATH itself is live.
//
// WHAT NOT TO DO, recorded because a reviewer hit it and had to kill a 600s
// hang: do not raise defaultTemplatePositions/defaultTemplateRetainedBytes
// themselves. Three of the five tests below SIZE THEIR OWN CONSTRUCTION from
// the live constant (manyArgs(int(defaultTemplatePositions) + 50), and
// int(defaultTemplatePositions) - 10 in FreshPerCall): raising
// defaultTemplatePositions to, say, 2_000_000_000 to "remove the bound"
// makes those tests try to allocate on the order of two billion string
// entries, which does not fail fast -- it hangs. Setting the KNOB on a budget
// has no such hazard: the construction's size stays tied to the default.

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

// TestCheckTemplateExpressions_TemplateWideBudget_E4bConstruction reproduces
// the SHAPE of EXPR sub-project E4b's own measured construction -- 16
// task-parameter definitions x 1024 RangeList entries, one step -- turned
// into a regression test per design spec §6 ("The instrument is the
// construction"). E4b's original payload, `{{ ("x" * 900000).upper() }}` in
// every one of the 16,384 entries, cost 96 seconds in the resolver alone,
// with every per-Eval budget respected the entire time (design spec §1.1).
//
// The payload here is deliberately a trivial `{{ 'a' }}`, NOT E4b's original
// expensive expression. Fix round 1 (post-implementation review) found the
// expensive version made this ONE test cost 455s under -race in isolation --
// the package's dominant race cost by two orders of magnitude over the other
// four budget tests combined (1.27s) -- for fidelity the assertions below
// never used: this test proves POSITION COUNT trips the budget, which a
// trivial payload demonstrates identically to an expensive one (the budget
// charges one position per checkFormatString call regardless of what that
// call evaluates), and rejects in ~milliseconds instead of tens of seconds.
// It also means a REGRESSION in the position bound now fails FAST rather
// than running to completion in ~95s (non-race) or roughly an hour under
// race, which on a CI runner reads as a hang, not a red test. The original
// 900,000-byte-per-entry construction remains what E4b actually measured;
// it is not re-measured here.
//
// It is deliberately BELOW the two count caps Task 1/2 of this sub-project
// rely on (maxTaskParameterDefinitions = 16, maxTaskParamValues = 1024 --
// both "at most", not "fewer than"), so parameterSpaceOverCaps(tmpl) --
// checked directly below, per this task's brief ("check, and if so build one
// that is within every structural cap") -- reports false: Task 1's pre-walk
// guard does NOT reject this construction, and neither does maxSteps (one
// step). The template-wide budget is therefore the ONLY thing in the
// package that can still catch it.
func TestCheckTemplateExpressions_TemplateWideBudget_E4bConstruction(t *testing.T) {
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
			"be rejected by Task 1's pre-walk guard and prove nothing about the budget added here")
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
// isolates the RETAINED-BYTES dimension, and doubles as this task's Step 3
// proof (design spec §4, "the asymmetry this wave should also close"): many
// let: blocks, each INDIVIDUALLY well within every existing per-block bound
// (maxLetBindings = 50 bindings; each binding's own Eval under
// defaultSubmissionMemoryBytes = 1,000,000 bytes), that are cumulatively rejected
// only because the template-wide budget sums bytes ACROSS blocks.
//
// Before this task, nothing bounded that sum: checkLetBindings caps a single
// block's BINDING COUNT (E3's fix) but not the BYTES those bindings retain,
// so a template with many individually-compliant blocks could retain
// tens of megabytes with no guard seeing it -- exactly the gap
// workerLetRetainedLimit closed on the worker side (E4a) with no server-side
// counterpart until now.
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

// TestCheckTemplateExpressions_TemplateWideBudget_FreshPerCall pins design
// spec §3.1's "one budget per phase": checkTemplateExpressions allocates a
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
// design spec §6's floor: "a template without extensions: [EXPR] never
// enters the walk, so it should cost exactly what it costs today". A
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

// TestTemplateBudget_WorkerCapIsNotTighter pins the ONE relation between this
// package's template-wide position budget and the worker's per-assignment one
// (internal/worker/fmtres, [fmtres.DefaultAssignmentPositions]).
//
// THE DEFECT IT EXISTS TO PREVENT (EXPR sub-project E4c, whole-branch review,
// IMPORTANT 1): the two constants were 10,000 here and 5,000 there, with
// nothing relating them, no test asserting a relation, and neither comment
// mentioning the other. A template with one job environment declaring 5,000
// variables charged ~5,001 positions HERE -- comfortably accepted, created
// and persisted -- and then tripped the worker's 5,000-position budget inside
// session.Manager.Create, failing EVERY task in the job, one at a time, after
// submission, naming a budget the submitter never saw.
//
// THE RELATION: the positions one assignment resolves on the worker (its own
// step action and embedded files, plus every job and step environment the
// session enters) are a SUBSET of the positions this package's walk charged
// for the whole template. So the worker's cap must be at least this one, or
// there exists a template the server accepts and the worker cannot run.
// Fixed by RAISING the worker's cap to match, not by lowering this one --
// fix round 1 raised this one deliberately to stop rejecting legitimate
// templates, and the alternative (having the server charge a per-assignment
// SUB-budget so the rejection lands at submit) needs a partition of the walk
// that nothing in this package computes.
//
// It imports internal/worker/fmtres, which no production file in this package
// does and none should. That is the point: the two constants live in
// different packages in different processes, and an invariant between them
// has to be asserted somewhere that can see both. A test-only import is the
// cheapest place that also runs under plain `make test`.
// SINCE E4d TASKS 1 AND 2 THIS COMPARES TWO DEFAULTS, NOT THE ENFORCED
// VALUES. The server's cap is openjd.ExprLimits.TemplatePositions
// (openjd.expr_template_positions) and the worker's is
// fmtres.ExprLimits.AssignmentPositions (the worker config's
// expr.assignment_positions); the two constants below are only what each falls
// back to. A farm whose YAML raises one above the other reproduces exactly the
// failure described above, and NO COMPILE-TIME TEST CAN SEE IT.
//
// E4d TASK 3 CLOSES THAT AT RUNTIME, not here. A worker advertises the caps it
// will enforce in its registration message; the server persists them
// (store.WorkerExprLimits) and refuses to dispatch an EXPR job to a worker that
// is tighter than the limits the template was accepted under
// (internal/scheduler/exprcaps.go, TestExprCaps_ViolationThroughConfiguration-
// IsCaught). This test remains the DEFAULTS half of the same invariant: it is
// what fails if a future edit ships a fresh install that violates the relation
// out of the box, which the runtime gate would then dutifully enforce by
// refusing every worker in the farm. Do not delete or weaken it.
//
// What Task 2 did do is make the relation SATISFIABLE at every legal setting:
// fmtres.MaxExprAssignmentPositions is >= MaxExprTemplatePositions (today both
// are 100,000), so there is no server value an operator can choose that a
// worker cannot legally match. internal/scheduler's
// TestExprCaps_RelationIsSatisfiableAtEveryLegalServerSetting extends that
// check to all four related dimensions.
func TestTemplateBudget_WorkerCapIsNotTighter(t *testing.T) {
	if fmtres.DefaultAssignmentPositions < defaultTemplatePositions {
		t.Fatalf("the worker's per-assignment position cap (%d) is TIGHTER than the server's "+
			"template-wide cap (%d).\n"+
			"An assignment's positions are a subset of its template's, so this makes a job the "+
			"server ACCEPTS fail on the worker -- per task, after submission, naming a budget the "+
			"submitter was never shown. Raise fmtres.DefaultAssignmentPositions, or give the server "+
			"a per-assignment sub-budget so the rejection happens at submit.",
			fmtres.DefaultAssignmentPositions, defaultTemplatePositions)
	}
	// The same relation, one level up: an operator may raise this package's
	// cap as far as MaxExprTemplatePositions, so the worker's configurable
	// ceiling must reach at least that far or the relation above becomes
	// unsatisfiable BY CONFIGURATION -- there would exist a legal server
	// setting no legal worker setting could match.
	//
	// What is asserted is that relation (worker ceiling >= server ceiling),
	// NOT equality: raising the worker's ceiling alone, or lowering this
	// package's alone, keeps satisfiability and keeps this green. Only the two
	// moves that break it -- lowering the worker's or raising this one past it
	// -- fail here. E4d Task 2 happens to set them equal (both 100,000); that
	// is the current value, not the invariant.
	if fmtres.MaxExprAssignmentPositions < MaxExprTemplatePositions {
		t.Fatalf("the worker's configurable position CEILING (%d) is below the server's (%d): "+
			"an operator could set openjd.expr_template_positions to a value no worker's "+
			"expr.assignment_positions is allowed to match, making the subset relation above "+
			"unsatisfiable rather than merely breakable.",
			fmtres.MaxExprAssignmentPositions, MaxExprTemplatePositions)
	}
}

// TestExprWalkApplies_BaseSpecTemplateShortCircuitsTheCostGuard pins fix round
// 2's MINOR 1: a template that declares no extensions: [EXPR] must not pay for
// parameterSpaceOverCaps, the O(n) pre-walk cost guard.
//
// The guard exists to decide whether to SKIP the expression walk. For a
// base-spec template the walk is a no-op, so running the guard is pure waste
// -- 15 ms on a 200,000-sub-range INT parameter, ~40 ms at the body cap --
// and it breaks design spec §6's floor that such a template "should cost
// exactly what it costs today". The call site in ValidateWithOptions is
// "exprDeclared && !parameterSpaceOverCaps(t)", and Go's && short-circuits, so
// asserting the declaration term is false for a base-spec template IS the
// assertion that the guard never runs.
//
// The declaration is now the WHOLE of that term, and it is a direct
// hasExtension call rather than a helper. It used to be exprWalkApplies, a
// two-argument function whose second argument was the since-deleted
// ValidateOptions.CheckEXPRExpressionsWhileUnsupported, and this test passed
// that as TRUE deliberately: the short-circuit had to hold even for the caller
// that had explicitly asked for expressions to be checked while EXPR was
// unsupported, and -- more importantly -- it had to keep holding after
// sub-project H2 flipped the extension to StatusSupported and the status term
// became permanently true. It did, the helper's remaining half was the same
// hasExtension call ValidateWithBudget already had in hand as exprDeclared, and
// this test now asserts on that call directly.
func TestExprWalkApplies_BaseSpecTemplateShortCircuitsTheCostGuard(t *testing.T) {
	base := &JobTemplate{Name: "T"} // no Extensions
	if base.hasExtension("EXPR") {
		t.Error("a template with no extensions: [EXPR] must short-circuit before " +
			"parameterSpaceOverCaps -- see design spec §6's base-spec cost floor")
	}

	withExpr := &JobTemplate{Name: "T", Extensions: []string{"EXPR"}}
	if !withExpr.hasExtension("EXPR") {
		t.Error("an EXPR-declaring template must reach the walk; the extension term " +
			"must narrow the gate, not close it")
	}
}
