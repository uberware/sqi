// SPDX-License-Identifier: AGPL-3.0-or-later

package executor_test

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/worker/protocol"
)

// TestExecutor_Dispatch_BaseSpec_ExpressionSyntaxStaysMalformed proves the
// pre-EXPR (base-spec) resolution path in resolveAssignment is byte-for-byte
// unchanged by EXPR sub-project E4a's Task 6 wiring: msg.EXPR is unset (the
// zero value, false), and the {{...}} reference below is not a valid dotted
// OpenJD identifier -- it is only meaningful as an EXPR expression. Plain
// substitution (fmtstring.Resolve) rejects it as a malformed reference before
// ever asking what it means; only an EXPR-aware evaluator would parse and run
// it. If resolveAssignment were ever rerouted to the EXPR-aware family for a
// base-spec assignment (msg.EXPR left false), this task would wrongly
// SUCCEED with a computed value instead of failing here -- making this the
// cheapest possible detector for that regression.
func TestExecutor_Dispatch_BaseSpec_ExpressionSyntaxStaysMalformed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	exec, nc, _ := newTestExecutor(t, nil)

	msg := &protocol.AssignMsg{
		Version:           protocol.ProtocolVersion,
		Type:              protocol.TypeAssign,
		TaskID:            "basespec-expr-syntax-task",
		AttemptID:         "attempt-1",
		JobID:             "job-1",
		JobParameters:     map[string]string{"Scene": "world"},
		JobParameterTypes: map[string]string{"Scene": "STRING"},
		OnRun: &protocol.Action{
			Command: "sh",
			// Not a valid dotted identifier: only meaningful as an expression.
			Args: []string{"-c", `echo {{Param.Scene + "-suffix"}}`},
		},
	}

	if err := exec.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	waitForStatus(t, nc, 2, 10*time.Second)

	last := nc.lastStatus()
	if last.Status != "failed" {
		t.Fatalf("terminal status = %q; want failed (base-spec substitution must reject expression syntax); Message=%q", last.Status, last.Message)
	}
	if !strings.Contains(last.Message, "not a valid dotted identifier") {
		t.Errorf("Message = %q; want it to report a malformed dotted-identifier reference (proof base-spec substitution ran, not EXPR evaluation)", last.Message)
	}
}

// TestExecutor_Dispatch_EXPR_ResolvesExpressionInArgs is the EXPR-flag
// positive counterpart to the base-spec test above: the SAME shape of
// reference, now with msg.EXPR true and the declared parameter type EXPR
// needs, evaluates as a real expression and its computed value reaches the
// command line -- the first point in the whole EXPR program where that
// happens.
func TestExecutor_Dispatch_EXPR_ResolvesExpressionInArgs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	capture := &captureOutput{}
	exec, nc, _ := newTestExecutor(t, capture)

	msg := &protocol.AssignMsg{
		Version:           protocol.ProtocolVersion,
		Type:              protocol.TypeAssign,
		TaskID:            "expr-resolves-args-task",
		AttemptID:         "attempt-1",
		JobID:             "job-1",
		EXPR:              true,
		JobParameters:     map[string]string{"Scene": "world"},
		JobParameterTypes: map[string]string{"Scene": "STRING"},
		OnRun: &protocol.Action{
			Command: "sh",
			Args:    []string{"-c", `echo {{Param.Scene + "-suffix"}}`},
		},
	}

	if err := exec.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	waitForStatus(t, nc, 2, 10*time.Second)

	if last := nc.lastStatus(); last.Status != "succeeded" {
		t.Fatalf("terminal status = %q; want succeeded (Message=%q)", last.Status, last.Message)
	}
	if !hasStdoutLine(capture, "world-suffix") {
		t.Errorf("expected the evaluated expression's value %q in stdout; got: %v", "world-suffix", capture.all())
	}
}

// TestExecutor_Dispatch_EXPR_LetBindingAppliedOnceSharedByActionAndFile
// proves fmtres.ApplyTaskLet is called EXACTLY ONCE per resolveAssignment
// call, reusing the same symbol table for both the OnRun action and the step
// embedded file -- not once per resolution. ApplyTaskLet's own doc comment
// warns that calling it a second time over the same table makes every
// binding fail the shadow check ("doubled" already present as a key); had
// Task 6 wired ApplyTaskLet before EACH resolution instead of once before
// both, the second call here would return a shadow error and the task would
// fail pre-execution instead of succeeding with the expected output.
func TestExecutor_Dispatch_EXPR_LetBindingAppliedOnceSharedByActionAndFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	capture := &captureOutput{}
	exec, nc, _ := newTestExecutor(t, capture)

	msg := &protocol.AssignMsg{
		Version:        protocol.ProtocolVersion,
		Type:           protocol.TypeAssign,
		TaskID:         "expr-let-once-task",
		AttemptID:      "attempt-1",
		JobID:          "job-1",
		EXPR:           true,
		Parameters:     map[string]string{"Frame": "21"},
		ParameterTypes: map[string]string{"Frame": "INT"},
		StepScriptLet:  []string{"doubled = Task.Param.Frame * 2"},
		OnRun: &protocol.Action{
			Command: "sh",
			Args:    []string{"-c", "echo doubled={{doubled}} && cat data.txt"},
		},
		EmbeddedFiles: []protocol.EmbeddedFile{
			{Name: "data.txt", Data: "file-doubled={{doubled}}\n"},
		},
	}

	if err := exec.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	waitForStatus(t, nc, 2, 10*time.Second)

	if last := nc.lastStatus(); last.Status != "succeeded" {
		t.Fatalf("terminal status = %q; want succeeded (Message=%q) -- a shadow-check failure here means ApplyTaskLet ran more than once over the same table", last.Status, last.Message)
	}
	if !hasStdoutLine(capture, "doubled=42") {
		t.Errorf("expected let-bound value in OnRun args stdout; got: %v", capture.all())
	}
	if !hasStdoutLine(capture, "file-doubled=42") {
		t.Errorf("expected the SAME let-bound value in the embedded file's resolved data; got: %v", capture.all())
	}
}
