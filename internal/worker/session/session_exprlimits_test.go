// SPDX-License-Identifier: AGPL-3.0-or-later

package session

// The operator's configured phase-3 limits must
// travel worker config -> Manager -> Session -> AssignmentBudget -> every
// phase-3 evaluation.
//
// The fmtres tests prove each limit is enforced; these prove the wiring,
// which can fall back to the defaults with every one of those tests still
// passing.

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

// tightLimits differs from the defaults in every field, so a half-wired
// mapping (four fields threaded, one forgotten) fails rather than matching by
// coincidence.
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
// Manager constructed with the zero value (as most tests in this package do)
// meters by the defaults.
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
// retention limit must change the verdict of a real environment
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
		t.Errorf("error = %v, want it to name the configured limit", err)
	}
}

// TestResolveEnvAction_TeardownUsesTheConfiguredLimits pins the one path
// exempt from the assignment-wide ledger: environment teardown. It is still
// metered by the operator's limits. Passing no budget at all would meter
// teardown against the built-in defaults on a host configured otherwise.
//
// To tell the two apart the limit is configured looser than the default:
// three 4 MB bindings retain ~12,000,192 bytes in one table, which the
// configured 15 MB per-table limit accepts and the built-in 10 MB default
// does not. Entry succeeds, and teardown succeeds only if it is metered by the
// same configured number.
//
// It also pins the fresh half of "fresh ledger, same limits": entry already
// charged ~12 MB against the 20 MB assignment-wide default, so a teardown that
// reused the entry ledger would trip at ~24 MB.
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

// TestResolveEnvAction_TeardownBudgetIsFreshPerEvaluation pins that each of
// resolveEnvAction's four evaluations (EnvSymbols, ResolveVarsExpr,
// ApplyEnvLet, ResolveActionExpr) gets its own budget. With one budget passed
// to all four, their position charges would accumulate against a single
// 10,000 cap.
//
// An environment with 9,000 variables and a 1,500-arg onExit then enters
// successfully (9,001 positions against the assignment-wide ledger) and fails
// at teardown: 9,000 variables + 1 command + 1,500 args = 10,501 against one
// shared cap. [Session.ExitEnvironments] treats a resolve failure as a
// warning and continues, so the onExit is skipped and the license check-in,
// daemon shutdown or unmount never runs, the leak the teardown exemption from
// the assignment-wide ledger exists to prevent.
//
// This is reachable at the default limits, and more easily when an operator
// raises the server's openjd.expr_template_positions (values up to 100,000
// are legal), because the template budget is not gated by EnforceLimits.
//
// Passing one shared budget to resolveEnvParts instead of the
// s.teardownBudget method value makes this test fail, naming the
// assignment-wide budget.
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
		// Portable: Create executes this action, and the budget
		// property under test does not care which process runs. See noopAction.
		OnEnter: noopAction(),
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

	// onExit is only resolved, never started, so its command need not exist;
	// it is kept portable so the file carries no POSIX-only name to copy into
	// a test that does start one. The 1,500 args are what matters here.
	onExit := &protocol.Action{Command: noopAction().Command, Args: args}
	if _, _, err := s.resolveEnvAction(env, onExit, vars); err != nil {
		t.Fatalf("teardown must resolve 9,000 variables and a 1,500-arg onExit -- each evaluation "+
			"gets its OWN ledger, so nothing accumulates across the four. A shared budget makes "+
			"ExitEnvironments skip this onExit with only a log line. Got: %v", err)
	}
}
