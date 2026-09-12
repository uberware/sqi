// SPDX-License-Identifier: AGPL-3.0-or-later

package executor_test

import (
	"log/slog"
	"path/filepath"
	"testing"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/executor"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/session"
)

// TestResolveAssignment_ExportedSeamResolvesTaskParams proves the exported
// resolver reaches the real base-spec resolution path: a {{Task.Param.*}}
// reference in the command args comes back substituted.
func TestResolveAssignment_ExportedSeamResolvesTaskParams(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.DiscardHandler)
	mgr := session.NewManager(
		filepath.Join(tmpDir, "sessions"), false,
		isolation.NewFake(nil), workerconfig.IsolationConfig{},
		fmtres.ExprLimits{}, logger,
	)

	msg := &protocol.AssignMsg{
		Version:    protocol.ProtocolVersion,
		Type:       protocol.TypeAssign,
		TaskID:     "task-1",
		JobID:      "job-1",
		StepID:     "step-1",
		AttemptID:  "attempt-1",
		Parameters: map[string]string{"Frame": "7"},
		OnRun: &protocol.Action{
			Command: "renderer",
			Args:    []string{"-f", "{{Task.Param.Frame}}"},
		},
	}

	sess, err := mgr.Create(t.Context(), msg)
	if err != nil {
		t.Fatalf("session Create: %v", err)
	}
	t.Cleanup(func() { mgr.Cleanup(t.Context(), sess, false) })

	action, _, _, err := executor.ResolveAssignment(msg, sess)
	if err != nil {
		t.Fatalf("ResolveAssignment: %v", err)
	}
	if action.Command != "renderer" {
		t.Errorf("command = %q, want %q", action.Command, "renderer")
	}
	if len(action.Args) != 2 || action.Args[1] != "7" {
		t.Errorf("args = %v, want [-f 7]", action.Args)
	}
}
