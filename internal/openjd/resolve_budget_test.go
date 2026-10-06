// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

// The resolver's share of the template-wide budget (exprcheck.go's
// templateBudget). This file proves:
//
//   - ResolveParameterSpaceParams (resolve.go) is bounded on its own, in both
//     dimensions, exactly like checkTemplateExpressions is -- a template the
//     checker's own walk catches is also caught here.
//   - submit.go's Submit gives the checker's phase-2 re-check
//     (checkExpressionsAtSubmit) and every step's ResolveParameterSpaceParams
//     call TWO SEPARATE budgets for one submission, not one shared budget.
//
// The two walks charge the IDENTICAL range positions and let bytes for the
// SAME submission (these positions are evaluated twice), so pooling them in
// one budget would HALVE the effective cap for those classes -- a template
// ValidateWithOptions (phase 1) accepted outright could be rejected by Submit
// (phase 2) purely because two walks shared one allowance, with no way for
// the submitter to see why. Two constructions show it, both used by
// TestSubmit_ValidateAcceptsMustNotRejectOnBudgetAlone below; with a shared
// budget:
//
//   - 1 step, 6 let bindings x 900,000 bytes each (~5.4 MB, comfortably under
//     the 10 MB per-walk cap ALONE) -- ValidateWithOptions accepts; Submit
//     rejects with "... reached 10800768" -- 5.4 MB counted TWICE.
//   - 6 task-parameter definitions x 1,000 trivial RangeList entries (6,000
//     positions, comfortably under the 10,000 per-walk cap ALONE) --
//     ValidateWithOptions accepts; Submit rejects with "... reached 10001"
//     -- 6,000 counted twice plus the job name.
//
// HOW TO MUTATION-TEST THESE (as in exprcheck_budget_test.go): set the KNOB
// -- pass newTemplateBudget(ExprLimits{TemplatePositions: n}) (or
// TemplateRetainedBytes) as the threaded budget -- or neuter the comparison
// inside templateBudget.chargePositions/chargeRetainedBytes (exprcheck.go,
// reading b.limits.TemplatePositions). Do NOT raise
// defaultTemplatePositions/defaultTemplateRetainedBytes: several tests below
// size their own construction from those live constants, and raising them
// makes the construction try to allocate on the order of the raised value
// rather than failing fast.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// manyRangeEntries returns n trivial, cheap-to-evaluate RangeList entries --
// exprcheck_budget_test.go's manyArgs, adapted for a task-parameter's
// RangeList: no retained bytes (resolveRangeListEntry discards its own
// intermediate work the same way checkFormatString does) and negligible
// per-position operation/memory cost, isolating the POSITIONS dimension from
// the retained-bytes one.
func manyRangeEntries(n int) []string {
	entries := make([]string, n)
	for i := range entries {
		entries[i] = "{{ 'a' }}"
	}
	return entries
}

// singleStepParamSpaceTemplate builds a minimal single-step EXPR template
// around ps, for the tests below that only care about ResolveParameterSpaceParams'
// own behavior against one step's parameter space.
func singleStepParamSpaceTemplate(ps *StepParameterSpace, let []string) *JobTemplate {
	return &JobTemplate{
		Name:       "T",
		Extensions: []string{"EXPR"},
		Steps: []StepTemplate{{
			Name:           "Step1",
			Let:            let,
			LetSet:         len(let) > 0,
			ParameterSpace: ps,
		}},
	}
}

// TestResolveParameterSpaceParams_TemplateWideBudget_PositionsDimension
// isolates the POSITIONS dimension for the resolver called ON ITS OWN (no
// external budget threaded in -- ResolveParameterSpaceParams' own
// templateBudgetOrFresh gives it a fresh allowance): the measured
// construction (16 task-parameter definitions x 1024 RangeList entries, one
// step), reused directly from
// TestCheckTemplateExpressions_TemplateWideBudget_CatchesWhatThePreWalkGuardAdmits
// (exprcheck_budget_test.go) because it is already proven to sit WITHIN
// every structural cap (parameterSpaceOverCaps, maxSteps). The template-wide
// budget is the ONLY thing that can still catch it; without one in the
// resolver, this exact construction would reach ResolveParameterSpaceParams
// (and, in production, ExpandParameterSpace beyond it) completely unbounded.
//
// Mutation target: commenting out chargePositions' cap check (or raising
// defaultTemplatePositions past this construction's size) must make this
// test start accepting.
func TestResolveParameterSpaceParams_TemplateWideBudget_PositionsDimension(t *testing.T) {
	const numDefs = maxTaskParameterDefinitions // 16
	const numValues = maxTaskParamValues        // 1024

	defs := make([]TaskParamDefinition, numDefs)
	for i := range defs {
		defs[i] = TaskParamDefinition{
			Name:      fmt.Sprintf("P%d", i),
			Type:      TaskParamTypeString,
			RangeList: manyRangeEntries(numValues),
		}
	}
	ps := &StepParameterSpace{TaskParameterDefinitions: defs}
	tmpl := singleStepParamSpaceTemplate(ps, nil)

	if parameterSpaceOverCaps(tmpl) {
		t.Fatal("test setup is wrong: this construction must sit WITHIN maxTaskParameterDefinitions " +
			"and maxTaskParamValues (16 and 1024 are 'at most', not 'fewer than'), or a structural cap " +
			"would already reject it upstream and this test would prove nothing about the resolver's " +
			"own budget")
	}

	resolved, errs := ResolveParameterSpaceParams(tmpl, &tmpl.Steps[0], ps, nil)
	if len(errs) == 0 {
		t.Fatal("16 x 1024 range positions, within every existing structural cap, must be rejected by " +
			"the resolver's own template-wide budget on position count alone")
	}
	if resolved != nil {
		t.Fatalf("resolver reported errors but returned a non-nil space: %+v", resolved)
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

// TestResolveParameterSpaceParams_TemplateWideBudget_RetainedBytesDimension
// isolates the RETAINED-BYTES dimension for the resolver called on its own.
// Unlike the checker's own retained-bytes test (which needs several STEPS to
// show the budget is cumulative ACROSS blocks, since checkTemplateExpressions
// walks every step in one call), ResolveParameterSpaceParams processes only
// ONE step per call -- so a single step's own let: block, sized just over the
// 10,000,000-byte limit on its own, is enough to isolate this dimension here.
//
// 12 bindings x 900,064 bytes (expr.SizeOf: a 64-byte header plus a
// 900,000-byte string) = 10,800,768 bytes, over the budget; each binding's
// own Eval (900,000 bytes) stays comfortably under defaultSubmissionMemoryBytes
// (1,000,000 bytes) and the block's own count (12) stays comfortably under
// maxLetBindings (50), so neither of THOSE per-binding/per-block bounds is
// what rejects this construction.
func TestResolveParameterSpaceParams_TemplateWideBudget_RetainedBytesDimension(t *testing.T) {
	const bindingCount = 12
	const bytesPerBinding = 900_000

	lets := make([]string, bindingCount)
	for i := range lets {
		lets[i] = fmt.Sprintf("a%d = \"x\" * %d", i, bytesPerBinding)
	}
	ps := &StepParameterSpace{TaskParameterDefinitions: []TaskParamDefinition{{
		Name: "P", Type: TaskParamTypeString, RangeList: []string{"{{ 'a' }}"},
	}}}
	tmpl := singleStepParamSpaceTemplate(ps, lets)

	resolved, errs := ResolveParameterSpaceParams(tmpl, &tmpl.Steps[0], ps, nil)
	if len(errs) == 0 {
		t.Fatalf("%d lets of ~%d bytes each (~%d bytes total), over the %d-byte template-wide "+
			"retained-bytes budget even though the block's own count and each binding's own size are "+
			"both individually compliant, must be rejected",
			bindingCount, bytesPerBinding+64, bindingCount*(bytesPerBinding+64), defaultTemplateRetainedBytes)
	}
	if resolved != nil {
		t.Fatalf("resolver reported errors but returned a non-nil space: %+v", resolved)
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
}

// TestResolveParameterSpaceParams_TemplateWideBudget_FreshPerCall pins the
// same "one budget per call, not a shared/leaked one" property
// TestCheckTemplateExpressions_TemplateWideBudget_FreshPerCall pins for
// checkTemplateExpressions, here for ResolveParameterSpaceParams called with
// no external budget: a template sized just under the position cap must be
// accepted on every independent call.
func TestResolveParameterSpaceParams_TemplateWideBudget_FreshPerCall(t *testing.T) {
	n := int(defaultTemplatePositions) - 10 // comfortably under the cap
	ps := &StepParameterSpace{TaskParameterDefinitions: []TaskParamDefinition{{
		Name: "P", Type: TaskParamTypeString, RangeList: manyRangeEntries(n),
	}}}
	tmpl := singleStepParamSpaceTemplate(ps, nil)

	for i := range 2 {
		if _, errs := ResolveParameterSpaceParams(tmpl, &tmpl.Steps[0], ps, nil); len(errs) != 0 {
			t.Fatalf("call %d: a step under the budget must be accepted on every independent call, not "+
				"just the first (a shared/leaked budget would make later calls stricter than earlier "+
				"ones): %v", i, errs)
		}
	}
}

// TestResolveParameterSpaceParams_TemplateWideBudget_BaseSpecUnaffected pins
// the base-spec cost floor for the resolver, mirroring the checker's own: a
// template that does not declare extensions: [EXPR] takes the resolver's
// exact base-spec substitution path, with no budget interaction of any kind
// -- a construction that WOULD trip the positions budget under EXPR must
// still resolve with zero errors when the template does not declare it,
// because fmtstring.Resolve (the base-spec path) does not even accept a
// "{{ 'a' }}" expression body as a valid dotted-identifier reference in the
// first place; if it silently succeeded that would itself be evidence the
// exprEnabled gate had failed open.
func TestResolveParameterSpaceParams_TemplateWideBudget_BaseSpecUnaffected(t *testing.T) {
	n := int(defaultTemplatePositions) + 50
	entries := make([]string, n)
	for i := range entries {
		entries[i] = fmt.Sprintf("Value%d", i) // a valid BASE-SPEC dotted identifier, not an expression
	}
	ps := &StepParameterSpace{TaskParameterDefinitions: []TaskParamDefinition{{
		Name: "P", Type: TaskParamTypeString, RangeList: entries,
	}}}
	tmpl := &JobTemplate{
		Name:  "T", // no Extensions: EXPR is not declared
		Steps: []StepTemplate{{Name: "Step1", ParameterSpace: ps}},
	}

	jobParams := make(map[string]string, n)
	for i := range entries {
		jobParams["Value"+strconv.Itoa(i)] = "x"
	}

	if _, errs := ResolveParameterSpaceParams(tmpl, &tmpl.Steps[0], ps, jobParams); len(errs) != 0 {
		t.Fatalf("a non-EXPR template's resolver call must never interact with the budget, regardless "+
			"of position count: %v", errs)
	}
}

// TestPhase2Budget_CheckerAndResolverHaveIndependentBudgets is the central
// unit-level proof that the checker's phase-2 re-check and every step's
// resolver call get TWO SEPARATE budgets for one submission, not one shared
// budget: "one budget per phase" means one budget per WALK per phase, not
// "one budget for the whole phase, however many walks it has".
//
// The construction -- 6 task-parameter definitions x 1,000 trivial RangeList
// entries, one step -- is sized so EACH walk, run alone, comfortably fits
// under defaultTemplatePositions (10,000): the checker's own phase-2 walk
// charges ~6,001 positions (6,000 range entries plus the job name), and the
// resolver's walk charges ~6,000 (the same 6,000 range entries, re-walked:
// these positions are evaluated TWICE). Run through TWO SEPARATE budgets,
// exactly as submit.go's Submit does, both walks must still accept --
// proving the two do not interfere with each other's allowance.
func TestPhase2Budget_CheckerAndResolverHaveIndependentBudgets(t *testing.T) {
	const numDefs = 6
	const numValues = 1000

	defs := make([]TaskParamDefinition, numDefs)
	for i := range defs {
		defs[i] = TaskParamDefinition{
			Name:      fmt.Sprintf("P%d", i),
			Type:      TaskParamTypeString,
			RangeList: manyRangeEntries(numValues),
		}
	}
	ps := &StepParameterSpace{TaskParameterDefinitions: defs}
	tmpl := singleStepParamSpaceTemplate(ps, nil)

	if parameterSpaceOverCaps(tmpl) {
		t.Fatal("test setup is wrong: this construction must sit within maxTaskParameterDefinitions " +
			"and maxTaskParamValues")
	}

	t.Run("phase 1 accepts", func(t *testing.T) {
		if errs := checkTemplateExpressions(tmpl, nil); len(errs) != 0 {
			t.Fatalf("phase 1 (its own fresh budget) must accept this construction: %v", errs)
		}
	})

	t.Run("phase-2 checker walk alone accepts", func(t *testing.T) {
		if err := checkExpressionsAtSubmit(tmpl, nil); err != nil {
			t.Fatalf("the checker's phase-2 walk, run with its own fresh budget (no external one "+
				"supplied), must accept this construction on its own: %v", err)
		}
	})

	t.Run("resolver alone accepts", func(t *testing.T) {
		if _, errs := ResolveParameterSpaceParams(tmpl, &tmpl.Steps[0], ps, nil); len(errs) != 0 {
			t.Fatalf("the resolver, run with its own fresh budget (no external one supplied), must "+
				"accept this construction on its own: %v", errs)
		}
	})

	t.Run("both accept when each has its OWN budget, exactly as Submit wires them", func(t *testing.T) {
		checkerBudget := newTemplateBudget(ExprLimits{})
		resolverBudget := newTemplateBudget(ExprLimits{})

		if err := checkExpressionsAtSubmit(tmpl, nil, checkerBudget); err != nil {
			t.Fatalf("the checker, spending against its OWN budget, must accept this construction: %v", err)
		}

		_, errs := ResolveParameterSpaceParams(tmpl, &tmpl.Steps[0], ps, nil, resolverBudget)
		if len(errs) != 0 {
			t.Fatalf("the resolver, spending against its OWN SEPARATE budget (not the checker's), must "+
				"also accept this construction -- if this fails, the two budgets are sharing state again: %v",
				errs)
		}
	})
}

// rangePositionsTemplateYAML builds the raw YAML for numDefs task-parameter
// definitions x numValues trivial RangeList entries each, one step -- the
// shared construction TestPhase2Budget_CheckerAndResolverHaveIndependentBudgets
// exercises at the unit level and the two Submit-level tests below exercise
// end to end.
func rangePositionsTemplateYAML(name string, numDefs, numValues int) string {
	var b strings.Builder
	b.WriteString("specificationVersion: jobtemplate-2023-09\n")
	b.WriteString("extensions:\n- EXPR\n")
	fmt.Fprintf(&b, "name: %s\n", name)
	b.WriteString("steps:\n- name: Step1\n")
	b.WriteString("  script:\n    actions:\n      onRun:\n        command: echo\n")
	b.WriteString("  parameterSpace:\n")
	// combination ZIPS every definition together (equal-length ranges, one
	// row per index) instead of the default Cartesian product -- without it,
	// numDefs definitions of numValues entries each expand to numValues^numDefs
	// tasks (6 x 1,000 would be 10^18, far over maxTasksPerStep), which has
	// nothing to do with the budget this construction is sized to exercise.
	// Zipping keeps the resulting task count at exactly numValues regardless
	// of numDefs, so ExpandParameterSpace's own resource-exhaustion guard
	// (expand.go's maxTasksPerStep) never enters into what these tests are
	// asserting.
	names := make([]string, numDefs)
	for i := range numDefs {
		names[i] = fmt.Sprintf("P%d", i)
	}
	fmt.Fprintf(&b, "    combination: \"(%s)\"\n", strings.Join(names, ","))
	b.WriteString("    taskParameterDefinitions:\n")
	for i := range numDefs {
		fmt.Fprintf(&b, "    - name: P%d\n      type: STRING\n      range:\n", i)
		for range numValues {
			b.WriteString("      - \"{{ 'a' }}\"\n")
		}
	}
	return b.String()
}

// letBytesTemplateYAML builds the raw YAML for one step whose OWN let: block
// binds bindingCount names, each to a bytesEach-byte string -- the other
// construction TestSubmit_ValidateAcceptsMustNotRejectOnBudgetAlone uses. A
// trivial one-entry parameterSpace is included so the resolver's stepLet
// charge actually runs (ResolveParameterSpaceParams returns before charging
// anything for a step with NO parameterSpace at all -- see that function's
// own doc comment on the ps == nil early return).
func letBytesTemplateYAML(name string, bindingCount, bytesEach int) string {
	var b strings.Builder
	b.WriteString("specificationVersion: jobtemplate-2023-09\n")
	b.WriteString("extensions:\n- EXPR\n")
	fmt.Fprintf(&b, "name: %s\n", name)
	b.WriteString("steps:\n- name: Step1\n")
	b.WriteString("  let:\n")
	for i := range bindingCount {
		fmt.Fprintf(&b, "  - a%d = \"x\" * %d\n", i, bytesEach)
	}
	b.WriteString("  script:\n    actions:\n      onRun:\n        command: echo\n")
	b.WriteString("  parameterSpace:\n    taskParameterDefinitions:\n")
	b.WriteString("    - name: P\n      type: STRING\n      range:\n      - \"{{ 'a' }}\"\n")
	return b.String()
}

// TestSubmit_PhaseTwoBudget_ChecksAndResolverEachGetOwnAllowance is the
// end-to-end proof, through the public Submitter.Submit API, that the split
// phase-2 budgets are live in production: a submission whose checker walk
// and resolver walk EACH individually fit under the 10,000-position cap must
// SUCCEED, not fail, because prepareTemplate gives them two separate budgets
// instead of one shared one.
func TestSubmit_PhaseTwoBudget_ChecksAndResolverEachGetOwnAllowance(t *testing.T) {
	ctx := context.Background()
	st := fake.New()
	farm, err := st.CreateFarm(ctx, store.Farm{ID: uuid.NewString(), Name: "budget-farm"})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err := st.CreateQueue(ctx, store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: "budget-queue"})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}

	sub := NewSubmitter(st)

	raw := rangePositionsTemplateYAML("Phase2BudgetJob", 6, 1000)
	res, err := sub.Submit(ctx, raw, store.TemplateFormatYAML, SubmitOptions{
		FarmID: farm.ID, QueueID: queue.ID,
	})
	if err != nil {
		t.Fatalf("a submission whose checker walk (~6,001 positions) and resolver walk (~6,000 "+
			"positions) EACH individually fit under the 10,000-position cap must succeed once the two "+
			"walks have separate budgets: %v", err)
	}
	if res == nil || res.Job.ID == "" {
		t.Fatal("expected a persisted job, got none")
	}
}

// TestSubmit_Phase1SpendDoesNotCarryIntoPhase2 proves, through the public
// Submitter.Submit API rather than the lower-level checker/resolver
// functions TestPhase2Budget_CheckerAndResolverHaveIndependentBudgets
// exercises directly, that a template that spends CLOSE TO THE FULL
// position cap in phase 1 (ValidateWithOptions, inside prepareTemplate) does
// not fail phase 2 (checkExpressionsAtSubmit plus every step's
// ResolveParameterSpaceParams) merely because phase 1 already spent that
// budget -- the property per-phase scoping exists to guarantee.
//
// The construction deliberately spends its near-cap position count on ARGS
// entries, NOT range positions: an args entry is a CHECKER-ONLY position (the
// resolver never touches Action.Args at all), so phase 2's own combined
// spend here is checker-walk-only (~9,992) with ZERO resolver contribution
// (this step declares no parameterSpace) -- comfortably under the cap on its
// own. If phase 1's ~9,992-position spend leaked into the SAME budget phase 2
// draws from, phase 2's OWN re-walk of the identical ~9,992 positions would
// immediately push the shared total to ~19,984 and this submission would be
// rejected. It is not: Submit succeeds, and the job's task is persisted --
// proof that prepareTemplate's ValidateWithOptions call (phase 1) and its
// own, separately allocated checkerBudget (phase 2) are genuinely two
// different [templateBudget] objects, not one threaded too far.
func TestSubmit_Phase1SpendDoesNotCarryIntoPhase2(t *testing.T) {
	ctx := context.Background()
	st := fake.New()
	farm, err := st.CreateFarm(ctx, store.Farm{ID: uuid.NewString(), Name: "t4-indep-farm"})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err := st.CreateQueue(ctx, store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: "t4-indep-queue"})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}

	sub := NewSubmitter(st)

	const numArgs = 9990 // 1 (name) + 1 (command) + numArgs stays just under defaultTemplatePositions
	var b strings.Builder
	b.WriteString("specificationVersion: jobtemplate-2023-09\n")
	b.WriteString("extensions:\n- EXPR\n")
	b.WriteString("name: Phase1IndependenceJob\n")
	b.WriteString("steps:\n- name: Step1\n")
	b.WriteString("  script:\n    actions:\n      onRun:\n        command: echo\n        args:\n")
	for range numArgs {
		b.WriteString("        - \"{{ 'a' }}\"\n")
	}

	res, err := sub.Submit(ctx, b.String(), store.TemplateFormatYAML, SubmitOptions{
		FarmID: farm.ID, QueueID: queue.ID,
	})
	if err != nil {
		t.Fatalf("a template that spends most of the position cap in phase 1 must still submit "+
			"successfully -- phase 2 gets its OWN fresh budget, not what phase 1 already spent: %v", err)
	}
	if res == nil || res.Job.ID == "" {
		t.Fatal("expected a persisted job, got none")
	}
	if len(res.Tasks) != 1 {
		t.Fatalf("expected exactly one task (no parameterSpace declared), got %d", len(res.Tasks))
	}
}

// TestSubmit_ValidateAcceptsMustNotRejectOnBudgetAlone is the direct
// property test: for a template [ValidateWithOptions] (phase 1) accepts,
// [Submitter.Submit] (phase 2) must not reject it purely because phase 2 has
// two walks sharing one budget -- the same property
// TestSubmit_Phase1SpendDoesNotCarryIntoPhase2 proves for phase 1 vs. phase 2,
// one level down.
//
// Both constructions from this file's header are exercised, one per
// dimension, each in its own subtest:
//
//   - "let bytes": one step, 6 let bindings x 900,000 bytes each (~5.4 MB,
//     under the 10 MB per-walk cap ALONE, over it if the checker's and the
//     resolver's charges for the SAME let block are pooled).
//   - "range positions": 6 task-parameter definitions x 1,000 trivial
//     RangeList entries (6,000 positions, under the 10,000 per-walk cap
//     ALONE, over it -- ~12,000 -- if the checker's and the resolver's
//     charges for the SAME range entries are pooled).
//
// Each subtest confirms ValidateWithOptions accepts the construction
// DIRECTLY (not merely assumed), then confirms Submit also accepts it --
// the property itself, not an inference from either walk's own isolated
// pass/fail the way TestPhase2Budget_CheckerAndResolverHaveIndependentBudgets
// checks it at the lower level.
func TestSubmit_ValidateAcceptsMustNotRejectOnBudgetAlone(t *testing.T) {
	ctx := context.Background()
	st := fake.New()
	farm, err := st.CreateFarm(ctx, store.Farm{ID: uuid.NewString(), Name: "t4-property-farm"})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err := st.CreateQueue(ctx, store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: "t4-property-queue"})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	sub := NewSubmitter(st)

	cases := []struct {
		name string
		raw  string
	}{
		{"let bytes: 6 x 900,000-byte bindings, one step", letBytesTemplateYAML("PropertyLetBytesJob", 6, 900_000)},
		{"range positions: 6 x 1,000 trivial entries", rangePositionsTemplateYAML("PropertyPositionsJob", 6, 1000)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpl, perr := Parse([]byte(tc.raw), FormatYAML)
			if perr != nil {
				t.Fatalf("test setup: Parse: %v", perr)
			}
			if errs := ValidateWithOptions(tmpl, ValidateOptions{EnforceLimits: true}); len(errs) != 0 {
				t.Fatalf("test setup: ValidateWithOptions must accept this construction (phase 1, its "+
					"own independent budget): %v", errs)
			}

			_, err := sub.Submit(ctx, tc.raw, store.TemplateFormatYAML, SubmitOptions{
				FarmID: farm.ID, QueueID: queue.ID,
			})
			if err != nil {
				t.Fatalf("ValidateWithOptions accepted this template, but Submit rejected it: %v -- "+
					"a phase-2 rejection here can only be about the budget (nothing else differs "+
					"between phase 1 and phase 2 for this construction), and an accepted template "+
					"must not be rejected on budget alone", err)
			}
		})
	}
}
