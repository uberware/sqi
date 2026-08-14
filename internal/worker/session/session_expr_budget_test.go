// SPDX-License-Identifier: AGPL-3.0-or-later

package session

// EXPR sub-project E4c's Task 4: the worker's assignment-wide budget
// (internal/worker/fmtres.AssignmentBudget), wired end to end through
// Manager.Create -> enterEnvironments -> enterOne -> resolveEnvEntry ->
// fmtres.ApplyEnvLet(..., s.exprBudget). The fmtres-package tests
// (assignmentbudget_test.go) prove the budget type itself sums correctly
// across several tables built directly; this file proves the WIRING is
// live -- that a real Manager.Create call, entering several real
// environments in one real session, shares ONE budget across all of them,
// not one budget per environment.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// bigEnvLetMsg returns an AssignMsg declaring n job environments, each
// entered in order, each with a let: block that binds ONE name to a string
// of bytesPerEnv bytes -- individually well under
// fmtres's own per-table LetRetainedBytes limit (10,000,000 bytes) at the
// sizes this file's tests use, so any rejection is attributable to the
// ASSIGNMENT-wide budget summing across environments, not the pre-existing
// per-table one.
func bigEnvLetMsg(n, bytesPerEnv int) *protocol.AssignMsg {
	envs := make([]protocol.AssignEnvironment, n)
	for i := range envs {
		envs[i] = protocol.AssignEnvironment{
			Name: "env",
			Let:  []string{`a = "x" * ` + strconv.Itoa(bytesPerEnv)},
		}
	}
	return &protocol.AssignMsg{
		JobID:        "j",
		EXPR:         true,
		Environments: envs,
	}
}

// TestManagerCreate_EnterEnvironment_EXPR_AssignmentBudgetSharedAcrossEnvironments
// is this task's central end-to-end proof: 3 environments, each retaining
// ~7,000,064 bytes (well under fmtres's own 10,000,000-byte PER-TABLE limit,
// so that limit alone would accept every one of them), whose CUMULATIVE
// total (~21,000,192 bytes) exceeds the assignment-wide budget
// (20,000,000 bytes) on the third environment -- proving Manager.Create
// shares ONE fmtres.AssignmentBudget across every environment it enters, not
// a fresh one per environment (a fresh-per-environment budget would accept
// all three, since each is individually compliant).
func TestManagerCreate_EnterEnvironment_EXPR_AssignmentBudgetSharedAcrossEnvironments(t *testing.T) {
	dataDir := t.TempDir()
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, nopLogger())

	msg := bigEnvLetMsg(3, 7_000_000)

	s, err := mgr.Create(context.Background(), msg)
	if err == nil {
		t.Fatalf("Create: want an assignment-wide budget error (3 environments x ~7,000,064 bytes each "+
			"= ~21,000,192 bytes, over the 20,000,000-byte assignment-wide cap, even though each "+
			"environment is individually under the 10,000,000-byte per-table cap); got success (session %+v)", s)
	}
	if !strings.Contains(err.Error(), "assignment-wide expression budget exceeded") {
		t.Errorf("error = %v, want it to name the assignment-wide budget, not merely a per-table one", err)
	}
}

// TestManagerCreate_EnterEnvironment_EXPR_AssignmentBudgetFreshPerSession is
// the wiring-level FreshPerCall proof: TWO independent sessions, each
// entering environments that would ALONE fit comfortably under the
// assignment-wide budget (well under both dimensions), must EACH succeed --
// proving Manager.Create allocates a NEW fmtres.AssignmentBudget every call,
// not a Manager-level (or process-wide) one a second session would inherit
// an already-partly-spent allowance from.
func TestManagerCreate_EnterEnvironment_EXPR_AssignmentBudgetFreshPerSession(t *testing.T) {
	dataDir := t.TempDir()
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, nopLogger())

	// Each session's own environments retain ~14,000,128 bytes (2 x
	// 7,000,064) -- comfortably under the 20,000,000-byte assignment-wide
	// cap ALONE, but two sessions' worth summed (~28,000,256) would exceed it
	// if they shared one budget.
	for i := range 2 {
		msg := bigEnvLetMsg(2, 7_000_000)
		msg.JobID = "j"
		s, err := mgr.Create(context.Background(), msg)
		if err != nil {
			t.Fatalf("session %d: Create: %v -- a budget that leaked across sessions would make the "+
				"second session stricter than the first", i, err)
		}
		if s.ExprBudget() == nil {
			t.Fatalf("session %d: ExprBudget() must never be nil for a session that exists", i)
		}
	}
}

// TestSession_ExitEnvironments_EXPR_AllOnExitRunAfterEntryNearsBudgetCap is
// fix round 1's regression test for Critical 2: teardown must not silently
// skip onExit actions merely because entry-time let bindings pushed the
// assignment-wide budget close to (or, before the fix, at) its cap.
//
// Two environments each let-bind a 7 MB string at ENTRY -- Manager.Create
// succeeds at ~14,000,128 bytes, comfortably under the 20,000,000-byte
// assignment-wide cap (the exact construction the coordinator's review
// verified). BEFORE this fix, resolveEnvAction re-evaluated the SAME two
// let: blocks a SECOND time at EXIT against the SAME shared budget, pushing
// the cumulative total to ~21,000,192 bytes -- over the cap -- so
// ExitEnvironments' second (later-entered) environment failed to resolve,
// logged a warning, and skipped its onExit entirely, with only the FIRST
// environment's onExit having already run via the earlier iteration. Both
// onExit actions must run now: each writes its own witness file, and this
// test asserts both exist and ExitEnvironments returns no error.
func TestSession_ExitEnvironments_EXPR_AllOnExitRunAfterEntryNearsBudgetCap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	witnessDir := t.TempDir()
	dataDir := t.TempDir()
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, nopLogger())

	witness := func(name string) string { return filepath.Join(witnessDir, name) }

	msg := &protocol.AssignMsg{
		JobID: "j",
		EXPR:  true,
		Environments: []protocol.AssignEnvironment{
			{
				Name: "env-a",
				Let:  []string{`a = "x" * 7000000`},
				OnExit: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "touch " + witness("env-a-exited")},
				},
			},
			{
				Name: "env-b",
				Let:  []string{`b = "x" * 7000000`},
				OnExit: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "touch " + witness("env-b-exited")},
				},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v -- two 7 MB entry-time let bindings (~14 MB total) must fit under the "+
			"20 MB assignment-wide cap", err)
	}

	if err := s.ExitEnvironments(context.Background(), nopLogger()); err != nil {
		t.Fatalf("ExitEnvironments: %v -- teardown must not fail merely because entry-time let "+
			"bindings were close to the assignment-wide budget", err)
	}

	for _, name := range []string{"env-a-exited", "env-b-exited"} {
		if _, err := os.Stat(witness(name)); err != nil {
			t.Errorf("witness file %q does not exist: %v -- this environment's onExit did not run", name, err)
		}
	}
}
