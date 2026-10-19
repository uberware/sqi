// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/uberware/sqi/internal/worker/commands"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/session"
)

// TestCommandsOption_ReachesTheSessions is the wiring test for worker.commands:
// the option commandsOption returns, handed to session.NewManager the way
// start.go does, must make the sessions it creates resolve the configured name.
func TestCommandsOption_ReachesTheSessions(t *testing.T) {
	table := commands.Table{"nuke": {Path: "/opt/nuke", Args: []string{"--nc"}}}
	mgr := session.NewManager(
		filepath.Join(t.TempDir(), "sessions"), false, isolation.NewFake(nil),
		workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, slog.New(slog.DiscardHandler),
		commandsOption(table, slog.New(slog.DiscardHandler)),
	)
	s, err := mgr.Create(context.Background(), &protocol.AssignMsg{JobID: "job-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer mgr.Cleanup(context.Background(), s, false)

	got := s.ResolveAction(&protocol.Action{Command: "nuke", Args: []string{"-x"}})

	if got.Command != "/opt/nuke" || !slices.Equal(got.Args, []string{"--nc", "-x"}) {
		t.Errorf("ResolveAction = %q %q, want /opt/nuke [--nc -x]", got.Command, got.Args)
	}
}

func TestUnresolvableCommands_NamesEntriesWhoseTargetIsMissing(t *testing.T) {
	present := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(present, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "gone")
	table := commands.Table{
		"present":  {Path: present},
		"missing":  {Path: missing},
		"not-here": {Path: "sqi-test-no-such-command-anywhere"}, // a bare name PATH cannot resolve
	}

	got := unresolvableCommands(table)

	if want := []string{"missing", "not-here"}; !slices.Equal(got, want) {
		t.Errorf("unresolvableCommands = %v, want %v", got, want)
	}
}

func TestUnresolvableCommands_EmptyTableHasNone(t *testing.T) {
	if got := unresolvableCommands(nil); len(got) != 0 {
		t.Errorf("unresolvableCommands(nil) = %v, want none", got)
	}
}
