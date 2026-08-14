// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtres_test

// Tests for assignmentbudget.go -- EXPR sub-project E4c's Task 4, the
// worker's share of the template-wide budget design spec §3 introduces.
//
// Two dimensions, asserted through SEPARATE tests exactly as
// internal/openjd/exprcheck_budget_test.go's own note requires ("two
// dimensions that share one test are one dimension"): removing either bound
// in isolation must fail exactly one dimension's tests, not both.
//
// MUTATION-TESTING TRAP, carried forward from the server-side file's own
// note: the correct way to mutation-test either bound is to comment out the
// threshold check inside AssignmentBudget.ChargePositions/
// ChargeRetainedBytes (assignmentbudget.go), not to raise
// MaxAssignmentPositions/assignmentMaxRetainedBytes -- several tests below
// size their own construction from those live constants.

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// wantAssignmentMaxPositions is assignmentbudget.go's own cap, read from the
// package rather than mirrored as a literal.
//
// It WAS a hand-copied 5,000, with a comment explaining that an external test
// package (fmtres_test) cannot see an unexported constant. Fix round 2
// (whole-branch review, IMPORTANT 1) exported the constant -- because
// internal/openjd's own TestTemplateBudget_WorkerCapIsNotTighter has to read
// it to pin the server/worker relation, and once it is exported there is no
// reason for this file to carry a copy that can drift from it.
//
// E4d Task 2 renamed it [fmtres.DefaultAssignmentPositions]: it is now the
// DEFAULT for a configurable limit rather than the only possible value. The
// tests in this file all construct their budget with ExprLimits{}, so they
// exercise exactly that default; the configured value is exercised by
// exprlimits_test.go instead.
const wantAssignmentMaxPositions = fmtres.DefaultAssignmentPositions

// ── AssignmentBudget: direct unit tests ─────────────────────────────────────

// TestAssignmentBudget_PositionsDimension isolates the POSITIONS dimension at
// the [fmtres.AssignmentBudget] level, with no phase-3 evaluation involved --
// just the counter itself. Mutation target: commenting out the threshold
// check inside ChargePositions must make this test start accepting.
func TestAssignmentBudget_PositionsDimension(t *testing.T) {
	b := fmtres.NewAssignmentBudget(fmtres.ExprLimits{})

	// Spend right up to the cap in two charges -- proves the counter is
	// cumulative across CALLS, not merely a single-call check. Sized from the
	// live constant so raising or lowering the cap cannot silently turn this
	// into a test of nothing.
	const first = wantAssignmentMaxPositions / 2
	if err := b.ChargePositions(first, "first batch"); err != nil {
		t.Fatalf("%d of %d must be accepted: %v", first, wantAssignmentMaxPositions, err)
	}
	if err := b.ChargePositions(wantAssignmentMaxPositions-first-1, "second batch"); err != nil {
		t.Fatalf("one under the cap must be accepted: %v", err)
	}
	if err := b.ChargePositions(1, "boundary"); err != nil {
		t.Fatalf("exactly %d must be accepted (the limit is inclusive): %v", wantAssignmentMaxPositions, err)
	}
	err := b.ChargePositions(1, "over the top")
	if err == nil {
		t.Fatalf("position %d must be rejected", wantAssignmentMaxPositions+1)
	}
	if !strings.Contains(err.Error(), "expression positions") {
		t.Errorf("error = %v, want it to name the positions dimension", err)
	}
	if !strings.Contains(err.Error(), "over the top") {
		t.Errorf("error = %v, want it to name the charge that tripped it", err)
	}

	// Once tripped, the SAME error is returned on every further call --
	// cheap no-ops, not a fresh evaluation of the threshold.
	err2 := b.ChargePositions(1, "irrelevant")
	if err2 == nil || err2.Error() != err.Error() {
		t.Errorf("a spent budget must keep returning the SAME error; got %v then %v", err, err2)
	}
	if b.Err() == nil {
		t.Error("Err() must report the tripped error once the budget is spent")
	}
}

// TestAssignmentBudget_RetainedBytesDimension is the RetainedBytes
// counterpart, isolating that dimension the same way. Mutation target:
// commenting out the threshold check inside ChargeRetainedBytes must make
// this test start accepting, and must NOT affect
// TestAssignmentBudget_PositionsDimension above.
func TestAssignmentBudget_RetainedBytesDimension(t *testing.T) {
	b := fmtres.NewAssignmentBudget(fmtres.ExprLimits{})

	if err := b.ChargeRetainedBytes(19_999_999, "first table"); err != nil {
		t.Fatalf("19,999,999 of 20,000,000 must be accepted: %v", err)
	}
	if err := b.ChargeRetainedBytes(1, "boundary"); err != nil {
		t.Fatalf("exactly 20,000,000 must be accepted (the limit is inclusive): %v", err)
	}
	err := b.ChargeRetainedBytes(1, "second table")
	if err == nil {
		t.Fatal("the 20,000,001st byte must be rejected")
	}
	if !strings.Contains(err.Error(), "retain") {
		t.Errorf("error = %v, want it to name the retained-bytes dimension", err)
	}
	if strings.Contains(err.Error(), "expression positions") {
		t.Errorf("the retained-bytes error must not also read like a positions error: %v", err)
	}

	// n <= 0 is a documented no-op, matching chargePositions' own contract --
	// it must not itself trip or clear the budget.
	fresh := fmtres.NewAssignmentBudget(fmtres.ExprLimits{})
	if err := fresh.ChargeRetainedBytes(0, "noop"); err != nil {
		t.Errorf("a zero charge must be a no-op: %v", err)
	}
	if err := fresh.ChargeRetainedBytes(-5, "noop"); err != nil {
		t.Errorf("a negative charge must be a no-op: %v", err)
	}
}

// TestAssignmentBudget_FreshPerAssignment pins "one budget per assignment,
// not a shared/leaked one" -- the same property Task 3's
// TestCheckTemplateExpressions_TemplateWideBudget_FreshPerCall pins
// server-side. session.Manager.Create allocates a NEW AssignmentBudget every
// call (session.go), so two independent sessions -- and therefore two
// independent [fmtres.NewAssignmentBudget] calls -- each get their own full
// allowance.
func TestAssignmentBudget_FreshPerAssignment(t *testing.T) {
	for i := range 2 {
		b := fmtres.NewAssignmentBudget(fmtres.ExprLimits{})
		if err := b.ChargePositions(wantAssignmentMaxPositions-10, fmt.Sprintf("assignment %d", i)); err != nil {
			t.Fatalf("assignment %d: a fresh budget must accept a charge just under the cap: %v", i, err)
		}
	}
}

// TestAssignmentBudget_ConcurrentCharges proves [fmtres.AssignmentBudget] is
// safe under concurrent use -- see that type's own doc comment for why this
// matters even though Phase 1 defers session reuse across TASKS: a session's
// own environment entry/exit and its task-table resolution can still race
// each other charging the same object, and a plain unsynchronized counter
// would corrupt updates under concurrent access (or fail -race even if this
// particular run happened not to observably corrupt one). This asserts the
// total is EXACT, not merely that nothing crashes: N goroutines each
// charging 1 position must leave the counter at exactly N, and exactly one
// of them must observe the charge that pushes the total over the cap.
func TestAssignmentBudget_ConcurrentCharges(t *testing.T) {
	const goroutines = 200
	b := fmtres.NewAssignmentBudget(fmtres.ExprLimits{})

	var wg sync.WaitGroup
	results := make([]error, goroutines)
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = b.ChargePositions(1, fmt.Sprintf("goroutine %d", i))
		}(i)
	}
	wg.Wait()

	var accepted, rejected int
	for _, err := range results {
		if err == nil {
			accepted++
		} else {
			rejected++
		}
	}
	// goroutines (200) is comfortably under MaxAssignmentPositions,
	// so every charge must have been accepted, and NONE lost to a race --
	// 200 successes is only possible if every one of the 200 concurrent
	// += 1 updates was applied.
	if accepted != goroutines || rejected != 0 {
		t.Fatalf("accepted=%d rejected=%d, want all %d accepted (a lost update under a race would "+
			"under-count positions and hide a real over-cap assignment)", accepted, rejected, goroutines)
	}
}

// ── AssignmentBudget wired through the phase-3 evaluators: cross-table proof ──

// simpleTaskLetMsg returns an AssignMsg whose step-script let: block binds
// exactly one name to a string of n bytes -- a minimal "one table" for the
// cross-table tests below.
func simpleTaskLetMsg(name string, n int) *protocol.AssignMsg {
	return &protocol.AssignMsg{
		EXPR:          true,
		StepScriptLet: []string{fmt.Sprintf(`%s = "x" * %d`, name, n)},
	}
}

// TestAssignmentBudget_AcrossTables_RetainedBytes is this task's central
// proof for the retained-bytes dimension: EXPR sub-project E4a's per-table
// bound (LetRetainedBytes, 10 MB) already bounds ONE table; this asserts the
// NEW bound sums across SEVERAL tables the SAME assignment builds, which
// nothing bounded before this task.
//
// Three tables (mirroring a task table plus two environment tables one
// session might enter), each retaining ~7,000,064 bytes -- individually
// comfortably under LetRetainedBytes (10,000,000) -- but the third table's
// own contribution pushes the ASSIGNMENT's cumulative total (21,000,192
// bytes) over AssignmentRetainedBytes (20,000,000), even though every table,
// considered alone, would pass the per-table check.
func TestAssignmentBudget_AcrossTables_RetainedBytes(t *testing.T) {
	const bytesPerTable = 7_000_000 // + 64-byte header = 7,000,064 via expr.SizeOf
	budget := fmtres.NewAssignmentBudget(fmtres.ExprLimits{})

	// Table 1: the task's own table.
	msg1 := simpleTaskLetMsg("a0", bytesPerTable)
	syms1, err := fmtres.TaskSymbols(msg1, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols (table 1): %v", err)
	}
	if err := fmtres.ApplyTaskLet(msg1, syms1, nil, budget); err != nil {
		t.Fatalf("table 1 (~%d bytes), well under both the per-table and assignment-wide budgets on "+
			"its own, must be accepted: %v", bytesPerTable, err)
	}

	// Table 2: one environment's table.
	env2 := &protocol.AssignEnvironment{Name: "Env1", Let: []string{fmt.Sprintf(`b0 = "x" * %d`, bytesPerTable)}}
	syms2, err := fmtres.EnvSymbols(&protocol.AssignMsg{}, env2, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("EnvSymbols (table 2): %v", err)
	}
	if err := fmtres.ApplyEnvLet(env2, syms2, nil, budget); err != nil {
		t.Fatalf("table 2 (~%d bytes; ~%d cumulative), still under the %d-byte assignment-wide budget, "+
			"must be accepted: %v", bytesPerTable, 2*(bytesPerTable+64), 20_000_000, err)
	}

	// Table 3: a second environment's table -- this is the one that tips the
	// ASSIGNMENT's cumulative total over the top, even though it is, on its
	// own, exactly as compliant as tables 1 and 2 were.
	env3 := &protocol.AssignEnvironment{Name: "Env2", Let: []string{fmt.Sprintf(`c0 = "x" * %d`, bytesPerTable)}}
	syms3, err := fmtres.EnvSymbols(&protocol.AssignMsg{}, env3, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("EnvSymbols (table 3): %v", err)
	}
	err = fmtres.ApplyEnvLet(env3, syms3, nil, budget)
	if err == nil {
		t.Fatalf("table 3, individually compliant (~%d bytes, under the per-table limit's own "+
			"10,000,000-byte per-table ceiling) but pushing the assignment's CUMULATIVE total to ~%d "+
			"bytes, must be rejected by the assignment-wide budget", bytesPerTable, 3*(bytesPerTable+64))
	}
	if !strings.Contains(err.Error(), "assignment-wide expression budget exceeded") ||
		!strings.Contains(err.Error(), "retain") {
		t.Errorf("error = %v, want it to name the assignment-wide retained-bytes budget", err)
	}
	if _, ok := syms3.Lookup("c0"); ok {
		t.Error(`"c0" should NOT be bound -- the assignment-wide budget was already spent before it could be`)
	}
}

// TestAssignmentBudget_AcrossActions_Positions is the positions-dimension
// counterpart: many resolutions, each individually trivial, across several
// SIMULATED environments' worth of variable resolution, none of which any
// PER-CALL limit (ExprLimits' OperationLimit/MemoryLimit) would catch --
// each call resolves a handful of plain-literal values, well within those.
// Only the assignment-wide budget sums the position COUNT across calls.
//
// The construction is sized from the live cap rather than hardcoded: 12 calls
// of one twelfth of it, plus enough overshoot to guarantee the last call
// trips, so that the largest any single call charges is nowhere close to the
// cap on its own and only the assignment-wide sum catches it.
func TestAssignmentBudget_AcrossActions_Positions(t *testing.T) {
	const calls = 12
	const perCall = int(wantAssignmentMaxPositions/calls) + 50

	budget := fmtres.NewAssignmentBudget(fmtres.ExprLimits{})
	var lastErr error
	var completed int
	for c := range calls {
		vars := make(map[string]string, perCall)
		for i := range perCall {
			vars[fmt.Sprintf("V%d_%d", c, i)] = "plain literal value with no reference"
		}
		_, err := fmtres.ResolveVarsExpr(vars, nil, nil, budget)
		if err != nil {
			lastErr = err
			break
		}
		completed++
	}

	if lastErr == nil {
		t.Fatalf("%d calls x %d positions (%d total), over the %d-position assignment-wide budget, "+
			"must be rejected somewhere -- got %d calls all accepted with no error",
			calls, perCall, calls*perCall, wantAssignmentMaxPositions, completed)
	}
	if completed >= calls {
		t.Errorf("expected the budget to trip strictly before the last (%dth) call; all %d completed",
			calls, completed)
	}
	if !strings.Contains(lastErr.Error(), "expression positions") {
		t.Errorf("error = %v, want it to name the positions dimension", lastErr)
	}
}

// TestResolveActionExpr_ChargesOnePlusArgsPositions pins the exact charge
// ResolveActionExpr makes per call: 1 (command) + len(args), so a caller
// combining it with other calls against the same budget can reason about the
// total precisely rather than by trial and error.
func TestResolveActionExpr_ChargesOnePlusArgsPositions(t *testing.T) {
	action := &protocol.Action{Command: "echo", Args: []string{"a", "b", "c"}}
	budget := fmtres.NewAssignmentBudget(fmtres.ExprLimits{})

	if _, err := fmtres.ResolveActionExpr(action, nil, nil, budget); err != nil {
		t.Fatalf("ResolveActionExpr: %v", err)
	}

	// Spend the budget down to exactly one position short of the cap, then
	// confirm the NEXT charge (from a second ResolveActionExpr call) is what
	// trips it -- pinning that the first call's own charge was exactly
	// 1 (command) + 3 (args) = 4, not some other count.
	const wantFirstCallCharge = 4
	remaining := wantAssignmentMaxPositions - wantFirstCallCharge
	if err := budget.ChargePositions(remaining, "filler"); err != nil {
		t.Fatalf("topping up to one-under-cap must succeed: %v", err)
	}
	if _, err := fmtres.ResolveActionExpr(&protocol.Action{Command: "x"}, nil, nil, budget); err == nil {
		t.Fatalf("a budget sitting at exactly (cap - %d) must be tripped by ONE more command position "+
			"if the first call's own charge was really %d, not some other number",
			wantFirstCallCharge, wantFirstCallCharge)
	}
}

// TestResolveEmbeddedFilesExpr_ChargesOnePerFile and
// TestResolveVarsExpr_ChargesOnePerVar are the same pin for the other two
// Resolve*Expr entry points, each charging exactly one position per entry
// with NO fixed "+1" the way the command adds one to ResolveActionExpr.
func TestResolveEmbeddedFilesExpr_ChargesOnePerFile(t *testing.T) {
	files := []protocol.EmbeddedFile{{Name: "a", Data: "1"}, {Name: "b", Data: "2"}}
	budget := fmtres.NewAssignmentBudget(fmtres.ExprLimits{})
	if _, err := fmtres.ResolveEmbeddedFilesExpr(files, nil, nil, budget); err != nil {
		t.Fatalf("ResolveEmbeddedFilesExpr: %v", err)
	}
	remaining := wantAssignmentMaxPositions - 2
	if err := budget.ChargeRetainedBytes(0, "noop"); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if err := budget.ChargePositions(remaining, "filler"); err != nil {
		t.Fatalf("topping up to one-under-cap must succeed: %v", err)
	}
	if _, err := fmtres.ResolveEmbeddedFilesExpr([]protocol.EmbeddedFile{{Name: "c", Data: "3"}}, nil, nil, budget); err == nil {
		t.Fatal("one more embedded file must trip a budget sitting at exactly (cap - 2)")
	}
}

func TestResolveVarsExpr_ChargesOnePerVar(t *testing.T) {
	vars := map[string]string{"K": "v"}
	budget := fmtres.NewAssignmentBudget(fmtres.ExprLimits{})
	if _, err := fmtres.ResolveVarsExpr(vars, nil, nil, budget); err != nil {
		t.Fatalf("ResolveVarsExpr: %v", err)
	}
	remaining := wantAssignmentMaxPositions - 1
	if err := budget.ChargePositions(remaining, "filler"); err != nil {
		t.Fatalf("topping up to one-under-cap must succeed: %v", err)
	}
	if _, err := fmtres.ResolveVarsExpr(map[string]string{"K2": "v"}, nil, nil, budget); err == nil {
		t.Fatal("one more variable must trip a budget sitting at exactly (cap - 1)")
	}
}

// TestAssignmentBudget_NilIsAFreshDefaultLimitedLedger pins the contract
// [budgetOrDefault] documents for a nil budget: the DEFAULT limits, on a
// ledger that is FRESH for that one call.
//
// E4d Task 2 fix round 1 made the budget a REQUIRED parameter at every phase-3
// entry point, so this is no longer "the call shape that omits it" -- omitting
// it does not compile. nil is what this package's own tests pass when the
// limits are not what they are testing, and roughly seventy call sites in this
// package depend on it behaving exactly like an unspent default budget.
//
// It is NOT subsumed by budgetguard_test.go's AST guard, which asserts the
// opposite half: that no PRODUCTION caller passes nil. This one asserts nil
// still works where it is legitimate.
func TestAssignmentBudget_NilIsAFreshDefaultLimitedLedger(t *testing.T) {
	action := &protocol.Action{Command: "echo", Args: []string{"hello"}}
	if _, err := fmtres.ResolveActionExpr(action, nil, nil, nil); err != nil {
		t.Fatalf("ResolveActionExpr with a nil budget must resolve an ordinary small action: %v", err)
	}
	if _, err := fmtres.ResolveVarsExpr(map[string]string{"K": "v"}, nil, nil, nil); err != nil {
		t.Fatalf("ResolveVarsExpr with a nil budget: %v", err)
	}
	if _, err := fmtres.ResolveEmbeddedFilesExpr([]protocol.EmbeddedFile{{Name: "a", Data: "1"}}, nil, nil, nil); err != nil {
		t.Fatalf("ResolveEmbeddedFilesExpr with a nil budget: %v", err)
	}

	// FRESH, not merely default-limited: two successive nil calls each
	// spending the WHOLE default position allowance must both succeed. A nil
	// that resolved to one shared package-level budget -- the obvious wrong
	// implementation -- would fail the second.
	vars := make(map[string]string, wantAssignmentMaxPositions)
	for i := range wantAssignmentMaxPositions {
		vars["V"+strconv.FormatInt(i, 10)] = "v"
	}
	for call := 1; call <= 2; call++ {
		if _, err := fmtres.ResolveVarsExpr(vars, nil, nil, nil); err != nil {
			t.Fatalf("nil-budget call %d spending the full default allowance (%d positions) must "+
				"succeed; a shared ledger would fail the second: %v", call, wantAssignmentMaxPositions, err)
		}
	}
}
