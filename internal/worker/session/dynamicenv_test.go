// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// TestDynamicEnv_SetVisibleToSubsequentEnv verifies that an openjd_env directive
// emitted by one environment's onEnter is visible to a SUBSEQUENT environment's
// onEnter action.
func TestDynamicEnv_SetVisibleToSubsequentEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	dataDir := t.TempDir()
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID: "j",
		Environments: []protocol.AssignEnvironment{
			{
				Name: "exporter",
				OnEnter: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "echo 'openjd_env: FOO=bar'"},
				},
			},
			{
				Name: "consumer",
				OnEnter: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", `printf '%s' "$FOO" > foo.txt`},
				},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got := readFile(t, filepath.Join(s.WorkDir, "foo.txt"))
	if got != "bar" {
		t.Errorf("subsequent env saw FOO=%q; want %q", got, "bar")
	}
}

// TestDynamicEnv_UnsetRemovesStaticVariable verifies that openjd_unset_env removes
// a variable that a static environment Variables entry sets for a subsequent
// environment.
func TestDynamicEnv_UnsetRemovesStaticVariable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	dataDir := t.TempDir()
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID: "j",
		Environments: []protocol.AssignEnvironment{
			{
				Name: "unsetter",
				OnEnter: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "echo 'openjd_unset_env: FOO'"},
				},
			},
			{
				Name: "consumer",
				// FOO is set statically here, but the earlier unset directive
				// must win and remove it from this action's environment.
				Variables: map[string]string{"FOO": "static-val"},
				OnEnter: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", `printf '[%s]' "$FOO" > foo.txt`},
				},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got := readFile(t, filepath.Join(s.WorkDir, "foo.txt"))
	if got != "[]" {
		t.Errorf("subsequent env saw FOO=%q; want empty (unset)", got)
	}
}

// TestDynamicEnv_UnsetRemovesEarlierDirective verifies that openjd_unset_env
// removes a variable that an earlier openjd_env directive set.
func TestDynamicEnv_UnsetRemovesEarlierDirective(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	dataDir := t.TempDir()
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID: "j",
		Environments: []protocol.AssignEnvironment{
			{
				Name: "exporter",
				OnEnter: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "echo 'openjd_env: FOO=bar'; echo 'openjd_unset_env: FOO'"},
				},
			},
			{
				Name: "consumer",
				OnEnter: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", `printf '[%s]' "$FOO" > foo.txt`},
				},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got := readFile(t, filepath.Join(s.WorkDir, "foo.txt"))
	if got != "[]" {
		t.Errorf("subsequent env saw FOO=%q; want empty (unset)", got)
	}
}

// TestDynamicEnv_RedactedValueNotLoggedButApplied verifies that an
// openjd_redacted_env value never appears in captured logs, yet the variable is
// still applied to subsequent actions.
func TestDynamicEnv_RedactedValueNotLoggedButApplied(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	dataDir := t.TempDir()
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, logger)

	msg := &protocol.AssignMsg{
		JobID: "j",
		Environments: []protocol.AssignEnvironment{
			{
				Name: "exporter",
				OnEnter: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "echo 'openjd_redacted_env: SECRET=shh'"},
				},
			},
			{
				Name: "consumer",
				OnEnter: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", `printf '%s' "$SECRET" > secret.txt`},
				},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The variable must be applied to the subsequent action.
	got := readFile(t, filepath.Join(s.WorkDir, "secret.txt"))
	if got != "shh" {
		t.Errorf("subsequent env saw SECRET=%q; want %q", got, "shh")
	}

	// The secret value must NOT appear anywhere in the captured logs.
	logs := buf.String()
	if strings.Contains(logs, "shh") {
		t.Errorf("secret value leaked into logs:\n%s", logs)
	}
	// The redaction placeholder should be present so operators still see the directive.
	if !strings.Contains(logs, "<redacted>") {
		t.Errorf("expected <redacted> placeholder in logs:\n%s", logs)
	}
}

// TestDynamicEnv_LongLineDoesNotFailAction is a regression test for the
// bufio.Scanner default 64 KB token limit. Previously, scanActionStream used
// bufio.NewScanner without raising the buffer, so any single output line longer
// than 64 KB caused bufio.ErrTooLong: the scan goroutine exited, the child
// process received SIGPIPE and terminated with a non-zero exit code, and the
// action was failed even though the process logic succeeded.
//
// The fix raises the per-token limit to 4 MB. This test verifies that:
//   - Create succeeds when an onEnter action emits a 100 KB line (> 64 KB).
//   - An openjd_env directive emitted before the long line is still applied.
func TestDynamicEnv_LongLineDoesNotFailAction(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	dataDir := t.TempDir()
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())

	// The onEnter script:
	//   1. Emits an openjd_env directive (must be applied to the session).
	//   2. Emits a single 100 KB line of 'x' characters (exceeds the old 64 KB
	//      bufio.Scanner default, confirming the regression is fixed).
	//   3. Exits 0.
	// After the fix, Create must succeed and the directive must be visible to the
	// subsequent environment's onEnter action.
	msg := &protocol.AssignMsg{
		JobID: "j",
		Environments: []protocol.AssignEnvironment{
			{
				Name: "longline",
				OnEnter: &protocol.Action{
					Command: "sh",
					Args: []string{
						"-c",
						"echo 'openjd_env: LONGLINE_RESULT=directive-ok' && " +
							"awk 'BEGIN{for(i=0;i<102400;i++)printf \"x\";print \"\"}'",
					},
				},
			},
			{
				Name: "verify",
				OnEnter: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", `printf '%s' "$LONGLINE_RESULT" > longline.txt`},
				},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: unexpected error when onEnter emits a long log line: %v", err)
	}

	got := readFile(t, filepath.Join(s.WorkDir, "longline.txt"))
	if got != "directive-ok" {
		t.Errorf("LONGLINE_RESULT = %q; want %q (directive must survive a long log line)", got, "directive-ok")
	}
}

// TestStaticEnv_MergedResolvedAndCloned verifies the static environment is
// resolved once at session-create time (against {{Param.*}}), merged across
// environments with later-overrides-earlier precedence, and returned as a clone
// the caller may mutate freely. The executor reuses this rather than re-resolving
// env vars against a merged all-environments scope.
func TestStaticEnv_MergedResolvedAndCloned(t *testing.T) {
	dataDir := t.TempDir()
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID:         "j",
		JobParameters: map[string]string{"Scene": "hero"},
		Environments: []protocol.AssignEnvironment{
			{Name: "a", Variables: map[string]string{"SHARED": "first", "ONLY_A": "scene-{{Param.Scene}}"}},
			{Name: "b", Variables: map[string]string{"SHARED": "second"}},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	env := s.StaticEnv()
	if env["SHARED"] != "second" {
		t.Errorf("SHARED = %q; want %q (later environment overrides earlier)", env["SHARED"], "second")
	}
	if env["ONLY_A"] != "scene-hero" {
		t.Errorf("ONLY_A = %q; want %q ({{Param.Scene}} resolved)", env["ONLY_A"], "scene-hero")
	}

	// Mutating the returned map must not affect the session's stored copy.
	env["SHARED"] = "mutated"
	if again := s.StaticEnv()["SHARED"]; again != "second" {
		t.Errorf("StaticEnv returned an aliased map: second call SHARED = %q; want %q", again, "second")
	}
}

// TestScrubRedacted verifies the exported Session.ScrubRedacted (applied by the
// executor to every task stdout/stderr line) replaces registered redacted values
// and leaves output untouched when none are registered.
func TestScrubRedacted(t *testing.T) {
	s := &Session{redactedValues: map[string]bool{"shh": true}}
	if got, want := s.ScrubRedacted("token=shh done"), "token="+redactedPlaceholder+" done"; got != want {
		t.Errorf("ScrubRedacted = %q; want %q", got, want)
	}
	if got := s.ScrubRedacted("no secret here"); got != "no secret here" {
		t.Errorf("ScrubRedacted altered a line with no redacted value: %q", got)
	}

	// With nothing registered, lines pass through unchanged.
	empty := &Session{redactedValues: map[string]bool{}}
	if got := empty.ScrubRedacted("shh"); got != "shh" {
		t.Errorf("ScrubRedacted with no registered values = %q; want unchanged", got)
	}
}

// readFile reads a whole file and fails the test on error.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
