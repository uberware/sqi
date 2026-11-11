// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/marker"
	"github.com/uberware/sqi/internal/loadtest/scenario"
)

var (
	buildOnce sync.Once
	noopPath  string
	errBuild  error
	buildDir  string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

// noopBin builds this command once per test process.
func noopBin(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the no-op command; skipped with -short")
	}
	buildOnce.Do(func() {
		buildDir, errBuild = os.MkdirTemp("", "sqi-load-noop-") //nolint:usetesting // shared across tests; TestMain removes it
		if errBuild != nil {
			return
		}
		name := "noop"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		noopPath = filepath.Join(buildDir, name)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		out, err := exec.CommandContext(ctx, "go", "build", "-o", noopPath, ".").CombinedOutput()
		if err != nil {
			errBuild = &buildError{err: err, out: string(out)}
		}
	})
	if errBuild != nil {
		t.Fatalf("build the no-op command: %v", errBuild)
	}
	return noopPath
}

type buildError struct {
	err error
	out string
}

func (e *buildError) Error() string { return e.err.Error() + "\n" + e.out }

// writeConfig writes a noop.json with one class, "c", and returns the
// config's path and its marker directory.
func writeConfig(t *testing.T, class marker.NoopClass) (path, markers string) {
	t.Helper()
	dir := t.TempDir()
	markers = filepath.Join(dir, "markers")
	path = filepath.Join(dir, "noop.json")
	b, err := json.Marshal(marker.NoopConfig{MarkerDir: markers, Classes: map[string]marker.NoopClass{"c": class}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, markers
}

// taskArgs are the arguments a real task of class c gets.
func taskArgs(config string) []string {
	return []string{marker.ConfigArg(config), "-x", scenario.KeyArg("job-1/step-1/1"), scenario.ClassArg("c")}
}

func TestNoop_DefaultUnchanged(t *testing.T) {
	bin := noopBin(t)
	markers := filepath.Join(t.TempDir(), "markers")
	cmd := exec.CommandContext(t.Context(), bin, "-x", scenario.KeyArg("j/step-1/1"), scenario.ClassArg("c"))
	cmd.Env = append(os.Environ(), "SQILOAD_MARKER_DIR="+markers, "SQILOAD_INCARNATION=0.0")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("noop: %v", err)
	}
	if string(out) != "sqi-load noop task\n" {
		t.Errorf("stdout = %q, want %q", out, "sqi-load noop task\n")
	}
	if _, err := os.Stat(markers); !os.IsNotExist(err) {
		t.Errorf("marker directory exists after a default run (stat err %v)", err)
	}
}

func TestNoop_ClassWithoutRealTaskIsDefault(t *testing.T) {
	bin := noopBin(t)
	config, markers := writeConfig(t, marker.NoopClass{DurationMS: 100})
	cmd := exec.CommandContext(t.Context(), bin, marker.ConfigArg(config), scenario.KeyArg("k"), scenario.ClassArg("other"))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("noop: %v", err)
	}
	if string(out) != "sqi-load noop task\n" {
		t.Errorf("stdout = %q, want the default line", out)
	}
	if _, err := os.Stat(markers); !os.IsNotExist(err) {
		t.Errorf("marker directory exists for a class without real_task (stat err %v)", err)
	}
}

func TestNoop_RealTaskWritesMarkers(t *testing.T) {
	bin := noopBin(t)
	config, markers := writeConfig(t, marker.NoopClass{DurationMS: 600, LogEveryMS: 200, ChattyChild: true, SilentChild: true})
	cmd := exec.CommandContext(t.Context(), bin, taskArgs(config)...)
	cmd.Env = append(os.Environ(), "SQILOAD_INCARNATION=2.1")
	start := time.Now()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("noop: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 600*time.Millisecond {
		t.Errorf("noop took %s, want at least its 600ms duration", elapsed)
	}
	execs, skipped, err := marker.ReadAll(markers)
	if err != nil || skipped != 0 || len(execs) != 3 {
		t.Fatalf("ReadAll = %+v, skipped %d, %v; want main, chatty and silent", execs, skipped, err)
	}
	roles := map[string]marker.Exec{}
	for _, e := range execs {
		roles[e.Role] = e
		if e.Key != "job-1/step-1/1" || e.Incarnation != "2.1" || e.End == nil || e.ExitCode != 0 {
			t.Errorf("exec %+v: want key job-1/step-1/1, incarnation 2.1, ended with 0", e)
		}
		if runtime.GOOS != "windows" && e.PGID == 0 {
			t.Errorf("exec %+v has no process group", e)
		}
	}
	if len(roles) != 3 {
		t.Fatalf("roles %v, want main, chatty and silent", roles)
	}
	if runtime.GOOS != "windows" {
		mainPGID := roles[marker.RoleMain].PGID
		if roles[marker.RoleChatty].PGID != mainPGID || roles[marker.RoleSilent].PGID != mainPGID {
			t.Errorf("children's groups %d, %d differ from the task's %d", roles[marker.RoleChatty].PGID, roles[marker.RoleSilent].PGID, mainPGID)
		}
	}
	// The main process prints a line every 200ms, the chatty child its own.
	if n := countLines(string(out), "sqi-load noop task"); n < 3 {
		t.Errorf("main printed %d lines across 600ms at one per 200ms, want at least 3:\n%s", n, out)
	}
	if n := countLines(string(out), "sqi-load noop chatty"); n < 2 {
		t.Errorf("chatty printed %d lines, want at least 2:\n%s", n, out)
	}
	outs, err := marker.Outputs(markers)
	if err != nil {
		t.Fatal(err)
	}
	lines := outs["job-1/step-1/1"]
	if len(lines) < 2 {
		t.Fatalf("silent child wrote %d output lines, want at least 2", len(lines))
	}
	for _, l := range lines {
		if l.ExecID != roles[marker.RoleSilent].ID {
			t.Errorf("output line by %s, want the silent child %s", l.ExecID, roles[marker.RoleSilent].ID)
		}
	}
	if _, err := os.Stat(filepath.Join(markers, "logs", roles[marker.RoleSilent].ID+".log")); err != nil {
		t.Errorf("silent child's log file: %v", err)
	}
}

func countLines(s, prefix string) int {
	n := 0
	for line := range strings.Lines(s) {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}
