// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/worker/envutil"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// nopLogger returns a logger that discards all output, keeping test output clean.
func nopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 100}))
}

// ── Manager.Create ────────────────────────────────────────────────────────────

func TestManagerCreate_CreatesWorkDir(t *testing.T) {
	dataDir := t.TempDir()
	mgr := NewManager(dataDir, false, nopLogger())

	msg := &protocol.AssignMsg{JobID: "job-1"}
	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if s.ID == "" {
		t.Error("session ID must not be empty")
	}
	if s.JobID != "job-1" {
		t.Errorf("JobID = %q; want %q", s.JobID, "job-1")
	}
	if s.CreatedAt.IsZero() {
		t.Error("CreatedAt must not be zero")
	}

	// Working directory must exist under <dataDir>/sessions/<id>/.
	want := filepath.Join(dataDir, "sessions", s.ID)
	if s.WorkDir != want {
		t.Errorf("WorkDir = %q; want %q", s.WorkDir, want)
	}
	if info, err := os.Stat(s.WorkDir); err != nil || !info.IsDir() {
		t.Errorf("WorkDir %q does not exist or is not a directory: %v", s.WorkDir, err)
	}
}

func TestManagerCreate_UniqueSessionIDs(t *testing.T) {
	dataDir := t.TempDir()
	mgr := NewManager(dataDir, false, nopLogger())

	msg := &protocol.AssignMsg{JobID: "job-x"}
	s1, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create 1: %v", err)
	}
	s2, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create 2: %v", err)
	}

	if s1.ID == s2.ID {
		t.Errorf("expected unique IDs; got %q for both sessions", s1.ID)
	}
	if s1.WorkDir == s2.WorkDir {
		t.Errorf("expected unique working directories; got %q for both sessions", s1.WorkDir)
	}
}

// ── Environment entry ─────────────────────────────────────────────────────────

func TestManagerCreate_NoEnvironments(t *testing.T) {
	dataDir := t.TempDir()
	mgr := NewManager(dataDir, false, nopLogger())

	msg := &protocol.AssignMsg{JobID: "j", Environments: nil}
	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.ActiveTaskCount() != 0 {
		t.Errorf("expected 0 active tasks; got %d", s.ActiveTaskCount())
	}
}

func TestManagerCreate_EnterEnvironment_Success(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, false, nopLogger())

	// OnEnter writes a sentinel file to the working directory.
	msg := &protocol.AssignMsg{
		JobID: "j",
		Environments: []protocol.AssignEnvironment{
			{
				Name: "env-a",
				OnEnter: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "echo entered > entered.txt"},
				},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	sentinel := filepath.Join(s.WorkDir, "entered.txt")
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("OnEnter sentinel file not found: %v", err)
	}
}

func TestManagerCreate_EnterEnvironment_RunsInDeclarationOrder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, false, nopLogger())

	// Each environment appends its name to a log file; we verify order.
	msg := &protocol.AssignMsg{
		JobID: "j",
		Environments: []protocol.AssignEnvironment{
			{
				Name: "first",
				OnEnter: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "echo first >> order.txt"},
				},
			},
			{
				Name: "second",
				OnEnter: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "echo second >> order.txt"},
				},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(s.WorkDir, "order.txt"))
	if err != nil {
		t.Fatalf("read order.txt: %v", err)
	}
	lines := strings.Fields(string(content))
	if len(lines) != 2 || lines[0] != "first" || lines[1] != "second" {
		t.Errorf("expected [first second]; got %v", lines)
	}
}

func TestManagerCreate_EnterEnvironment_FailureTriggersTeardown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	// Use a real directory (not t.TempDir()) so we can inspect content written
	// by OnExit before the working directory is removed by Create's cleanup.
	// We write the teardown sentinel to a separate witness directory that
	// persists after the session working directory is deleted.
	witnessDir := t.TempDir()
	dataDir := t.TempDir()
	mgr := NewManager(dataDir, false, nopLogger())

	// First env enters successfully. Second env fails.
	// We assert that first env's OnExit was run (by writing to witnessDir).
	msg := &protocol.AssignMsg{
		JobID: "j",
		Environments: []protocol.AssignEnvironment{
			{
				Name: "ok-env",
				OnEnter: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "echo ok"},
				},
				OnExit: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "echo torn-down > " + filepath.Join(witnessDir, "torn_down.txt")},
				},
			},
			{
				Name: "bad-env",
				OnEnter: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "exit 1"},
				},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err == nil {
		t.Fatal("expected Create to return an error when OnEnter fails")
	}
	if s != nil {
		t.Errorf("Create must return nil session on error; got %+v", s)
	}
	if !strings.Contains(err.Error(), "bad-env") {
		t.Errorf("error should mention failing env; got: %v", err)
	}

	// OnExit teardown must have run (written to witnessDir, not the session dir).
	sentinel := filepath.Join(witnessDir, "torn_down.txt")
	if _, statErr := os.Stat(sentinel); statErr != nil {
		t.Errorf("OnExit teardown sentinel not found in witnessDir after OnEnter failure: %v", statErr)
	}

	// The session working directory must have been cleaned up by Create.
	sessionsDir := filepath.Join(dataDir, "sessions")
	entries, readErr := os.ReadDir(sessionsDir)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("read sessions dir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Errorf("expected sessions directory to be empty after Create failure cleanup; found %d entries", len(entries))
	}
}

// ── Environment exit ──────────────────────────────────────────────────────────

func TestSession_ExitEnvironments_ReverseOrder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, false, nopLogger())

	msg := &protocol.AssignMsg{
		JobID: "j",
		Environments: []protocol.AssignEnvironment{
			{
				Name: "alpha",
				OnExit: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "echo alpha >> exit_order.txt"},
				},
			},
			{
				Name: "beta",
				OnExit: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "echo beta >> exit_order.txt"},
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

	content, err := os.ReadFile(filepath.Join(s.WorkDir, "exit_order.txt"))
	if err != nil {
		t.Fatalf("read exit_order.txt: %v", err)
	}
	lines := strings.Fields(string(content))
	// Entered: alpha, beta → exit: beta, alpha
	if len(lines) != 2 || lines[0] != "beta" || lines[1] != "alpha" {
		t.Errorf("expected [beta alpha] (reverse entry order); got %v", lines)
	}
}

func TestSession_ExitEnvironments_Idempotent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, false, nopLogger())

	msg := &protocol.AssignMsg{
		JobID: "j",
		Environments: []protocol.AssignEnvironment{
			{
				Name: "env",
				OnExit: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "echo x >> count.txt"},
				},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.ExitEnvironments(context.Background(), nopLogger()); err != nil {
		t.Fatalf("first ExitEnvironments: %v", err)
	}
	// Second call must be a no-op (idempotent).
	if err := s.ExitEnvironments(context.Background(), nopLogger()); err != nil {
		t.Fatalf("second ExitEnvironments: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(s.WorkDir, "count.txt"))
	if err != nil {
		t.Fatalf("read count.txt: %v", err)
	}
	lines := strings.Fields(string(content))
	if len(lines) != 1 {
		t.Errorf("OnExit should run exactly once; got %d lines in count.txt", len(lines))
	}
}

func TestSession_ExitEnvironments_ContinuesTeardownOnFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, false, nopLogger())

	// First env's OnExit fails; second env's OnExit should still run.
	msg := &protocol.AssignMsg{
		JobID: "j",
		Environments: []protocol.AssignEnvironment{
			{
				Name: "good",
				OnExit: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "echo good-exited > good.txt"},
				},
			},
			{
				Name: "bad",
				OnExit: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "exit 1"},
				},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// ExitEnvironments runs in reverse: bad (fails), good (succeeds).
	exitErr := s.ExitEnvironments(context.Background(), nopLogger())
	if exitErr == nil {
		t.Error("expected ExitEnvironments to return an error")
	}

	// "good" env's OnExit must still have run despite "bad" failing.
	if _, err := os.Stat(filepath.Join(s.WorkDir, "good.txt")); err != nil {
		t.Errorf("good.txt not found — good env's OnExit did not run despite bad env failure: %v", err)
	}
}

// ── Cleanup ───────────────────────────────────────────────────────────────────

func TestManagerCleanup_RemovesWorkDir(t *testing.T) {
	dataDir := t.TempDir()
	mgr := NewManager(dataDir, false, nopLogger())

	s, err := mgr.Create(context.Background(), &protocol.AssignMsg{JobID: "j"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	workDir := s.WorkDir
	mgr.Cleanup(context.Background(), s, false)

	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Errorf("expected working directory to be removed; stat returned: %v", err)
	}
}

func TestManagerCleanup_KeepFailedSessions_Retained(t *testing.T) {
	dataDir := t.TempDir()
	mgr := NewManager(dataDir, true /* keepFailedSessions */, nopLogger())

	s, err := mgr.Create(context.Background(), &protocol.AssignMsg{JobID: "j"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	workDir := s.WorkDir
	mgr.Cleanup(context.Background(), s, true /* failed */)

	if _, err := os.Stat(workDir); err != nil {
		t.Errorf("expected working directory to be retained (keepFailedSessions=true, failed=true); got: %v", err)
	}
}

func TestManagerCleanup_KeepFailedSessions_SuccessStillRemoved(t *testing.T) {
	dataDir := t.TempDir()
	// keepFailedSessions=true but the session succeeded → still remove.
	mgr := NewManager(dataDir, true /* keepFailedSessions */, nopLogger())

	s, err := mgr.Create(context.Background(), &protocol.AssignMsg{JobID: "j"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	workDir := s.WorkDir
	mgr.Cleanup(context.Background(), s, false /* not failed */)

	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Errorf("expected working directory to be removed for successful session; got: %v", err)
	}
}

func TestManagerCleanup_NoKeepFailed_FailedRemoved(t *testing.T) {
	dataDir := t.TempDir()
	mgr := NewManager(dataDir, false /* keepFailedSessions */, nopLogger())

	s, err := mgr.Create(context.Background(), &protocol.AssignMsg{JobID: "j"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	workDir := s.WorkDir
	mgr.Cleanup(context.Background(), s, true /* failed */)

	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Errorf("expected working directory to be removed (keepFailedSessions=false); got: %v", err)
	}
}

// ── Active task tracking ────────────────────────────────────────────

func TestSession_ActiveTaskTracking(t *testing.T) {
	s := &Session{ID: "test-session"}

	if got := s.ActiveTaskCount(); got != 0 {
		t.Fatalf("initial count = %d; want 0", got)
	}

	s.AddTask("task-1")
	s.AddTask("task-2")

	if got := s.ActiveTaskCount(); got != 2 {
		t.Errorf("count after 2 adds = %d; want 2", got)
	}

	ids := s.ActiveTaskIDs()
	if len(ids) != 2 {
		t.Errorf("len(ActiveTaskIDs()) = %d; want 2", len(ids))
	}

	s.RemoveTask("task-1")
	if got := s.ActiveTaskCount(); got != 1 {
		t.Errorf("count after removing task-1 = %d; want 1", got)
	}
	if ids := s.ActiveTaskIDs(); len(ids) != 1 || ids[0] != "task-2" {
		t.Errorf("ActiveTaskIDs after remove = %v; want [task-2]", ids)
	}

	// Removing a non-existent ID is a no-op.
	s.RemoveTask("task-999")
	if got := s.ActiveTaskCount(); got != 1 {
		t.Errorf("count after removing non-existent id = %d; want 1", got)
	}
}

func TestSession_ActiveTaskTracking_Concurrent(t *testing.T) {
	s := &Session{ID: "test-session"}
	const n = 100

	var wg sync.WaitGroup
	for i := range n {
		taskID := fmt.Sprintf("task-%d", i)
		wg.Go(func() {
			s.AddTask(taskID)
			time.Sleep(time.Microsecond)
			s.RemoveTask(taskID)
		})
	}
	wg.Wait()

	if got := s.ActiveTaskCount(); got != 0 {
		t.Errorf("expected 0 active tasks after all goroutines finish; got %d", got)
	}
}

// ── Embedded files ──────────────────────────────────────────────────

func TestWriteEmbeddedFiles_Basic(t *testing.T) {
	dir := t.TempDir()
	files := []protocol.EmbeddedFile{
		{Name: "hello.txt", Data: "hello world\n"},
	}
	if err := writeEmbeddedFiles(dir, files); err != nil {
		t.Fatalf("writeEmbeddedFiles: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(got) != "hello world\n" {
		t.Errorf("content = %q; want %q", got, "hello world\n")
	}
}

func TestWriteEmbeddedFiles_RunnableBit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions not applicable on Windows")
	}
	dir := t.TempDir()
	files := []protocol.EmbeddedFile{
		{Name: "run.sh", Data: "#!/bin/sh\necho hi\n", Runnable: true},
	}
	if err := writeEmbeddedFiles(dir, files); err != nil {
		t.Fatalf("writeEmbeddedFiles: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "run.sh"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("runnable file should have execute bit set; mode=%v", info.Mode())
	}
}

func TestWriteEmbeddedFiles_PathTraversalRejected(t *testing.T) {
	dir := t.TempDir()
	files := []protocol.EmbeddedFile{
		{Name: "evil", Filename: "../escape.txt", Data: "oops"},
	}
	if err := writeEmbeddedFiles(dir, files); err == nil {
		t.Error("expected error for path traversal filename; got nil")
	}
}

func TestWriteEmbeddedFiles_FallbackToName(t *testing.T) {
	dir := t.TempDir()
	// When Filename is empty, Name should be used.
	files := []protocol.EmbeddedFile{
		{Name: "by-name.txt", Filename: "", Data: "content"},
	}
	if err := writeEmbeddedFiles(dir, files); err != nil {
		t.Fatalf("writeEmbeddedFiles: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "by-name.txt")); err != nil {
		t.Errorf("expected file by-name.txt; got: %v", err)
	}
}

// ── EOL conversion ────────────────────────────────────────────────────────────

func TestApplyEOL(t *testing.T) {
	tests := []struct {
		name string
		data string
		eol  string
		want string
	}{
		{
			name: "AUTO passthrough",
			data: "a\r\nb\nc\rd",
			eol:  "",
			want: "a\r\nb\nc\rd",
		},
		{
			name: "AUTO explicit",
			data: "a\r\nb\nc\rd",
			eol:  "AUTO",
			want: "a\r\nb\nc\rd",
		},
		{
			name: "LF normalise CRLF",
			data: "a\r\nb\r\n",
			eol:  "LF",
			want: "a\nb\n",
		},
		{
			name: "LF normalise lone CR",
			data: "a\rb\r",
			eol:  "LF",
			want: "a\nb\n",
		},
		{
			name: "LF mixed",
			data: "a\r\nb\nc\rd",
			eol:  "LF",
			want: "a\nb\nc\nd",
		},
		{
			name: "LF empty input",
			data: "",
			eol:  "LF",
			want: "",
		},
		{
			name: "CRLF normalise LF",
			data: "a\nb\n",
			eol:  "CRLF",
			want: "a\r\nb\r\n",
		},
		{
			name: "CRLF idempotent on CRLF",
			data: "a\r\nb\r\n",
			eol:  "CRLF",
			want: "a\r\nb\r\n",
		},
		{
			name: "CRLF normalise lone CR",
			data: "a\rb\r",
			eol:  "CRLF",
			want: "a\r\nb\r\n",
		},
		{
			name: "CRLF empty input",
			data: "",
			eol:  "CRLF",
			want: "",
		},
		{
			name: "no line endings",
			data: "hello world",
			eol:  "LF",
			want: "hello world",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := applyEOL(tc.data, tc.eol)
			if got != tc.want {
				t.Errorf("applyEOL(%q, %q) = %q; want %q", tc.data, tc.eol, got, tc.want)
			}
		})
	}
}

// ── Session.WriteEmbeddedFiles (public method) ────────────────────────────────

func TestSession_WriteEmbeddedFiles_WritesContent(t *testing.T) {
	s := &Session{WorkDir: t.TempDir()}
	files := []protocol.EmbeddedFile{
		{Name: "data.txt", Data: "hello embedded\n"},
	}
	if err := s.WriteEmbeddedFiles(files); err != nil {
		t.Fatalf("WriteEmbeddedFiles: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(s.WorkDir, "data.txt"))
	if err != nil {
		t.Fatalf("read data.txt: %v", err)
	}
	if string(got) != "hello embedded\n" {
		t.Errorf("content = %q; want %q", string(got), "hello embedded\n")
	}
}

func TestSession_WriteEmbeddedFiles_RunnableBit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions not applicable on Windows")
	}
	s := &Session{WorkDir: t.TempDir()}
	files := []protocol.EmbeddedFile{
		{Name: "run.sh", Data: "#!/bin/sh\necho hi\n", Runnable: true},
	}
	if err := s.WriteEmbeddedFiles(files); err != nil {
		t.Fatalf("WriteEmbeddedFiles: %v", err)
	}
	info, err := os.Stat(filepath.Join(s.WorkDir, "run.sh"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("runnable file should have execute bit set; mode=%v", info.Mode())
	}
}

func TestSession_WriteEmbeddedFiles_EOLConversion(t *testing.T) {
	s := &Session{WorkDir: t.TempDir()}
	files := []protocol.EmbeddedFile{
		{Name: "crlf.txt", Data: "line1\r\nline2\r\n", EndOfLine: "LF"},
	}
	if err := s.WriteEmbeddedFiles(files); err != nil {
		t.Fatalf("WriteEmbeddedFiles: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(s.WorkDir, "crlf.txt"))
	if err != nil {
		t.Fatalf("read crlf.txt: %v", err)
	}
	want := "line1\nline2\n"
	if string(got) != want {
		t.Errorf("EOL-converted content = %q; want %q", string(got), want)
	}
}

func TestSession_WriteEmbeddedFiles_PathTraversalError(t *testing.T) {
	s := &Session{WorkDir: t.TempDir()}
	files := []protocol.EmbeddedFile{
		{Name: "evil", Filename: "../escape.txt", Data: "oops"},
	}
	if err := s.WriteEmbeddedFiles(files); err == nil {
		t.Error("expected error for path traversal filename; got nil")
	}
}

// ── envutil.Build (via session) ───────────────────────────────────────────────
// These tests exercise the shared helper that session uses for process env
// construction; they are kept here to confirm the wiring after the refactor.

func TestBuildEnv_OverridesInheritedVars(t *testing.T) {
	overrides := map[string]string{"MY_VAR": "overridden"}
	env := envutil.Build(overrides)

	found := false
	for _, kv := range env {
		if kv == "MY_VAR=overridden" {
			found = true
		}
	}
	if !found {
		t.Error("expected MY_VAR=overridden in built environment")
	}
}

func TestBuildEnv_NilOverrides_ReturnsOsEnviron(t *testing.T) {
	// Capture os.Environ() once to avoid TOCTOU if another goroutine changes
	// the environment between the Build call and the comparison.
	want := os.Environ()
	env := envutil.Build(nil)
	if len(env) != len(want) {
		t.Errorf("len(envutil.Build(nil)) = %d; want %d", len(env), len(want))
	}
}
