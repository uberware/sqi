// SPDX-License-Identifier: AGPL-3.0-or-later

package main_test

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	stubrecord "github.com/uberware/sqi/test/stubproc/record"
)

// buildStub compiles the stub into dir and returns its path.
//
// The binary keeps the platform's executable suffix: Windows will not exec a
// file without one, and main strips the extension before recording the name,
// so a "kick.exe" here still records itself as "kick".
func buildStub(t *testing.T, dir, name string) string {
	t.Helper()
	out := filepath.Join(dir, name+exeSuffix())
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", out, "github.com/uberware/sqi/test/stubproc")
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build stub: %v\n%s", err, combined)
	}
	return out
}

func TestStub_RecordsArgvCwdAndStdin(t *testing.T) {
	dir := t.TempDir()
	bin := buildStub(t, dir, "kick")
	record := filepath.Join(dir, "record.jsonl")

	cmd := exec.CommandContext(t.Context(), bin, "-i", "scene.ass", "-o", "out.exr")
	cmd.Env = append(os.Environ(), "SQI_STUB_RECORD="+record)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader("scene.open foo\nrender.animation\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("stub run: %v\n%s", err, out)
	}

	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var rec stubrecord.Record
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &rec); err != nil {
		t.Fatalf("decode record: %v (raw %q)", err, data)
	}
	if rec.Command != "kick" {
		t.Errorf("command = %q, want %q -- the stub must record the NAME it was invoked as", rec.Command, "kick")
	}
	if len(rec.Args) != 4 || rec.Args[1] != "scene.ass" {
		t.Errorf("args = %v", rec.Args)
	}
	if !strings.Contains(rec.Stdin, "render.animation") {
		t.Errorf("stdin = %q, want the piped commands -- a stdin-driven CLI has no argv to snapshot", rec.Stdin)
	}
}

func TestStub_ExitsNonZeroOnRequest(t *testing.T) {
	dir := t.TempDir()
	bin := buildStub(t, dir, "renderer")
	cmd := exec.CommandContext(t.Context(), bin)
	cmd.Env = append(os.Environ(), "SQI_STUB_EXIT=3")
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("run: err = %v, want an ExitError", err)
	}
	if code := exitErr.ExitCode(); code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
}

func TestStub_ExitOnSelectsByName(t *testing.T) {
	dir := t.TempDir()
	ffmpeg := buildStub(t, dir, "ffmpeg")
	other := buildStub(t, dir, "kick")
	env := append(os.Environ(), "SQI_STUB_EXIT_ON=ffmpeg=4")

	cmd := exec.CommandContext(t.Context(), ffmpeg)
	cmd.Env = env
	var exitErr *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &exitErr) || exitErr.ExitCode() != 4 {
		t.Errorf("ffmpeg: err = %v, want exit 4", err)
	}

	cmd = exec.CommandContext(t.Context(), other)
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		t.Errorf("kick: err = %v, want success (EXIT_ON named ffmpeg only)", err)
	}
}

func TestStub_TouchesOutputFileFromArg(t *testing.T) {
	dir := t.TempDir()
	bin := buildStub(t, dir, "renderer")
	target := filepath.Join(dir, "frame.0001.exr")

	cmd := exec.CommandContext(t.Context(), bin, "-o", target)
	cmd.Env = append(os.Environ(), "SQI_STUB_TOUCH_ARG=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("stub run: %v\n%s", err, out)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("stub did not create the file named by arg 1: %v", err)
	}
}

func TestStub_EmitsProgressAndStatus(t *testing.T) {
	dir := t.TempDir()
	bin := buildStub(t, dir, "renderer")
	cmd := exec.CommandContext(t.Context(), bin)
	cmd.Env = append(os.Environ(), "SQI_STUB_PROGRESS=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("stub run: %v\n%s", err, out)
	}
	for _, want := range []string{"openjd_status:", "openjd_progress: 100"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestStub_SleepsOnRequest(t *testing.T) {
	dir := t.TempDir()
	bin := buildStub(t, dir, "renderer")
	cmd := exec.CommandContext(t.Context(), bin)
	cmd.Env = append(os.Environ(), "SQI_STUB_SLEEP=300ms")
	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("stub run: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Errorf("returned after %s, want at least ~300ms", elapsed)
	}
}

// exeSuffix is ".exe" on Windows and "" elsewhere.
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
