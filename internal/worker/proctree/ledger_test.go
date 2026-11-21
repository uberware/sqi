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

var tokenPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// startSleepTree starts a 30 s sleep as a supervised tree whose environment
// MarkEnv marked, and kills it when the test ends.
func startSleepTree(t *testing.T) (*exec.Cmd, *Tree) {
	t.Helper()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(t.Context(), "ping", "-n", "30", "127.0.0.1")
	} else {
		cmd = exec.CommandContext(t.Context(), "sleep", "30")
	}
	Configure(cmd)
	cmd.Env = MarkEnv(nil)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	tree, err := Supervise(cmd)
	if err != nil {
		t.Fatalf("Supervise: %v", err)
	}
	t.Cleanup(func() {
		tree.Kill() //nolint:errcheck // already dead in some tests
		cmd.Wait()  //nolint:errcheck // reaps the killed process
	})
	return cmd, tree
}

// markers returns env's EnvVar entries.
func markers(env []string) []string {
	var out []string
	for _, kv := range env {
		if strings.HasPrefix(kv, EnvVar+"=") {
			out = append(out, kv)
		}
	}
	return out
}

func TestNewToken_Is32LowerHex(t *testing.T) {
	a, b := newToken(), newToken()
	if !tokenPattern.MatchString(a) {
		t.Errorf("newToken() = %q, want 32 lowercase hex characters", a)
	}
	if a == b {
		t.Errorf("two newToken() calls both returned %q", a)
	}
}

func TestMarkEnv_AppendsAFreshToken(t *testing.T) {
	got := MarkEnv([]string{"A=1"})
	if len(got) != 2 || got[0] != "A=1" || !tokenPattern.MatchString(envToken(got)) {
		t.Errorf("MarkEnv = %q, want [A=1 SQI_PROCESS_TREE=<32 hex>]", got)
	}
	if envToken(MarkEnv(nil)) == envToken(got) {
		t.Error("two MarkEnv calls drew the same token")
	}
}

func TestMarkEnv_ReplacesInheritedToken(t *testing.T) {
	got := MarkEnv([]string{"SQI_PROCESS_TREE=old", "B=2"})
	if m := markers(got); len(m) != 1 || m[0] == "SQI_PROCESS_TREE=old" {
		t.Errorf("marker entries = %q, want exactly one, not the inherited one", m)
	}
	if !slices.Contains(got, "B=2") {
		t.Errorf("MarkEnv dropped B=2: %q", got)
	}
}

func TestMarkEnv_NilStartsFromProcessEnvironment(t *testing.T) {
	t.Setenv("SQI_TEST_MARK", "x")
	got := MarkEnv(nil)
	if !slices.Contains(got, "SQI_TEST_MARK=x") {
		t.Errorf("MarkEnv(nil) lacks the process environment: %q", got)
	}
	if len(markers(got)) != 1 {
		t.Errorf("MarkEnv(nil) markers = %q, want one", markers(got))
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
	_, tree := startSleepTree(t)
	if err := tree.Track(nil, Label{}); err != nil {
		t.Errorf("Track(nil ledger) = %v, want nil", err)
	}
	if err := tree.Release(); err != nil {
		t.Errorf("Release = %v, want nil", err)
	}
}
