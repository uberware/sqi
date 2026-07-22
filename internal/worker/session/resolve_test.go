// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// TestEnterEnvironment_ResolvesParamInOnEnter verifies that {{Param.X}} in an
// environment onEnter action is resolved with the environment-action scope.
func TestEnterEnvironment_ResolvesParamInOnEnter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID:         "j",
		JobParameters: map[string]string{"Greeting": "hello"},
		Environments: []protocol.AssignEnvironment{
			{
				Name: "env-a",
				OnEnter: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "echo {{Param.Greeting}} > out.txt"},
				},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(s.WorkDir, "out.txt"))
	if err != nil {
		t.Fatalf("read out.txt: %v", err)
	}
	if strings.TrimSpace(string(got)) != "hello" {
		t.Errorf("onEnter output = %q; want %q", strings.TrimSpace(string(got)), "hello")
	}
}

// TestEnterEnvironment_ResolvesVariableValue verifies that an environment
// variable VALUE containing {{Param.X}} is resolved before it is placed into
// the onEnter action's process environment.
func TestEnterEnvironment_ResolvesVariableValue(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID:         "j",
		JobParameters: map[string]string{"Greeting": "hello"},
		Environments: []protocol.AssignEnvironment{
			{
				Name:      "env-a",
				Variables: map[string]string{"MY": "{{Param.Greeting}}-world"},
				OnEnter: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "echo $MY > out.txt"},
				},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(s.WorkDir, "out.txt"))
	if err != nil {
		t.Fatalf("read out.txt: %v", err)
	}
	if strings.TrimSpace(string(got)) != "hello-world" {
		t.Errorf("resolved variable value = %q; want %q", strings.TrimSpace(string(got)), "hello-world")
	}
}

// TestEnterEnvironment_ResolvesSessionWorkingDirectory verifies that
// {{Session.WorkingDirectory}} resolves to the session work dir in env actions.
func TestEnterEnvironment_ResolvesSessionWorkingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID: "j",
		Environments: []protocol.AssignEnvironment{
			{
				Name: "env-a",
				OnEnter: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "echo {{Session.WorkingDirectory}} > wd.txt"},
				},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(s.WorkDir, "wd.txt"))
	if err != nil {
		t.Fatalf("read wd.txt: %v", err)
	}
	if strings.TrimSpace(string(got)) != s.WorkDir {
		t.Errorf("resolved Session.WorkingDirectory = %q; want %q", strings.TrimSpace(string(got)), s.WorkDir)
	}
}

// TestEnterEnvironment_TaskParamInOnEnterFails verifies that an onEnter action
// referencing Task.Param.* — not available in the environment scope — fails the
// environment (and therefore session creation) with a clear, naming error.
func TestEnterEnvironment_TaskParamInOnEnterFails(t *testing.T) {
	dataDir := t.TempDir()
	mgr := NewManager(dataDir, false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID: "j",
		Environments: []protocol.AssignEnvironment{
			{
				Name: "bad-env",
				OnEnter: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "echo {{Task.Param.Frame}}"},
				},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err == nil {
		t.Fatal("Create: want error for Task.Param reference in environment; got nil")
	}
	if s != nil {
		t.Errorf("Create must return nil session on error; got %+v", s)
	}
	if !strings.Contains(err.Error(), "Task.Param.Frame") {
		t.Errorf("error = %q; want it to name the unresolved variable %q", err.Error(), "Task.Param.Frame")
	}
}

// TestEnterEnvironment_TaskParamInVariableFails verifies that an environment
// variable VALUE referencing Task.Param.* fails the environment cleanly.
func TestEnterEnvironment_TaskParamInVariableFails(t *testing.T) {
	dataDir := t.TempDir()
	mgr := NewManager(dataDir, false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID:      "j",
		Parameters: map[string]string{"Frame": "42"},
		Environments: []protocol.AssignEnvironment{
			{
				Name:      "bad-env",
				Variables: map[string]string{"BAD": "{{Task.Param.Frame}}"},
				OnEnter: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "echo hi"},
				},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err == nil {
		t.Fatal("Create: want error for Task.Param reference in env variable; got nil")
	}
	if s != nil {
		t.Errorf("Create must return nil session on error; got %+v", s)
	}
	if !strings.Contains(err.Error(), "Task.Param.Frame") {
		t.Errorf("error = %q; want it to name the unresolved variable %q", err.Error(), "Task.Param.Frame")
	}
}

// TestEnterEnvironment_ResolvesEmbeddedFileData verifies that {{Param.X}} inside
// an environment embedded file's data is resolved (against the environment
// scope) before the file is materialized.
func TestEnterEnvironment_ResolvesEmbeddedFileData(t *testing.T) {
	dataDir := t.TempDir()
	mgr := NewManager(dataDir, false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID:         "j",
		JobParameters: map[string]string{"Greeting": "hello"},
		Environments: []protocol.AssignEnvironment{
			{
				Name: "env-a",
				EmbeddedFiles: []protocol.EmbeddedFile{
					{Name: "conf", Filename: "conf.txt", Data: "value={{Param.Greeting}}"},
				},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(s.WorkDir, "conf.txt"))
	if err != nil {
		t.Fatalf("read conf.txt: %v", err)
	}
	if string(got) != "value=hello" {
		t.Errorf("embedded file data = %q; want %q", string(got), "value=hello")
	}
}

// TestEnterEnvironment_EnvFileVarInOnEnter verifies that {{Env.File.<name>}} in
// an onEnter command resolves to the materialized path of that environment's
// embedded file.
func TestEnterEnvironment_EnvFileVarInOnEnter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID: "j",
		Environments: []protocol.AssignEnvironment{
			{
				Name: "env-a",
				EmbeddedFiles: []protocol.EmbeddedFile{
					{Name: "script", Filename: "marker.txt", Data: "env-file-marker"},
				},
				OnEnter: &protocol.Action{
					Command: "sh",
					// cat the embedded file via its Env.File path into out.txt.
					Args: []string{"-c", "cat {{Env.File.script}} > out.txt"},
				},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(s.WorkDir, "out.txt"))
	if err != nil {
		t.Fatalf("read out.txt: %v", err)
	}
	if strings.TrimSpace(string(got)) != "env-file-marker" {
		t.Errorf("onEnter via Env.File path output = %q; want %q", strings.TrimSpace(string(got)), "env-file-marker")
	}
}

// TestEnterEnvironment_TaskParamInEmbeddedFileDataFails verifies that a
// {{Task.Param.*}} reference inside an environment embedded file's data — not in
// the environment scope — fails the environment (and session creation) cleanly.
func TestEnterEnvironment_TaskParamInEmbeddedFileDataFails(t *testing.T) {
	dataDir := t.TempDir()
	mgr := NewManager(dataDir, false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID: "j",
		Environments: []protocol.AssignEnvironment{
			{
				Name: "bad-env",
				EmbeddedFiles: []protocol.EmbeddedFile{
					{Name: "conf", Filename: "conf.txt", Data: "{{Task.Param.Frame}}"},
				},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err == nil {
		t.Fatal("Create: want error for Task.Param reference in env file data; got nil")
	}
	if s != nil {
		t.Errorf("Create must return nil session on error; got %+v", s)
	}
	if !strings.Contains(err.Error(), "Task.Param.Frame") {
		t.Errorf("error = %q; want it to name the unresolved variable %q", err.Error(), "Task.Param.Frame")
	}
}

// TestExitEnvironment_EnvFileVarInOnExit verifies that {{Env.File.<name>}} in an
// onExit action resolves to the materialized path of the environment's embedded
// file (written at session creation during enterOne) when ExitEnvironments runs.
func TestExitEnvironment_EnvFileVarInOnExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID: "j",
		Environments: []protocol.AssignEnvironment{
			{
				Name: "env-a",
				EmbeddedFiles: []protocol.EmbeddedFile{
					{Name: "script", Filename: "marker.txt", Data: "env-file-exit-marker"},
				},
				OnExit: &protocol.Action{
					Command: "sh",
					// cat the embedded file (written at Enter time) via its
					// Env.File path into exit_out.txt.
					Args: []string{"-c", "cat {{Env.File.script}} > exit_out.txt"},
				},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.ExitEnvironments(context.Background(), nopLogger()); err != nil {
		t.Fatalf("ExitEnvironments: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(s.WorkDir, "exit_out.txt"))
	if err != nil {
		t.Fatalf("read exit_out.txt: %v", err)
	}
	if strings.TrimSpace(string(got)) != "env-file-exit-marker" {
		t.Errorf("onExit via Env.File path output = %q; want %q", strings.TrimSpace(string(got)), "env-file-exit-marker")
	}
}

// TestExitEnvironment_ResolvesParamInOnExit verifies that {{Param.X}} in an
// environment onExit action is resolved with the environment-action scope.
func TestExitEnvironment_ResolvesParamInOnExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID:         "j",
		JobParameters: map[string]string{"Tag": "teardown-ok"},
		Environments: []protocol.AssignEnvironment{
			{
				Name: "env-exit",
				OnExit: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "echo {{Param.Tag}} > exit_out.txt"},
				},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.ExitEnvironments(context.Background(), nopLogger()); err != nil {
		t.Fatalf("ExitEnvironments: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(s.WorkDir, "exit_out.txt"))
	if err != nil {
		t.Fatalf("read exit_out.txt: %v", err)
	}
	if strings.TrimSpace(string(got)) != "teardown-ok" {
		t.Errorf("onExit output = %q; want %q", strings.TrimSpace(string(got)), "teardown-ok")
	}
}

// TestExitEnvironment_TaskParamInOnExitContinuesTeardown verifies that a
// {{Task.Param.*}} reference in an onExit command — which cannot resolve in the
// environment-action scope — does NOT abort teardown of remaining environments.
// ExitEnvironments must log/record the resolution error and continue, consistent
// with its "continue on error" semantics.
func TestExitEnvironment_TaskParamInOnExitContinuesTeardown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())

	// Entered in order: good, bad.  ExitEnvironments runs in reverse: bad
	// (fails to resolve), good (must still run and write the sentinel file).
	msg := &protocol.AssignMsg{
		JobID: "j",
		Environments: []protocol.AssignEnvironment{
			{
				Name: "good",
				OnExit: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "echo good-exited > sentinel.txt"},
				},
			},
			{
				Name: "bad",
				OnExit: &protocol.Action{
					Command: "sh",
					// Task.Param.* is not available in the environment scope.
					Args: []string{"-c", "echo {{Task.Param.Frame}}"},
				},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// ExitEnvironments should return an error (from the bad env) but must
	// not bail out before running the good env's OnExit.
	exitErr := s.ExitEnvironments(context.Background(), nopLogger())
	if exitErr == nil {
		t.Error("ExitEnvironments: want non-nil error for unresolvable onExit reference; got nil")
	}

	// "good" env's OnExit must have run despite the preceding failure.
	if _, statErr := os.Stat(filepath.Join(s.WorkDir, "sentinel.txt")); statErr != nil {
		t.Errorf("sentinel.txt not found — good env's OnExit did not run after bad env resolution failure: %v", statErr)
	}
}
