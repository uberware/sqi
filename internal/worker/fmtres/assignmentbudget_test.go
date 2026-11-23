// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtres_test

// Tests for assignmentbudget.go, the worker's assignment-wide expression
// budget.
//
// The two dimensions are asserted through separate tests, as in
// internal/openjd/exprcheck_budget_test.go: removing either bound alone must
// fail only that dimension's tests.
//
// To mutation-test either bound, comment out the threshold check inside
// AssignmentBudget.ChargePositions/ChargeRetainedBytes (assignmentbudget.go)
// rather than raising DefaultAssignmentPositions/
// defaultAssignmentRetainedBytes: several tests below size their
// construction from those constants.

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// wantAssignmentMaxPositions is assignmentbudget.go's cap, read from the
// package rather than copied as a literal (it is exported because
// internal/openjd's TestTemplateBudget_WorkerCapIsNotTighter reads it to pin
// the server/worker relation).
//
// [fmtres.DefaultAssignmentPositions] is the default for a configurable limit.
// The tests in this file construct their budget with ExprLimits{}, so they
// exercise that default; exprlimits_test.go exercises configured values.
const wantAssignmentMaxPositions = fmtres.DefaultAssignmentPositions

// ── AssignmentBudget: direct unit tests ─────────────────────────────────────

// TestAssignmentBudget_PositionsDimension isolates the positions dimension at
// the [fmtres.AssignmentBudget] level, with no phase-3 evaluation involved.
// Commenting out the threshold check inside ChargePositions makes it fail.
func TestAssignmentBudget_PositionsDimension(t *testing.T) {
	b := fmtres.NewAssignmentBudget(fmtres.ExprLimits{})

	// Spend up to the cap in two charges, so the counter must be cumulative
	// across calls. Sized from the constant so changing the cap keeps the
	// test meaningful.
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

	// Once tripped, every further call returns the same error without
	// re-evaluating the threshold.
	err2 := b.ChargePositions(1, "irrelevant")
	if err2 == nil || err2.Error() != err.Error() {
		t.Errorf("a spent budget must keep returning the same error; got %v then %v", err, err2)
	}
	if b.Err() == nil {
		t.Error("Err() must report the tripped error once the budget is spent")
	}
}

// TestAssignmentBudget_RetainedBytesDimension is the RetainedBytes
// counterpart, isolating that dimension the same way. Commenting out the
// threshold check inside ChargeRetainedBytes makes it fail and leaves
// TestAssignmentBudget_PositionsDimension passing.
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

	// n <= 0 is a no-op, as for ChargePositions: it must not trip or clear
	// the budget.
	fresh := fmtres.NewAssignmentBudget(fmtres.ExprLimits{})
	if err := fresh.ChargeRetainedBytes(0, "noop"); err != nil {
		t.Errorf("a zero charge must be a no-op: %v", err)
	}
	if err := fresh.ChargeRetainedBytes(-5, "noop"); err != nil {
		t.Errorf("a negative charge must be a no-op: %v", err)
	}
}

// TestAssignmentBudget_FreshPerAssignment pins one budget per assignment,
// not a shared one, the property
// TestCheckTemplateExpressions_TemplateWideBudget_FreshPerCall pins
// server-side. session.Manager.Create allocates a new AssignmentBudget on
// every call (session.go), so two sessions, and therefore two
// [fmtres.NewAssignmentBudget] calls, each get a full allowance.
func TestAssignmentBudget_FreshPerAssignment(t *testing.T) {
	for i := range 2 {
		b := fmtres.NewAssignmentBudget(fmtres.ExprLimits{})
		if err := b.ChargePositions(wantAssignmentMaxPositions-10, fmt.Sprintf("assignment %d", i)); err != nil {
			t.Fatalf("assignment %d: a fresh budget must accept a charge just under the cap: %v", i, err)
		}
	}
}

// TestAssignmentBudget_ConcurrentCharges proves [fmtres.AssignmentBudget] is
// safe under concurrent use. Sessions are not reused across tasks, but a
// session's environment entry/exit and its task-table resolution can charge
// the same object concurrently (see that type's doc comment), and an
// unsynchronized counter would lose updates and fail under -race. N
// goroutines each charging 1 position, below the cap, must all be accepted.
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
	// goroutines (200) is well under the default positions cap, so every
	// charge must be accepted.
	if accepted != goroutines || rejected != 0 {
		t.Fatalf("accepted=%d rejected=%d, want all %d accepted (a lost update under a race would "+
			"under-count positions and hide a real over-cap assignment)", accepted, rejected, goroutines)
	}
}

// ── AssignmentBudget wired through the phase-3 evaluators: across tables ────

// simpleTaskLetMsg returns an AssignMsg whose step-script let: block binds
// one name to a string of n bytes, a minimal table for the cross-table tests
// below.
func simpleTaskLetMsg(name string, n int) *protocol.AssignMsg {
	return &protocol.AssignMsg{
		EXPR:          true,
		StepScriptLet: []string{fmt.Sprintf(`%s = "x" * %d`, name, n)},
	}
}

// TestAssignmentBudget_AcrossTables_RetainedBytes covers the retained-bytes
// dimension: the per-table bound (LetRetainedBytes, 10 MB) bounds one table,
// and the assignment-wide bound sums across the tables one assignment builds.
//
// Three tables (a task table plus two environment tables), each retaining
// ~7,000,064 bytes, under LetRetainedBytes (10,000,000); the third pushes the
// assignment's cumulative total (21,000,192 bytes) over
// AssignmentRetainedBytes (20,000,000), though each table alone passes the
// per-table check.
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

	// Table 3: a second environment's table, which takes the assignment's
	// cumulative total over the limit though it is as compliant as tables 1
	// and 2.
	env3 := &protocol.AssignEnvironment{Name: "Env2", Let: []string{fmt.Sprintf(`c0 = "x" * %d`, bytesPerTable)}}
	syms3, err := fmtres.EnvSymbols(&protocol.AssignMsg{}, env3, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("EnvSymbols (table 3): %v", err)
	}
	err = fmtres.ApplyEnvLet(env3, syms3, nil, budget)
	if err == nil {
		t.Fatalf("table 3, individually compliant (~%d bytes, under the per-table limit's own "+
			"10,000,000-byte per-table ceiling) but pushing the assignment's cumulative total to ~%d "+
			"bytes, must be rejected by the assignment-wide budget", bytesPerTable, 3*(bytesPerTable+64))
	}
	if !strings.Contains(err.Error(), "assignment-wide expression budget exceeded") ||
		!strings.Contains(err.Error(), "retain") {
		t.Errorf("error = %v, want it to name the assignment-wide retained-bytes budget", err)
	}
	if _, ok := syms3.Lookup("c0"); ok {
		t.Error(`"c0" should not be bound: the assignment-wide budget was spent before it`)
	}
}

// TestAssignmentBudget_AcrossActions_Positions is the positions-dimension
// counterpart: many cheap resolutions, across several simulated
// environments' worth of variables, that no per-call limit (ExprLimits'
// OperationLimit/MemoryLimit) would catch. Only the assignment-wide budget
// sums the position count across calls.
//
// The construction is sized from the cap: 12 calls of one twelfth of it plus
// enough overshoot that the last call trips, so no single call comes close to
// the cap on its own.
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
// ResolveActionExpr makes per call: 1 (command) + len(args).
func TestResolveActionExpr_ChargesOnePlusArgsPositions(t *testing.T) {
	action := &protocol.Action{Command: "echo", Args: []string{"a", "b", "c"}}
	budget := fmtres.NewAssignmentBudget(fmtres.ExprLimits{})

	if _, err := fmtres.ResolveActionExpr(action, nil, nil, budget); err != nil {
		t.Fatalf("ResolveActionExpr: %v", err)
	}

	// Spend the budget up to the cap, then confirm the next charge (from a
	// second ResolveActionExpr call) trips it, so the first call charged
	// 1 (command) + 3 (args) = 4.
	const wantFirstCallCharge = 4
	remaining := wantAssignmentMaxPositions - wantFirstCallCharge
	if err := budget.ChargePositions(remaining, "filler"); err != nil {
		t.Fatalf("topping up to one-under-cap must succeed: %v", err)
	}
	if _, err := fmtres.ResolveActionExpr(&protocol.Action{Command: "x"}, nil, nil, budget); err == nil {
		t.Fatalf("a budget sitting at exactly (cap - %d) must be tripped by one more command position "+
			"if the first call's own charge was really %d, not some other number",
			wantFirstCallCharge, wantFirstCallCharge)
	}
}

// TestResolveEmbeddedFilesExpr_ChargesOnePerFile and
// TestResolveVarsExpr_ChargesOnePerVar are the same pin for the other two
// Resolve*Expr entry points, each charging one position per entry with no
// extra charge like ResolveActionExpr's command.
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
// [budgetOrDefault] documents for a nil budget: the default limits, on a
// ledger that is fresh for that one call.
//
// The budget is a required parameter at every phase-3 entry point. This
// package's tests pass nil when the limits are not under test, and many call
// sites here depend on it behaving like an unspent default budget.
//
// budgetguard_test.go's AST guard asserts the other half: that no production
// caller passes nil.
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

	// Fresh, not only default-limited: two successive nil calls each
	// spending the whole default position allowance must both succeed. A nil
	// that resolved to one shared package-level budget would fail the second.
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
