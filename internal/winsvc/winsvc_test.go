// SPDX-License-Identifier: AGPL-3.0-or-later

package winsvc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun_ConsolePassesLiveContextAndReturnsFnError(t *testing.T) {
	if IsService() {
		t.Skip("running under the SCM")
	}
	want := errors.New("boom")
	configPath := "relative.yaml"
	got := Run("x", &configPath, func(ctx context.Context) error {
		if ctx.Err() != nil {
			t.Errorf("ctx already done: %v", ctx.Err())
		}
		if ServiceName(ctx) != "" {
			t.Errorf("console ctx carries a service name")
		}
		return want
	})
	if configPath != "relative.yaml" {
		t.Errorf("console Run rewrote --config to %q", configPath)
	}
	if !errors.Is(got, want) {
		t.Fatalf("Run = %v, want %v", got, want)
	}
}

func TestServiceName(t *testing.T) {
	if got := ServiceName(withService(context.Background(), "sqi-worker-2")); got != "sqi-worker-2" {
		t.Fatalf("ServiceName = %q", got)
	}
}

// TestRequireConfigFile pins that a service given no --config refuses to run:
// the config search path it would otherwise fall back to includes locations
// any local user can create. A service with --config is unaffected.
func TestRequireConfigFile(t *testing.T) {
	if err := requireConfigFile("sqi-worker", `C:\ProgramData\sqi\sqi-worker.yaml`); err != nil {
		t.Fatalf("with --config = %v, want nil", err)
	}
	err := requireConfigFile("sqi-worker", "")
	if err == nil {
		t.Fatal("without --config = nil, want a refusal")
	}
	for _, want := range []string{"--config", `\etc\sqi`, "sqi-worker service install"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestDefaultLogPath_UnderProgramData(t *testing.T) {
	pd := t.TempDir()
	t.Setenv("ProgramData", pd)
	want := filepath.Join(pd, "sqi", "logs", "sqi-worker-2.log")
	if got := DefaultLogPath("sqi-worker-2"); got != want {
		t.Fatalf("DefaultLogPath = %q, want %q", got, want)
	}
}

func TestResolveLogFile(t *testing.T) {
	pd := t.TempDir()
	t.Setenv("ProgramData", pd)
	console := context.Background()
	svcCtx := withService(context.Background(), "sqi-server")

	if got, err := ResolveLogFile(console, ""); err != nil || got != "" {
		t.Errorf("console, unset = %q, %v; want stderr (\"\")", got, err)
	}
	got, err := ResolveLogFile(console, "x.log")
	if err != nil {
		t.Fatalf("console, set: %v", err)
	}
	if got != "x.log" {
		t.Errorf("console, set = %q", got)
	}
	got, err = ResolveLogFile(svcCtx, "x.log")
	if err != nil {
		t.Fatalf("service, set: %v", err)
	}
	if got != "x.log" {
		t.Errorf("service, set = %q; configured value must win", got)
	}
}

func TestResolveLogFile_ServiceModeCreatesDirectory(t *testing.T) {
	pd := filepath.Join(t.TempDir(), "fresh-programdata")
	t.Setenv("ProgramData", pd)
	ctx := withService(context.Background(), "sqi-server")
	got, err := ResolveLogFile(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(pd, "sqi", "logs", "sqi-server.log") {
		t.Fatalf("path = %q", got)
	}
	if info, err := os.Stat(filepath.Dir(got)); err != nil || !info.IsDir() {
		t.Fatalf("log directory not created: %v", err)
	}
}

func TestAppendTrace_WritesJSONLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "svc.log")
	if err := appendTrace(path, errors.New("load config: bad yaml")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"level":"ERROR"`, `"msg":"service exited with error"`, `"error":"load config: bad yaml"`} {
		if !strings.Contains(s, want) {
			t.Errorf("trace %q missing %s", s, want)
		}
	}
	if !strings.HasSuffix(s, "\n") {
		t.Error("trace line not newline-terminated")
	}
}

// TestFallbackTracePath pins the fallback trace's name: the service name with
// anything unsafe in a file name replaced, so it stays a plain file in dir.
func TestFallbackTracePath(t *testing.T) {
	dir := t.TempDir()
	for name, want := range map[string]string{
		"sqi-worker":   "sqi-worker.trace.log",
		"sqi_worker.2": "sqi_worker.2.trace.log",
		`a:b*c?"<>|`:   "a_b_c_____.trace.log",
		"..":           "...trace.log",
	} {
		got := fallbackTracePath(dir, name)
		if got != filepath.Join(dir, want) {
			t.Errorf("fallbackTracePath(%q) = %q, want %q", name, got, filepath.Join(dir, want))
		}
		if filepath.Dir(got) != dir {
			t.Errorf("fallbackTracePath(%q) = %q leaves %s", name, got, dir)
		}
	}
}

func TestWorkDir(t *testing.T) {
	pd := t.TempDir()
	t.Setenv("ProgramData", pd)
	if got := workDir(""); got != filepath.Join(pd, "sqi") {
		t.Errorf("workDir(\"\") = %q", got)
	}
	cfg := filepath.Join(t.TempDir(), "conf", "sqi-server.yaml")
	if got := workDir(cfg); got != filepath.Dir(cfg) {
		t.Errorf("workDir(%q) = %q", cfg, got)
	}
}
