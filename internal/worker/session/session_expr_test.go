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

// ── EXPR sub-project E4a, Task 6: environment-path wiring ──────────────────
//
// These tests exercise enterOne/resolveEnvAction's EXPR-selection (msg.EXPR)
// directly against the Session package, complementing the executor-level
// tests in internal/worker/executor/resolve_expr_test.go (which drive the
// same wiring end-to-end via Executor.Dispatch, plus the task path).

// TestManagerCreate_EnterEnvironment_BaseSpec_ExpressionSyntaxStaysMalformed
// proves enterOne's pre-EXPR (base-spec) branch is byte-for-byte unchanged:
// msg.EXPR is unset (false, the zero value), and the {{...}} reference below
// is not a valid dotted OpenJD identifier -- only meaningful as an EXPR
// expression. Plain substitution (fmtres.EnvScope/ResolveVars) rejects it as
// a malformed reference; an EXPR-aware evaluator would instead parse and run
// it. If enterOne were ever rerouted to the EXPR-aware family for a
// base-spec assignment, this environment would wrongly enter successfully
// instead of failing here.
func TestManagerCreate_EnterEnvironment_BaseSpec_ExpressionSyntaxStaysMalformed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	dataDir := t.TempDir()
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID:         "j",
		JobParameters: map[string]string{"Scene": "world"},
		// EXPR is unset: must take literal substitution, not evaluation.
		Environments: []protocol.AssignEnvironment{
			{
				Name:      "env-a",
				OnEnter:   &protocol.Action{Command: "sh", Args: []string{"-c", "echo hi"}},
				Variables: map[string]string{"MY_VAR": `{{Param.Scene + "-suffix"}}`},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err == nil {
		t.Fatalf("Create: want error for expression syntax under base-spec substitution; got success (session %+v)", s)
	}
	if !strings.Contains(err.Error(), "not a valid dotted identifier") {
		t.Errorf("error = %v; want it to report a malformed dotted-identifier reference (proof base-spec substitution ran, not EXPR evaluation)", err)
	}
}

// TestManagerCreate_EnterEnvironment_EXPR_LetBindingSharedAcrossEnterVariablesAndFiles
// proves fmtres.ApplyEnvLet is called EXACTLY ONCE per environment entry,
// with the resulting table reused for onEnter, Variables, AND
// EmbeddedFiles -- not once per resolution. Had enterOne (via
// resolveEnvEntry) instead called ApplyEnvLet before each of those three
// resolutions, the second and third calls would fail the shadow check
// ("greeting" already a key in the table) and Create would return an error
// instead of succeeding with the expected content in both files.
func TestManagerCreate_EnterEnvironment_EXPR_LetBindingSharedAcrossEnterVariablesAndFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	dataDir := t.TempDir()
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID:             "j",
		EXPR:              true,
		JobParameters:     map[string]string{"Scene": "world"},
		JobParameterTypes: map[string]string{"Scene": "STRING"},
		Environments: []protocol.AssignEnvironment{
			{
				Name: "env-a",
				Let:  []string{`greeting = Param.Scene + "-suffix"`},
				OnEnter: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "echo $MY_VAR > entered.txt"},
				},
				Variables: map[string]string{"MY_VAR": "{{greeting}}"},
				EmbeddedFiles: []protocol.EmbeddedFile{
					{Name: "note", Filename: "note.txt", Data: "{{greeting}}\n"},
				},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v -- a shadow-check failure here means ApplyEnvLet ran more than once over the same table", err)
	}

	entered, err := os.ReadFile(filepath.Join(s.WorkDir, "entered.txt"))
	if err != nil {
		t.Fatalf("read entered.txt: %v", err)
	}
	if got := strings.TrimSpace(string(entered)); got != "world-suffix" {
		t.Errorf("entered.txt (via Variables, resolved by ResolveVarsExpr) = %q; want %q", got, "world-suffix")
	}

	note, err := os.ReadFile(filepath.Join(s.WorkDir, "note.txt"))
	if err != nil {
		t.Fatalf("read note.txt: %v", err)
	}
	if got := strings.TrimSpace(string(note)); got != "world-suffix" {
		t.Errorf("note.txt (via EmbeddedFiles, resolved by ResolveEmbeddedFilesExpr) = %q; want %q", got, "world-suffix")
	}
}

// TestSession_ExitEnvironments_EXPR_LetBindingAppliedOnce is the onExit
// counterpart: resolveEnvAction builds its OWN fresh table (EnvSymbols is
// called again at exit time, separate from enterOne's table) and must call
// ApplyEnvLet exactly once over it, reused for BOTH the onExit action and
// Variables. A second call over the same table would fail the shadow check
// and ExitEnvironments would report an error instead of running onExit with
// the let-bound value available.
func TestSession_ExitEnvironments_EXPR_LetBindingAppliedOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	witnessDir := t.TempDir()
	dataDir := t.TempDir()
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID:             "j",
		EXPR:              true,
		JobParameters:     map[string]string{"Scene": "world"},
		JobParameterTypes: map[string]string{"Scene": "STRING"},
		Environments: []protocol.AssignEnvironment{
			{
				Name:    "env-a",
				Let:     []string{`greeting = Param.Scene + "-suffix"`},
				OnEnter: &protocol.Action{Command: "sh", Args: []string{"-c", "echo entered"}},
				OnExit: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "echo $MY_VAR > " + filepath.Join(witnessDir, "exited.txt")},
				},
				Variables: map[string]string{"MY_VAR": "{{greeting}}"},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.ExitEnvironments(context.Background(), nopLogger()); err != nil {
		t.Fatalf("ExitEnvironments: %v -- a shadow-check failure here means ApplyEnvLet ran more than once over the same table", err)
	}

	exited, err := os.ReadFile(filepath.Join(witnessDir, "exited.txt"))
	if err != nil {
		t.Fatalf("read exited.txt: %v", err)
	}
	if got := strings.TrimSpace(string(exited)); got != "world-suffix" {
		t.Errorf("exited.txt = %q; want %q", got, "world-suffix")
	}
}
