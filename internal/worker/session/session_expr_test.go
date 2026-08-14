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
	"github.com/uberware/sqi/internal/worker/fmtres"
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
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, nopLogger())

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

// TestManagerCreate_EnterEnvironment_EXPR_LetBindingSharedAcrossEnterAndFiles
// proves fmtres.ApplyEnvLet is called EXACTLY ONCE per environment entry,
// with the resulting table reused for BOTH onEnter and EmbeddedFiles -- not
// once per resolution. Had enterOne (via resolveEnvEntry) instead called
// ApplyEnvLet before each of those resolutions, the second call would fail
// the shadow check ("greeting" already a key in the table) and Create would
// return an error instead of succeeding with the expected content.
//
// Variables deliberately references Param.Scene, NOT the let binding: an
// environment script's let: names are Script's children's to see (actions,
// embeddedFiles), and Variables is a SIBLING of Script -- see
// TestManagerCreate_EnterEnvironment_EXPR_VariablesCannotSeeEnvLet below,
// and fmtres.ApplyEnvLet's doc comment. An earlier revision of this test
// resolved MY_VAR from "{{greeting}}" and passed, which was itself the
// divergence from phase 2 that the E4a whole-branch review recorded.
func TestManagerCreate_EnterEnvironment_EXPR_LetBindingSharedAcrossEnterAndFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	dataDir := t.TempDir()
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, nopLogger())

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
					Args:    []string{"-c", "echo {{greeting}} > entered.txt"},
				},
				Variables: map[string]string{"MY_VAR": `{{Param.Scene + "-var"}}`},
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
		t.Errorf("entered.txt (via OnEnter args, resolved by ResolveActionExpr) = %q; want %q", got, "world-suffix")
	}

	note, err := os.ReadFile(filepath.Join(s.WorkDir, "note.txt"))
	if err != nil {
		t.Fatalf("read note.txt: %v", err)
	}
	if got := strings.TrimSpace(string(note)); got != "world-suffix" {
		t.Errorf("note.txt (via EmbeddedFiles, resolved by ResolveEmbeddedFilesExpr) = %q; want %q", got, "world-suffix")
	}
	if got := s.StaticEnv()["MY_VAR"]; got != "world-var" {
		t.Errorf("MY_VAR (via Variables, resolved by ResolveVarsExpr) = %q; want %q", got, "world-var")
	}
}

// TestManagerCreate_EnterEnvironment_EXPR_VariablesCannotSeeEnvLet pins the
// ORDERING half of Template Schemas §3.6.2 row 4: an environment script's
// let: names reach the script's own children (actions, embeddedFiles) and
// nothing else, so a variable value referencing one is an unknown symbol --
// exactly what phase 2 reports for the same template
// (checkEnvironmentExpressions checks Variables against baseSyms, BEFORE the
// environment's own let is bound). Phase 3 matches it by resolving Variables
// before calling ApplyEnvLet; reversing those two lines in resolveEnvEntry
// makes this test fail.
func TestManagerCreate_EnterEnvironment_EXPR_VariablesCannotSeeEnvLet(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	dataDir := t.TempDir()
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID:             "j",
		EXPR:              true,
		JobParameters:     map[string]string{"Scene": "world"},
		JobParameterTypes: map[string]string{"Scene": "STRING"},
		Environments: []protocol.AssignEnvironment{
			{
				Name:      "env-a",
				Let:       []string{`envlet = Param.Scene + "-suffix"`},
				OnEnter:   &protocol.Action{Command: "sh", Args: []string{"-c", "echo hi"}},
				Variables: map[string]string{"MY_VAR": "{{envlet}}"},
			},
		},
	}

	if _, err := mgr.Create(context.Background(), msg); err == nil {
		t.Fatal("Create: want an unknown-symbol error for a variable referencing the environment's own let binding; got success")
	} else if !strings.Contains(err.Error(), `unknown symbol "envlet"`) {
		t.Errorf("error = %v; want it to report unknown symbol \"envlet\"", err)
	}
}

// TestManagerCreate_EnterEnvironment_EXPR_StepEnvironmentSeesStepTemplateLet
// is the E4a whole-branch review's Critical 1 reproducer, end to end: a step
// template's let: block binds "outdir", and that step's stepEnvironments use
// it in both a variable value and an onEnter argument. Template Schemas
// §3.6.2 row 1 grants the name to the whole stepEnvironments subtree, and
// phase 2 accepts the template with zero expression errors; before the fix,
// phase 3 failed BOTH positions with unknown symbol "outdir", which failed
// Manager.Create and therefore every task in the step.
func TestManagerCreate_EnterEnvironment_EXPR_StepEnvironmentSeesStepTemplateLet(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	dataDir := t.TempDir()
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID:           "j",
		EXPR:            true,
		StepName:        "S",
		StepTemplateLet: []string{`outdir = "/tmp/out"`},
		Environments: []protocol.AssignEnvironment{
			{
				Name:            "E",
				StepEnvironment: true,
				OnEnter:         &protocol.Action{Command: "sh", Args: []string{"-c", "echo {{outdir}} > entered.txt"}},
				Variables:       map[string]string{"OUT": "{{outdir}}"},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v -- a step environment must see its own step template's let bindings (§3.6.2 row 1)", err)
	}
	entered, err := os.ReadFile(filepath.Join(s.WorkDir, "entered.txt"))
	if err != nil {
		t.Fatalf("read entered.txt: %v", err)
	}
	if got := strings.TrimSpace(string(entered)); got != "/tmp/out" {
		t.Errorf("entered.txt = %q; want %q", got, "/tmp/out")
	}
	if got := s.StaticEnv()["OUT"]; got != "/tmp/out" {
		t.Errorf("OUT = %q; want %q", got, "/tmp/out")
	}
}

// TestManagerCreate_EnterEnvironment_EXPR_JobEnvironmentIgnoresStepTemplateLet
// is Critical 1's negative half: a JOB environment has no enclosing step, so
// §3.6.2 row 1 grants it nothing -- sub-project E3's §2.2 ruling and the
// 7.3.1--step-name-in-job-environment-let.invalid.yaml fixture require the
// step-template block to stay invisible here. AssignEnvironment.StepEnvironment
// (absent/false below) is the only bit that distinguishes the two levels on
// the wire.
func TestManagerCreate_EnterEnvironment_EXPR_JobEnvironmentIgnoresStepTemplateLet(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	dataDir := t.TempDir()
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID:           "j",
		EXPR:            true,
		StepName:        "S",
		StepTemplateLet: []string{`outdir = "/tmp/out"`},
		Environments: []protocol.AssignEnvironment{
			{
				Name:      "E",
				OnEnter:   &protocol.Action{Command: "sh", Args: []string{"-c", "echo hi"}},
				Variables: map[string]string{"OUT": "{{outdir}}"},
			},
		},
	}

	if _, err := mgr.Create(context.Background(), msg); err == nil {
		t.Fatal("Create: want an unknown-symbol error -- a job environment must not see a step template's let bindings; got success")
	} else if !strings.Contains(err.Error(), `unknown symbol "outdir"`) {
		t.Errorf("error = %v; want it to report unknown symbol \"outdir\"", err)
	}
}

// TestSession_ExitEnvironments_EXPR_LetBindingAppliedOnce is the onExit
// counterpart: resolveEnvAction builds its OWN fresh table (EnvSymbols is
// called again at exit time, separate from enterOne's table) and must call
// ApplyEnvLet exactly once over it. A second call over the same table would
// fail the shadow check and ExitEnvironments would report an error instead
// of running onExit with the let-bound value available.
//
// As at entry, Variables references Param.Scene rather than the let binding:
// §3.6.2 row 4 does not grant an environment's let names to its Variables.
func TestSession_ExitEnvironments_EXPR_LetBindingAppliedOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	witnessDir := t.TempDir()
	dataDir := t.TempDir()
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, nopLogger())

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
					Args:    []string{"-c", "echo {{greeting}} > " + filepath.Join(witnessDir, "exited.txt")},
				},
				Variables: map[string]string{"MY_VAR": `{{Param.Scene + "-var"}}`},
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

// TestSession_ExitEnvironments_EXPR_StepEnvironmentSeesStepTemplateLet is
// Critical 1's teardown half. resolveEnvAction builds a fresh table at exit
// time, so the step-template block has to be folded in there too — otherwise
// an onExit action referencing a step-template binding fails with unknown
// symbol and the session cannot tear down cleanly.
func TestSession_ExitEnvironments_EXPR_StepEnvironmentSeesStepTemplateLet(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	witnessDir := t.TempDir()
	dataDir := t.TempDir()
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID:           "j",
		EXPR:            true,
		StepName:        "S",
		StepTemplateLet: []string{`tag = "teardown"`},
		Environments: []protocol.AssignEnvironment{
			{
				Name:            "E",
				StepEnvironment: true,
				OnEnter:         &protocol.Action{Command: "sh", Args: []string{"-c", "echo entered"}},
				OnExit: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "echo {{tag}} > " + filepath.Join(witnessDir, "exited.txt")},
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
	exited, err := os.ReadFile(filepath.Join(witnessDir, "exited.txt"))
	if err != nil {
		t.Fatalf("read exited.txt: %v", err)
	}
	if got := strings.TrimSpace(string(exited)); got != "teardown" {
		t.Errorf("exited.txt = %q; want %q", got, "teardown")
	}
}

// TestSession_ExitEnvironments_EXPR_VariablesCannotSeeEnvLet is the EXIT-path
// twin of TestManagerCreate_EnterEnvironment_EXPR_VariablesCannotSeeEnvLet.
// resolveEnvAction builds its own fresh table and orders Variables against
// ApplyEnvLet independently of resolveEnvEntry, so reversing those two lines
// on the exit path alone would otherwise go unnoticed. The environment is
// entered with variables that resolve cleanly (Param.Scene) and torn down
// with one that references the environment's own let name, which §3.6.2 row 4
// does not grant to Variables.
func TestSession_ExitEnvironments_EXPR_VariablesCannotSeeEnvLet(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	dataDir := t.TempDir()
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID:             "j",
		EXPR:              true,
		JobParameters:     map[string]string{"Scene": "world"},
		JobParameterTypes: map[string]string{"Scene": "STRING"},
		Environments: []protocol.AssignEnvironment{
			{
				Name:      "env-a",
				Let:       []string{`envlet = Param.Scene + "-suffix"`},
				OnEnter:   &protocol.Action{Command: "sh", Args: []string{"-c", "echo entered"}},
				OnExit:    &protocol.Action{Command: "sh", Args: []string{"-c", "echo exiting"}},
				Variables: map[string]string{"MY_VAR": "{{Param.Scene}}"},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Swap in the offending variable for teardown only: entry must succeed so
	// that ExitEnvironments has an entered environment to tear down.
	s.msg.Environments[0].Variables["MY_VAR"] = "{{envlet}}"

	err = s.ExitEnvironments(context.Background(), nopLogger())
	if err == nil {
		t.Fatal("ExitEnvironments: want an unknown-symbol error for a variable referencing the environment's own let binding; got success")
	}
	if !strings.Contains(err.Error(), `unknown symbol "envlet"`) {
		t.Errorf("error = %v; want it to report unknown symbol \"envlet\"", err)
	}
}
