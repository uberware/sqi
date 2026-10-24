// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/uberware/sqi/internal/worker/commands"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// mappedName is a command name no host has. A job that runs it can only start
// a process through worker.commands.
const mappedName = "sqi-test-mapped-command"

// mappedTable maps mappedName to a real command that starts a process and exits
// 0 on any host (see noopAction).
func mappedTable() commands.Table {
	na := noopAction()
	return commands.Table{mappedName: {Path: na.Command, Args: na.Args}}
}

func newMappedManager(t *testing.T, table commands.Table) *Manager {
	t.Helper()
	return NewManager(
		filepath.Join(t.TempDir(), "sessions"), false, isolation.NewFake(nil),
		workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, nopLogger(),
		WithCommands(commands.NewResolver(table)),
	)
}

func envRunning(action *protocol.Action, onExit bool) protocol.AssignEnvironment {
	env := protocol.AssignEnvironment{Name: "env"}
	if onExit {
		env.OnExit = action
	} else {
		env.OnEnter = action
	}
	return env
}

func TestOnEnter_RunsAMappedCommand(t *testing.T) {
	mgr := newMappedManager(t, mappedTable())
	msg := &protocol.AssignMsg{
		JobID:        "job-1",
		Environments: []protocol.AssignEnvironment{envRunning(&protocol.Action{Command: mappedName}, false)},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create with a mapped onEnter command: %v", err)
	}
	mgr.Cleanup(context.Background(), s, false)
}

// The same assignment on a worker with no mapping must fail: that is what makes
// the test above prove the mapping is what made it start.
func TestOnEnter_AnUnmappedUnknownCommandFails(t *testing.T) {
	mgr := newMappedManager(t, nil)
	msg := &protocol.AssignMsg{
		JobID:        "job-1",
		Environments: []protocol.AssignEnvironment{envRunning(&protocol.Action{Command: mappedName}, false)},
	}

	if _, err := mgr.Create(context.Background(), msg); err == nil {
		t.Fatal("Create succeeded with a command that exists nowhere and is not mapped")
	}
}

func TestOnExit_RunsAMappedCommand(t *testing.T) {
	mgr := newMappedManager(t, mappedTable())
	msg := &protocol.AssignMsg{
		JobID: "job-1",
		Environments: []protocol.AssignEnvironment{
			envRunning(&protocol.Action{Command: mappedName}, true),
		},
	}
	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.ExitEnvironments(context.Background(), nopLogger()); err != nil {
		t.Errorf("ExitEnvironments with a mapped onExit command: %v", err)
	}
	mgr.Cleanup(context.Background(), s, false)
}

func TestOnExit_AnUnmappedUnknownCommandFails(t *testing.T) {
	mgr := newMappedManager(t, nil)
	msg := &protocol.AssignMsg{
		JobID: "job-1",
		Environments: []protocol.AssignEnvironment{
			envRunning(&protocol.Action{Command: mappedName}, true),
		},
	}
	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.ExitEnvironments(context.Background(), nopLogger()); err == nil {
		t.Error("ExitEnvironments succeeded with a command that exists nowhere and is not mapped")
	}
	mgr.Cleanup(context.Background(), s, false)
}

func TestResolveAction_ReturnsACopyAndNeverMutatesTheAssignment(t *testing.T) {
	mgr := newMappedManager(t, commands.Table{"nuke": {Path: "/opt/nuke", Args: []string{"--nc"}}})
	s, err := mgr.Create(context.Background(), &protocol.AssignMsg{JobID: "job-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer mgr.Cleanup(context.Background(), s, false)
	orig := &protocol.Action{Command: "nuke", Args: []string{"-x"}, TimeoutSeconds: 30}

	got := s.ResolveAction(orig)

	if got.Command != "/opt/nuke" || !slices.Equal(got.Args, []string{"--nc", "-x"}) {
		t.Errorf("ResolveAction = %q %q, want /opt/nuke [--nc -x]", got.Command, got.Args)
	}
	if got.TimeoutSeconds != 30 {
		t.Errorf("TimeoutSeconds = %d, want it carried over", got.TimeoutSeconds)
	}
	if orig.Command != "nuke" || !slices.Equal(orig.Args, []string{"-x"}) {
		t.Errorf("the assignment's own action was mutated: %+v", orig)
	}
}

func TestResolveAction_PassesNilAndUnmappedThrough(t *testing.T) {
	mgr := newMappedManager(t, commands.Table{"nuke": {Path: "/opt/nuke"}})
	s, err := mgr.Create(context.Background(), &protocol.AssignMsg{JobID: "job-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer mgr.Cleanup(context.Background(), s, false)

	if got := s.ResolveAction(nil); got != nil {
		t.Errorf("ResolveAction(nil) = %+v, want nil", got)
	}
	other := &protocol.Action{Command: "blender"}
	if got := s.ResolveAction(other); got != other {
		t.Errorf("ResolveAction of an unmapped command = %+v, want the same action back", got)
	}
}

func TestNoCommandsOption_ResolvesNothing(t *testing.T) {
	mgr := NewManager(filepath.Join(t.TempDir(), "sessions"), false, isolation.NewFake(nil),
		workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, nopLogger())
	s, err := mgr.Create(context.Background(), &protocol.AssignMsg{JobID: "job-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer mgr.Cleanup(context.Background(), s, false)

	a := &protocol.Action{Command: "nuke"}
	if got := s.ResolveAction(a); got != a {
		t.Errorf("a manager built without WithCommands changed an action: %+v", got)
	}
}
