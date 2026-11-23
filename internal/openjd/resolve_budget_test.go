// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

// The resolver's share of the template-wide budget (exprcheck.go's
// templateBudget). This file tests that:
//
//   - ResolveParameterSpaceParams (resolve.go) is bounded on its own, in both
//     dimensions, like checkTemplateExpressions: a template the checker's walk
//     rejects is also rejected here.
//   - submit.go's Submit gives the checker's phase-2 re-check
//     (checkExpressionsAtSubmit) and every step's ResolveParameterSpaceParams
//     call two separate budgets for one submission.
//
// The two walks charge the same range positions and let bytes for the same
// submission (these positions are evaluated twice), so one shared budget
// would halve the effective cap for those classes: a template
// ValidateWithOptions (phase 1) accepts could be rejected by Submit (phase 2)
// only because two walks shared one allowance, with no way for the submitter
// to see why. TestSubmit_ValidateAcceptsMustNotRejectOnBudgetAlone uses two
// constructions that a shared budget would reject:
//
//   - 1 step, 6 let bindings x 900,000 bytes each (about 5.4 MB, under the
//     10 MB per-walk cap), which a shared budget counts twice.
//   - 6 task-parameter definitions x 1,000 trivial RangeList entries (6,000
//     positions, under the 10,000 per-walk cap), which a shared budget counts
//     twice plus the job name.
//
// To mutation-test these (as in exprcheck_budget_test.go), pass
// newTemplateBudget(ExprLimits{TemplatePositions: n}) (or
// TemplateRetainedBytes) as the threaded budget, or disable the comparison
// inside templateBudget.chargePositions/chargeRetainedBytes (exprcheck.go,
// reading b.limits.TemplatePositions). Do not raise
// defaultTemplatePositions/defaultTemplateRetainedBytes: several tests below
// size their construction from those constants, and raising them makes the
// construction allocate on the order of the raised value.

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

// manyRangeEntries returns n trivial RangeList entries, the task-parameter
// counterpart of exprcheck_budget_test.go's manyArgs: no retained bytes
// (resolveRangeListEntry discards its intermediate work as checkFormatString
// does) and negligible per-position operation and memory cost, isolating the
// positions dimension from the retained-bytes one.
func manyRangeEntries(n int) []string {
	entries := make([]string, n)
	for i := range entries {
		entries[i] = "{{ 'a' }}"
	}
	return entries
}

// singleStepParamSpaceTemplate builds a minimal single-step EXPR template
// around ps, for the tests below that exercise ResolveParameterSpaceParams
// against one step's parameter space.
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
// isolates the positions dimension for the resolver called on its own, with no
// external budget (templateBudgetOrFresh gives it a fresh allowance). The
// construction (16 task-parameter definitions x 1024 RangeList entries, one
// step) is the one
// TestCheckTemplateExpressions_TemplateWideBudget_CatchesWhatThePreWalkGuardAdmits
// (exprcheck_budget_test.go) uses, because it sits within every structural
// cap (parameterSpaceOverCaps, maxSteps). Only the template-wide budget can
// reject it; without one in the resolver, it would reach
// ResolveParameterSpaceParams (and ExpandParameterSpace after it) unbounded.
//
// Mutation target: disabling chargePositions' cap check (or raising
// defaultTemplatePositions past this construction's size) makes this test
// accept.
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
// isolates the retained-bytes dimension for the resolver called on its own.
// The checker's retained-bytes test needs several steps to show the budget is
// cumulative across blocks, since checkTemplateExpressions walks every step in
// one call; ResolveParameterSpaceParams processes one step per call, so a
// single let: block sized just over the 10,000,000-byte limit isolates this
// dimension.
//
// 12 bindings x 900,064 bytes (expr.SizeOf: a 64-byte header plus a
// 900,000-byte string) = 10,800,768 bytes, over the budget. Each binding's
// Eval (900,000 bytes) stays under defaultSubmissionMemoryBytes (1,000,000
// bytes) and the block's count (12) stays under maxLetBindings (50), so
// neither of those bounds is what rejects this construction.
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

// TestResolveParameterSpaceParams_TemplateWideBudget_FreshPerCall pins that
// ResolveParameterSpaceParams called with no external budget gets a fresh
// budget per call, as TestCheckTemplateExpressions_TemplateWideBudget_FreshPerCall
// pins for checkTemplateExpressions: a template sized just under the position
// cap is accepted on every independent call.
func TestResolveParameterSpaceParams_TemplateWideBudget_FreshPerCall(t *testing.T) {
	n := int(defaultTemplatePositions) - 10 // under the cap
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
// that the resolver's base-spec path, like the checker's, has no budget
// interaction: a template that does not declare extensions: [EXPR] takes the
// base-spec substitution path, so a position count that would trip the
// positions budget under EXPR still resolves with zero errors. The entries
// are dotted identifiers, because fmtstring.Resolve (the base-spec path) does
// not accept an expression body such as "{{ 'a' }}".
func TestResolveParameterSpaceParams_TemplateWideBudget_BaseSpecUnaffected(t *testing.T) {
	n := int(defaultTemplatePositions) + 50
	entries := make([]string, n)
	for i := range entries {
		entries[i] = fmt.Sprintf("Value%d", i) // a base-spec dotted identifier, not an expression
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

// TestPhase2Budget_CheckerAndResolverHaveIndependentBudgets pins, at the unit
// level, that the checker's phase-2 re-check and every step's resolver call
// get two separate budgets for one submission: one budget per walk per phase.
//
// The construction (6 task-parameter definitions x 1,000 trivial RangeList
// entries, one step) is sized so each walk alone fits under
// defaultTemplatePositions (10,000): the checker's phase-2 walk charges about
// 6,001 positions (6,000 range entries plus the job name), and the resolver's
// walk charges about 6,000 (the same range entries, evaluated a second time).
// Run through two separate budgets, as submit.go's Submit does, both walks
// accept.
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
				"also accept this construction; a failure means the two budgets share state: %v",
				errs)
		}
	})
}

// rangePositionsTemplateYAML builds the raw YAML for numDefs task-parameter
// definitions x numValues trivial RangeList entries each, one step: the
// construction TestPhase2Budget_CheckerAndResolverHaveIndependentBudgets
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
	// combination zips every definition together (equal-length ranges, one
	// row per index) instead of the default Cartesian product, which would
	// expand to numValues^numDefs tasks (10^18 for 6 x 1,000, far over
	// maxTasksPerStep). Zipping keeps the task count at numValues regardless
	// of numDefs, so expand.go's maxTasksPerStep does not affect these tests.
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

// letBytesTemplateYAML builds the raw YAML for one step whose let: block binds
// bindingCount names, each to a bytesEach-byte string, the other construction
// TestSubmit_ValidateAcceptsMustNotRejectOnBudgetAlone uses. A trivial
// one-entry parameterSpace is included so the resolver charges the let: block:
// ResolveParameterSpaceParams returns before charging anything for a step with
// no parameterSpace (see its ps == nil early return).
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

// TestSubmit_PhaseTwoBudget_ChecksAndResolverEachGetOwnAllowance tests the
// split phase-2 budgets through the public Submitter.Submit API: a submission
// whose checker walk and resolver walk each fit under the 10,000-position cap
// succeeds, because prepareTemplate gives them two separate budgets.
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
			"positions) each fit under the 10,000-position cap must succeed, because the two "+
			"walks have separate budgets: %v", err)
	}
	if res == nil || res.Job.ID == "" {
		t.Fatal("expected a persisted job, got none")
	}
}

// TestSubmit_Phase1SpendDoesNotCarryIntoPhase2 tests, through the public
// Submitter.Submit API, that a template spending close to the full position
// cap in phase 1 (ValidateWithOptions, inside prepareTemplate) does not fail
// phase 2 (checkExpressionsAtSubmit plus every step's
// ResolveParameterSpaceParams) because phase 1 already spent that budget.
// TestPhase2Budget_CheckerAndResolverHaveIndependentBudgets covers the
// checker and resolver functions directly.
//
// The construction spends its position count on args entries, which only the
// checker evaluates (the resolver never reads Action.Args), and the step
// declares no parameterSpace, so phase 2 spends about 9,992 positions, all in
// the checker walk. If phase 1's spend leaked into the budget phase 2 draws
// from, the total would reach about 19,984 and the submission would be
// rejected. Submit succeeding shows that prepareTemplate's ValidateWithOptions
// call (phase 1) and its checkerBudget (phase 2) are separate
// [templateBudget] objects.
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

// TestSubmit_ValidateAcceptsMustNotRejectOnBudgetAlone tests the property
// directly: a template [ValidateWithOptions] (phase 1) accepts must not be
// rejected by [Submitter.Submit] (phase 2) because phase 2's two walks share
// one budget. TestSubmit_Phase1SpendDoesNotCarryIntoPhase2 tests the same
// property between phase 1 and phase 2.
//
// Both constructions from this file's header are exercised, one per
// dimension, each in its own subtest:
//
//   - "let bytes": one step, 6 let bindings x 900,000 bytes each (about
//     5.4 MB, under the 10 MB per-walk cap, over it if the checker's and the
//     resolver's charges for the same let block are pooled).
//   - "range positions": 6 task-parameter definitions x 1,000 trivial
//     RangeList entries (6,000 positions, under the 10,000 per-walk cap, about
//     12,000 if the checker's and the resolver's charges for the same range
//     entries are pooled).
//
// Each subtest checks that ValidateWithOptions accepts the construction, then
// that Submit also accepts it.
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
