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
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// bigEnvLetMsg returns an AssignMsg declaring n job environments, each
// entered in order, each with a let: block that binds ONE name to a string
// of bytesPerEnv bytes -- individually well under
// fmtres's own per-table workerLetRetainedLimit (10,000,000 bytes) at the
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
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())

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
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())

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
