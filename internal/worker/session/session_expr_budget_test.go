// SPDX-License-Identifier: AGPL-3.0-or-later

package session

// The worker's assignment-wide budget
// (internal/worker/fmtres.AssignmentBudget), wired end to end through
// Manager.Create -> enterEnvironments -> enterOne -> resolveEnvEntry ->
// fmtres.ApplyEnvLet(..., s.exprBudget). The fmtres-package tests
// (assignmentbudget_test.go) prove the budget type itself sums correctly
// across several tables built directly; this file proves the wiring: a
// Manager.Create call entering several environments in one session shares
// one budget across all of them, not one budget per environment.

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
// entered in order, each with a let: block that binds one name to a string
// of bytesPerEnv bytes, each well under fmtres's per-table LetRetainedBytes
// limit (10,000,000 bytes) at the sizes this file's tests use, so any
// rejection comes from the assignment-wide budget summing across
// environments, not the per-table one.
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
// is the end-to-end proof: 3 environments, each retaining ~7,000,064 bytes
// (well under fmtres's 10,000,000-byte per-table limit, which alone would
// accept every one of them), whose cumulative total (~21,000,192 bytes)
// exceeds the assignment-wide budget (20,000,000 bytes) on the third
// environment. This proves Manager.Create shares one fmtres.AssignmentBudget
// across every environment it enters; a budget per environment would accept
// all three.
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
// the wiring-level FreshPerCall proof: two independent sessions, each
// entering environments that alone fit under the assignment-wide budget
// (well under both dimensions), must each succeed, proving Manager.Create
// allocates a new fmtres.AssignmentBudget every call rather than a
// Manager-level (or process-wide) one a second session would inherit
// partly spent.
func TestManagerCreate_EnterEnvironment_EXPR_AssignmentBudgetFreshPerSession(t *testing.T) {
	dataDir := t.TempDir()
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, nopLogger())

	// Each session's own environments retain ~14,000,128 bytes (2 x
	// 7,000,064), under the 20,000,000-byte assignment-wide cap alone, but
	// two sessions' worth summed (~28,000,256) would exceed it if they shared
	// one budget.
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

// TestSession_ExitEnvironments_EXPR_AllOnExitRunAfterEntryNearsBudgetCap:
// teardown must not skip onExit actions because entry-time let bindings
// brought the assignment-wide budget close to its cap.
//
// Two environments each let-bind a 7 MB string at entry; Manager.Create
// succeeds at ~14,000,128 bytes, under the 20,000,000-byte assignment-wide
// cap. If resolveEnvAction re-evaluated the two let: blocks at exit against
// the same shared budget, the cumulative total would reach ~21,000,192 bytes,
// over the cap, so ExitEnvironments' second (later-entered) environment would
// fail to resolve, log a warning and skip its onExit, with only the first
// environment's onExit having run. Both onExit actions must run: each writes
// its own witness file, and this test asserts both exist and
// ExitEnvironments returns no error.
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
