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

// hasStdoutLine reports whether capture recorded a stdout line equal to want.
func hasStdoutLine(capture *captureOutput, want string) bool {
	for _, l := range capture.all() {
		if l.stream == "stdout" && l.line == want {
			return true
		}
	}
	return false
}

// TestExecutor_Dispatch_EmbeddedFileDataResolved verifies that {{...}}
// references inside a step embedded file's data are resolved (against the task
// scope, which includes Task.Param.*) before the file is materialized.
func TestExecutor_Dispatch_EmbeddedFileDataResolved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	capture := &captureOutput{}
	exec, nc, _ := newTestExecutor(t, capture)

	msg := &protocol.AssignMsg{
		Version:    protocol.ProtocolVersion,
		Type:       protocol.TypeAssign,
		TaskID:     "embeds-dataresolved-task",
		AttemptID:  "attempt-1",
		JobID:      "job-1",
		Parameters: map[string]string{"Frame": "42"},
		OnRun: &protocol.Action{
			Command: "sh",
			Args:    []string{"-c", "cat data.txt"},
		},
		EmbeddedFiles: []protocol.EmbeddedFile{
			{Name: "data.txt", Data: "frame={{Task.Param.Frame}}\n"},
		},
	}

	if err := exec.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	waitForStatus(t, nc, 2, 10*time.Second)

	if last := nc.lastStatus(); last.Status != "succeeded" {
		t.Fatalf("terminal status = %q; want succeeded (Message=%q)", last.Status, last.Message)
	}
	if !hasStdoutLine(capture, "frame=42") {
		t.Errorf("expected resolved file content %q in stdout; got: %v", "frame=42", capture.all())
	}
}

// TestExecutor_Dispatch_TaskFileArg verifies that {{Task.File.<name>}} resolves
// to the materialized on-disk path of a step embedded file: here the script is
// run via that path as the OnRun argument.
func TestExecutor_Dispatch_TaskFileArg(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	capture := &captureOutput{}
	exec, nc, _ := newTestExecutor(t, capture)

	msg := &protocol.AssignMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeAssign,
		TaskID:    "task-file-arg-task",
		AttemptID: "attempt-1",
		JobID:     "job-1",
		OnRun: &protocol.Action{
			Command: "sh",
			// Run the materialized script by its Task.File path.
			Args: []string{"{{Task.File.script}}"},
		},
		EmbeddedFiles: []protocol.EmbeddedFile{
			{Name: "script", Filename: "render.sh", Data: "echo task-file-ran\n"},
		},
	}

	if err := exec.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	waitForStatus(t, nc, 2, 10*time.Second)

	if last := nc.lastStatus(); last.Status != "succeeded" {
		t.Fatalf("terminal status = %q; want succeeded (Message=%q)", last.Status, last.Message)
	}
	if !hasStdoutLine(capture, "task-file-ran") {
		t.Errorf("expected stdout %q from script run via Task.File path; got: %v", "task-file-ran", capture.all())
	}
}

// TestExecutor_Dispatch_EmbeddedFileDataUnknownVarFails verifies that an
// unresolvable {{...}} reference in step embedded-file data fails the task
// cleanly (running→failed, exit -1) naming the offending reference.
func TestExecutor_Dispatch_EmbeddedFileDataUnknownVarFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	exec, nc, _ := newTestExecutor(t, nil)

	msg := &protocol.AssignMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeAssign,
		TaskID:    "embeds-baddata-task",
		AttemptID: "attempt-1",
		JobID:     "job-1",
		OnRun: &protocol.Action{
			Command: "sh",
			Args:    []string{"-c", "echo should-not-run"},
		},
		EmbeddedFiles: []protocol.EmbeddedFile{
			{Name: "data.txt", Data: "{{Task.Param.Nope}}"},
		},
	}

	if err := exec.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	waitForStatus(t, nc, 2, 10*time.Second)

	last := nc.lastStatus()
	if last.Status != "failed" {
		t.Fatalf("terminal status = %q; want failed", last.Status)
	}
	if last.ExitCode == nil || *last.ExitCode != -1 {
		t.Errorf("ExitCode = %v; want -1", last.ExitCode)
	}
	if !strings.Contains(last.Message, "Task.Param.Nope") {
		t.Errorf("Message = %q; want it to name the unresolved reference %q", last.Message, "Task.Param.Nope")
	}
}

// TestExecutor_Dispatch_ResolvesFormatStringsInArgs verifies that {{...}}
// references in the OnRun command args are resolved with the task-action scope
// (Task.Param.*, Param.*, Session.WorkingDirectory) before the process runs.
func TestExecutor_Dispatch_ResolvesFormatStringsInArgs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	capture := &captureOutput{}
	exec, nc, _ := newTestExecutor(t, capture)

	msg := &protocol.AssignMsg{
		Version:       protocol.ProtocolVersion,
		Type:          protocol.TypeAssign,
		TaskID:        "resolve-args-task",
		AttemptID:     "attempt-1",
		JobID:         "job-1",
		Parameters:    map[string]string{"Frame": "42"},
		JobParameters: map[string]string{"Scene": "myscene"},
		OnRun: &protocol.Action{
			Command: "sh",
			Args:    []string{"-c", "echo frame={{Task.Param.Frame}} scene={{Param.Scene}}"},
		},
	}

	if err := exec.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	waitForStatus(t, nc, 2, 10*time.Second)

	if last := nc.lastStatus(); last.Status != "succeeded" {
		t.Fatalf("terminal status = %q; want succeeded (Message=%q)", last.Status, last.Message)
	}
	if !hasStdoutLine(capture, "frame=42 scene=myscene") {
		t.Errorf("resolved stdout line not captured; got: %v", capture.all())
	}
}

// TestExecutor_Dispatch_ResolvesSessionWorkingDirectory verifies that
// {{Session.WorkingDirectory}} resolves to the session working directory, which
// is only known at run time on the worker.
func TestExecutor_Dispatch_ResolvesSessionWorkingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	capture := &captureOutput{}
	exec, nc, dataDir := newTestExecutor(t, capture)

	msg := &protocol.AssignMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeAssign,
		TaskID:    "resolve-wd-task",
		AttemptID: "attempt-1",
		JobID:     "job-1",
		OnRun: &protocol.Action{
			Command: "sh",
			Args:    []string{"-c", "echo wd={{Session.WorkingDirectory}}"},
		},
	}

	if err := exec.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	waitForStatus(t, nc, 2, 10*time.Second)

	if last := nc.lastStatus(); last.Status != "succeeded" {
		t.Fatalf("terminal status = %q; want succeeded (Message=%q)", last.Status, last.Message)
	}
	// The resolved working directory must be under <dataDir>/sessions/.
	found := false
	for _, l := range capture.all() {
		if l.stream == "stdout" && strings.HasPrefix(l.line, "wd=") {
			found = true
			wd := strings.TrimPrefix(l.line, "wd=")
			if !strings.HasPrefix(wd, dataDir) {
				t.Errorf("resolved Session.WorkingDirectory = %q; want a path under %q", wd, dataDir)
			}
		}
	}
	if !found {
		t.Errorf("no wd= line captured; got: %v", capture.all())
	}
}

// TestExecutor_Dispatch_ResolvesEnvironmentVariableValues verifies that {{...}}
// references inside environment variable values are resolved (with the env
// scope) before being placed into the task process environment.
func TestExecutor_Dispatch_ResolvesEnvironmentVariableValues(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	capture := &captureOutput{}
	exec, nc, _ := newTestExecutor(t, capture)

	msg := &protocol.AssignMsg{
		Version:       protocol.ProtocolVersion,
		Type:          protocol.TypeAssign,
		TaskID:        "resolve-env-task",
		AttemptID:     "attempt-1",
		JobID:         "job-1",
		JobParameters: map[string]string{"Scene": "myscene"},
		Environments: []protocol.AssignEnvironment{
			{Name: "env-a", Variables: map[string]string{"MY_VAR": "scene-{{Param.Scene}}"}},
		},
		OnRun: &protocol.Action{
			Command: "sh",
			Args:    []string{"-c", "echo got=$MY_VAR"},
		},
	}

	if err := exec.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	waitForStatus(t, nc, 2, 10*time.Second)

	if last := nc.lastStatus(); last.Status != "succeeded" {
		t.Fatalf("terminal status = %q; want succeeded (Message=%q)", last.Status, last.Message)
	}
	if !hasStdoutLine(capture, "got=scene-myscene") {
		t.Errorf("resolved env var not seen by process; got: %v", capture.all())
	}
}

// TestExecutor_Dispatch_UnknownVariableFailsTask verifies that an OnRun action
// referencing a variable the scope does not provide fails the task with a
// descriptive message that names the offending reference.
func TestExecutor_Dispatch_UnknownVariableFailsTask(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	exec, nc, _ := newTestExecutor(t, nil)

	msg := &protocol.AssignMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeAssign,
		TaskID:    "unknown-var-task",
		AttemptID: "attempt-1",
		JobID:     "job-1",
		OnRun: &protocol.Action{
			Command: "sh",
			Args:    []string{"-c", "echo {{Task.Param.Missing}}"},
		},
	}

	if err := exec.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	waitForStatus(t, nc, 2, 10*time.Second)

	last := nc.lastStatus()
	if last.Status != "failed" {
		t.Fatalf("terminal status = %q; want failed", last.Status)
	}
	if !strings.Contains(last.Message, "Task.Param.Missing") {
		t.Errorf("failure Message = %q; want it to name the unresolved variable %q",
			last.Message, "Task.Param.Missing")
	}
}

// TestExecutor_Dispatch_EnvVarTaskParamFails verifies that an environment
// variable value referencing Task.Param.* — which is not in the environment
// scope — fails the environment cleanly: session setup fails and Dispatch
// returns a descriptive error naming the offending variable (the pull loop then
// nacks the assignment).
func TestExecutor_Dispatch_EnvVarTaskParamFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	exec, _, _ := newTestExecutor(t, nil)

	msg := &protocol.AssignMsg{
		Version:    protocol.ProtocolVersion,
		Type:       protocol.TypeAssign,
		TaskID:     "env-taskparam-task",
		AttemptID:  "attempt-1",
		JobID:      "job-1",
		Parameters: map[string]string{"Frame": "42"},
		Environments: []protocol.AssignEnvironment{
			{Name: "env-a", Variables: map[string]string{"BAD": "{{Task.Param.Frame}}"}},
		},
		OnRun: &protocol.Action{Command: "sh", Args: []string{"-c", "echo hi"}},
	}

	err := exec.Dispatch(context.Background(), msg)
	if err == nil {
		t.Fatal("Dispatch: want error for Task.Param reference in env variable; got nil")
	}
	if !strings.Contains(err.Error(), "Task.Param.Frame") {
		t.Errorf("error = %q; want it to name the unresolved variable %q", err.Error(), "Task.Param.Frame")
	}
}
