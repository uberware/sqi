// SPDX-License-Identifier: AGPL-3.0-or-later

package proctree

import (
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestNewToken_Is32LowerHex(t *testing.T) {
	a, b := NewToken(), NewToken()
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(a) {
		t.Errorf("NewToken() = %q, want 32 lowercase hex characters", a)
	}
	if a == b {
		t.Errorf("two NewToken() calls both returned %q", a)
	}
}

func TestMarkEnv_AppendsToken(t *testing.T) {
	got := MarkEnv([]string{"A=1"}, "t")
	want := []string{"A=1", "SQI_PROCESS_TREE=t"}
	if !slices.Equal(got, want) {
		t.Errorf("MarkEnv = %q, want %q", got, want)
	}
}

func TestMarkEnv_ReplacesInheritedToken(t *testing.T) {
	got := MarkEnv([]string{"SQI_PROCESS_TREE=old", "B=2"}, "new")
	var marks []string
	for _, kv := range got {
		if strings.HasPrefix(kv, EnvVar+"=") {
			marks = append(marks, kv)
		}
	}
	if !slices.Equal(marks, []string{"SQI_PROCESS_TREE=new"}) {
		t.Errorf("marker entries = %q, want exactly [SQI_PROCESS_TREE=new]", marks)
	}
	if !slices.Contains(got, "B=2") {
		t.Errorf("MarkEnv dropped B=2: %q", got)
	}
}

func TestMarkEnv_NilStartsFromProcessEnvironment(t *testing.T) {
	t.Setenv("SQI_TEST_MARK", "x")
	got := MarkEnv(nil, "t")
	if !slices.Contains(got, "SQI_TEST_MARK=x") {
		t.Errorf("MarkEnv(nil) lacks the process environment: %q", got)
	}
	if !slices.Contains(got, "SQI_PROCESS_TREE=t") {
		t.Errorf("MarkEnv(nil) lacks the marker: %q", got)
	}
}

func TestOpenLedger_CreatesDir0700(t *testing.T) {
	dir := t.TempDir() + "/proctrees"
	if _, err := OpenLedger(dir); err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat ledger dir: %v", err)
	}
	if !fi.IsDir() {
		t.Fatalf("%s is not a directory", dir)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
		t.Errorf("ledger dir mode = %o, want 700", fi.Mode().Perm())
	}
}

func TestNilLedger_IsNoOp(t *testing.T) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(t.Context(), "ping", "-n", "30", "127.0.0.1")
	} else {
		cmd = exec.CommandContext(t.Context(), "sleep", "30")
	}
	Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	tree, err := Supervise(cmd)
	if err != nil {
		t.Fatalf("Supervise: %v", err)
	}
	t.Cleanup(func() {
		tree.Kill() //nolint:errcheck // best-effort cleanup of the test's process
		cmd.Wait()  //nolint:errcheck // reaps the killed process
	})
	if err := tree.Track(nil, NewToken(), Label{}); err != nil {
		t.Errorf("Track(nil ledger) = %v, want nil", err)
	}
	if err := tree.Release(); err != nil {
		t.Errorf("Release = %v, want nil", err)
	}
}
