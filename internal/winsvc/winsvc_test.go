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
	got := Run("x", func(ctx context.Context) error {
		if ctx.Err() != nil {
			t.Errorf("ctx already done: %v", ctx.Err())
		}
		if ServiceName(ctx) != "" || StopReason(ctx) != "" {
			t.Errorf("console ctx carries service values")
		}
		return want
	})
	if !errors.Is(got, want) {
		t.Fatalf("Run = %v, want %v", got, want)
	}
}

func TestServiceContextAccessors(t *testing.T) {
	r := &stopReason{}
	ctx := withService(context.Background(), "sqi-worker-2", r)
	if ServiceName(ctx) != "sqi-worker-2" {
		t.Fatalf("ServiceName = %q", ServiceName(ctx))
	}
	if StopReason(ctx) != "" {
		t.Fatalf("StopReason before stop = %q", StopReason(ctx))
	}
	r.set("service stop")
	if StopReason(ctx) != "service stop" {
		t.Fatalf("StopReason = %q", StopReason(ctx))
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
	svcCtx := withService(context.Background(), "sqi-server", &stopReason{})

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
	ctx := withService(context.Background(), "sqi-server", &stopReason{})
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
