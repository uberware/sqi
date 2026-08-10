// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
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
// (maxTemplateExprPositions * submissionOperationLimit, exprcheck.go's own
// doc comment) and has nothing to assert here: no operation counter crosses
// into internal/openjd/expr for this file to observe.

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

// TestCheckTemplateExpressions_TemplateWideBudget_E4bConstruction is EXPR
// sub-project E4b's own measured construction -- 16 task-parameter
// definitions x 1024 RangeList entries, each entry `("x" * 900000).upper()`
// -- turned into a regression test per design spec §6 ("The instrument is
// the construction"). Unguarded, this cost 96 seconds in the resolver alone,
// with every per-Eval budget respected the entire time (design spec §1.1).
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
			rangeList[j] = `{{ ("x" * 900000).upper() }}`
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
		t.Fatal("16 x 1024 expensive range positions, within every existing structural cap, " +
			"must be rejected by the template-wide budget")
	}

	var found bool
	for _, e := range errs {
		if strings.Contains(e.Message, "template-wide expression budget exceeded") &&
			strings.Contains(e.Message, "expression positions") &&
			strings.Contains(e.Message, strconv.FormatInt(maxTemplateExprPositions, 10)) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("want an error naming the positions dimension and its %d-position limit; got %v",
			maxTemplateExprPositions, errs)
	}
}

// TestCheckTemplateExpressions_TemplateWideBudget_PositionsDimension isolates
// the POSITIONS dimension: many CHEAP positions (manyArgs -- no retained
// bytes, negligible per-position cost), well over maxTemplateExprPositions.
// Mutation target: commenting out chargePositions' cap check (or raising
// maxTemplateExprPositions past this construction's size) must make this
// test -- and ONLY this dimension's tests -- start passing/accepting.
func TestCheckTemplateExpressions_TemplateWideBudget_PositionsDimension(t *testing.T) {
	n := int(maxTemplateExprPositions) + 50
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
		t.Fatalf("%d cheap args entries, over the %d-position budget, must be rejected", n, maxTemplateExprPositions)
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
// submissionMemoryLimit = 1,000,000 bytes), that are cumulatively rejected
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
				bindingsPerStep, bindingsPerStep*(bytesPerBinding+64), maxTemplateExprRetainedBytes, errs)
		}
	})

	t.Run("many compliant blocks cumulatively exceed the budget", func(t *testing.T) {
		const steps = 4
		tmpl := buildTmpl(steps)
		errs := checkTemplateExpressions(tmpl, nil)
		if len(errs) == 0 {
			t.Fatalf("%d steps x %d lets of ~%d bytes each (~%d bytes total) must exceed the "+
				"%d-byte template-wide retained-bytes budget, even though every individual block "+
				"is within maxLetBindings and every individual binding is within submissionMemoryLimit",
				steps, bindingsPerStep, bytesPerBinding+64, steps*bindingsPerStep*(bytesPerBinding+64),
				maxTemplateExprRetainedBytes)
		}

		var found bool
		for _, e := range errs {
			if strings.Contains(e.Message, "template-wide expression budget exceeded") &&
				strings.Contains(e.Message, "let bindings may retain") &&
				strings.Contains(e.Message, strconv.FormatInt(maxTemplateExprRetainedBytes, 10)) {
				found = true
				if strings.Contains(e.Message, "expression positions") {
					t.Errorf("the retained-bytes error must not also read like a positions error: %q", e.Message)
				}
			}
		}
		if !found {
			t.Errorf("want an error naming the retained-bytes dimension and its %d-byte limit; got %v",
				maxTemplateExprRetainedBytes, errs)
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
	n := int(maxTemplateExprPositions) - 10 // comfortably under the cap
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
	n := int(maxTemplateExprPositions) + 50
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
