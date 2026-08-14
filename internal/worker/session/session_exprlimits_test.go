// SPDX-License-Identifier: AGPL-3.0-or-later

package session

// EXPR sub-project E4d's Task 2: the operator's configured phase-3 limits must
// travel worker config -> Manager -> Session -> AssignmentBudget -> every
// phase-3 evaluation.
//
// The fmtres tests prove each limit is genuinely enforced; these prove the
// WIRING, which is the half that can silently degrade to "the defaults" with
// every one of those tests still green.

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// tightLimits is deliberately unlike the defaults in EVERY field, so a
// half-wired mapping (four fields threaded, one forgotten) fails rather than
// coincidentally matching.
var tightLimits = fmtres.ExprLimits{
	OperationLimit:          123_456,
	MemoryLimit:             2_345_678,
	AssignmentPositions:     3_456,
	AssignmentRetainedBytes: 4_567_890,
	LetRetainedBytes:        1_234_567,
}

// TestManagerCreate_ExprLimitsReachTheAssignmentBudget pins the wiring point:
// the Manager's configured limits are what the session's budget meters
// against. Without this, cmd/sqi-worker could map the config perfectly and
// Manager.Create could still build a default-limited budget.
func TestManagerCreate_ExprLimitsReachTheAssignmentBudget(t *testing.T) {
	dataDir := t.TempDir()
	mgr := NewManager(
		filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil),
		workerconfig.IsolationConfig{}, tightLimits, nopLogger(),
	)

	s, err := mgr.Create(context.Background(), &protocol.AssignMsg{JobID: "j", EXPR: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := s.ExprBudget().Limits(); got != tightLimits {
		t.Errorf("session budget meters against %+v, want the Manager's configured %+v", got, tightLimits)
	}
}

// TestManagerCreate_ZeroExprLimitsAreTheDefaults pins the other half: a
// Manager constructed with the zero value (every caller that predates this
// field, and every test in this package) behaves exactly as it did before.
func TestManagerCreate_ZeroExprLimitsAreTheDefaults(t *testing.T) {
	dataDir := t.TempDir()
	mgr := NewManager(
		filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil),
		workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, nopLogger(),
	)

	s, err := mgr.Create(context.Background(), &protocol.AssignMsg{JobID: "j", EXPR: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := s.ExprBudget().Limits(); got != fmtres.DefaultExprLimits() {
		t.Errorf("a zero-value ExprLimits meters against %+v, want the defaults %+v",
			got, fmtres.DefaultExprLimits())
	}
}

// TestManagerCreate_ConfiguredLimitRejectsEnvironmentEntry is the behavioral
// counterpart of the two structural tests above: a configured per-table
// retention limit must actually change the verdict of a real environment
// entry through Manager.Create. One environment binds ~2 MB, which the default
// (10 MB) accepts and a configured 1 MB does not.
func TestManagerCreate_ConfiguredLimitRejectsEnvironmentEntry(t *testing.T) {
	msg := func() *protocol.AssignMsg {
		return &protocol.AssignMsg{
			JobID: "j",
			EXPR:  true,
			Environments: []protocol.AssignEnvironment{
				{Name: "env", Let: []string{`a = "x" * 2000000`}},
			},
		}
	}

	def := NewManager(
		filepath.Join(t.TempDir(), "sessions"), false, isolation.NewFake(nil),
		workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, nopLogger(),
	)
	if _, err := def.Create(context.Background(), msg()); err != nil {
		t.Fatalf("the defaults must accept a ~2 MB let binding: %v", err)
	}

	tight := NewManager(
		filepath.Join(t.TempDir(), "sessions"), false, isolation.NewFake(nil),
		workerconfig.IsolationConfig{}, fmtres.ExprLimits{LetRetainedBytes: 1_000_000}, nopLogger(),
	)
	_, err := tight.Create(context.Background(), msg())
	if err == nil {
		t.Fatal("a configured 1 MB per-table retention limit must reject a ~2 MB let binding")
	}
	if !strings.Contains(err.Error(), "1000000") {
		t.Errorf("error = %v, want it to name the CONFIGURED limit", err)
	}
}

// TestResolveEnvAction_TeardownUsesTheConfiguredLimits pins the one path that
// is deliberately exempt from the assignment-wide LEDGER: environment
// teardown. It must still be metered by the operator's LIMITS. Passing no
// budget at all -- which is what the code did before E4d Task 2, and the
// obvious way to preserve the ledger exemption -- would silently meter
// teardown against the built-in defaults on a host configured otherwise.
//
// The construction is chosen so the two are DISTINGUISHABLE, which required
// configuring the limit LOOSER than the default rather than tighter: three
// 4 MB bindings retain ~12,000,192 bytes in one table, which the configured
// 15 MB per-table limit accepts and the built-in 10 MB default does not. Entry
// therefore succeeds, and teardown succeeds only if it is metered by the same
// configured number.
//
// It also pins the FRESH half of "fresh ledger, same limits": entry already
// charged ~12 MB against the 20 MB assignment-wide default, so a teardown that
// re-used the entry ledger would trip at ~24 MB -- the exact regression fix
// round 1 of E4c fixed, which must survive Task 2's change.
func TestResolveEnvAction_TeardownUsesTheConfiguredLimits(t *testing.T) {
	dataDir := t.TempDir()
	mgr := NewManager(
		filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil),
		workerconfig.IsolationConfig{}, fmtres.ExprLimits{LetRetainedBytes: 15_000_000}, nopLogger(),
	)

	env := protocol.AssignEnvironment{
		Name: "env",
		Let: []string{
			`a = "x" * 4000000`,
			`b = "y" * 4000000`,
			`c = "z" * 4000000`,
		},
	}
	msg := &protocol.AssignMsg{JobID: "j", EXPR: true, Environments: []protocol.AssignEnvironment{env}}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: ~12 MB of bindings is under the configured 15 MB per-table limit: %v", err)
	}

	if _, _, err := s.resolveEnvAction(env, nil, nil); err != nil {
		t.Fatalf("teardown must re-resolve the same block under the SAME configured limits "+
			"(and a fresh ledger): %v", err)
	}
}

// TestResolveEnvAction_TeardownBudgetIsFreshPerEvaluation is the reviewer's
// construction from E4d Task 2's fix round 1, kept as a regression test.
//
// THE DEFECT: Task 2's first round built ONE budget in resolveEnvAction and
// passed it to all four of its evaluations (EnvSymbols, ResolveVarsExpr,
// ApplyEnvLet, ResolveActionExpr), so their POSITION charges accumulated
// against a single 10,000 cap. Before Task 2, each call got its own throwaway
// ledger and nothing accumulated.
//
// THE CONSEQUENCE, and why this is not merely tidiness: an environment with
// 9,000 variables and a 1,500-arg onExit ENTERS successfully (9,001 positions
// against the assignment-wide ledger) and then fails at TEARDOWN --
// 9,000 variables + 1 command + 1,500 args = 10,501 against one shared cap.
// [Session.ExitEnvironments] treats a resolve failure as a WARNING and
// continues, so the onExit is silently SKIPPED: the license check-in, daemon
// shutdown or unmount never runs. That is exactly the leak E4c's fix round 1
// Critical 2 exists to prevent, re-created one level down.
//
// It is reachable at the shipped defaults, and more easily once an operator
// raises the server's openjd.expr_template_positions (Task 1 made values up to
// 100,000 legal), because the template budget is not gated by EnforceLimits.
//
// Mutation: replace the four s.teardownBudget() calls with one shared budget
// and this test fails, naming the assignment-wide budget.
func TestResolveEnvAction_TeardownBudgetIsFreshPerEvaluation(t *testing.T) {
	vars := make(map[string]string, 9_000)
	for i := range 9_000 {
		vars["V"+strconv.Itoa(i)] = "x"
	}
	args := make([]string, 1_500)
	for i := range args {
		args[i] = "a"
	}

	env := protocol.AssignEnvironment{
		Name:      "env",
		Variables: vars,
		OnEnter:   &protocol.Action{Command: "true"},
	}
	msg := &protocol.AssignMsg{JobID: "j", EXPR: true, Environments: []protocol.AssignEnvironment{env}}

	mgr := NewManager(
		filepath.Join(t.TempDir(), "sessions"), false, isolation.NewFake(nil),
		workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, nopLogger(),
	)
	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: 9,001 positions is under the 10,000 assignment-wide default: %v", err)
	}

	onExit := &protocol.Action{Command: "true", Args: args}
	if _, _, err := s.resolveEnvAction(env, onExit, vars); err != nil {
		t.Fatalf("teardown must resolve 9,000 variables and a 1,500-arg onExit -- each evaluation "+
			"gets its OWN ledger, so nothing accumulates across the four. A shared budget makes "+
			"ExitEnvironments skip this onExit with only a log line. Got: %v", err)
	}
}
