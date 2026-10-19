// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/worker/capabilities"
	"github.com/uberware/sqi/internal/worker/commands"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/session"
)

var (
	errNotFound = errors.New("not found")
	noPath      = func(string) (string, error) { return "", errNotFound }
)

func onPath(found map[string]string) func(string) (string, error) {
	return func(name string) (string, error) {
		if p, ok := found[name]; ok {
			return p, nil
		}
		return "", errNotFound
	}
}

func reportFor(t *testing.T, plan commandPlan, name string) commandReport {
	t.Helper()
	for _, r := range plan.Reports {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no report for %q in %+v", name, plan.Reports)
	return commandReport{}
}

var nukeFound = []capabilities.DetectedCommand{{Name: "nuke", Path: "/opt/Nuke17.1v2/Nuke17.1", Version: "17.1"}}

func TestPlanCommands_ADetectedCommandFillsTheGap(t *testing.T) {
	plan := planCommands(nil, nukeFound, noPath)

	got, ok := plan.Table["nuke"]
	if !ok || got.Path != "/opt/Nuke17.1v2/Nuke17.1" || len(got.Args) != 0 {
		t.Fatalf("table[nuke] = %+v (present %v), want the detected path and no args", got, ok)
	}
	if r := reportFor(t, plan, "nuke"); r.Source != sourceDetected || r.Version != "17.1" {
		t.Errorf("report = %+v, want source %q, version 17.1", r, sourceDetected)
	}
}

func TestPlanCommands_TheOperatorsEntryBeatsADetectedOne(t *testing.T) {
	operator := commands.Table{"nuke": {Path: "/studio/bin/nuke-wrapper", Args: []string{"--nc"}}}

	plan := planCommands(operator, nukeFound, noPath)

	if got := plan.Table["nuke"]; got.Path != "/studio/bin/nuke-wrapper" || !slices.Equal(got.Args, []string{"--nc"}) {
		t.Errorf("table[nuke] = %+v, want the operator's entry untouched", got)
	}
	if r := reportFor(t, plan, "nuke"); r.Source != sourceConfig {
		t.Errorf("source = %q, want %q", r.Source, sourceConfig)
	}
}

// A studio that already provides a nuke command on PATH has set up what it
// wants; the worker must not shadow it with its own pick of install.
func TestPlanCommands_ACommandAlreadyOnPATHIsLeftAlone(t *testing.T) {
	plan := planCommands(nil, nukeFound, onPath(map[string]string{"nuke": "/studio/bin/nuke"}))

	if _, ok := plan.Table["nuke"]; ok {
		t.Errorf("a default was registered over a nuke on PATH: %+v", plan.Table["nuke"])
	}
	r := reportFor(t, plan, "nuke")
	if r.Source != sourcePATH || r.Path != "/studio/bin/nuke" {
		t.Errorf("report = %+v, want source %q resolving to /studio/bin/nuke", r, sourcePATH)
	}
}

func TestPlanCommands_OperatorEntriesAreReportedEvenWhenNothingIsDetected(t *testing.T) {
	plan := planCommands(commands.Table{"tool": {Path: "/opt/tool"}}, nil, noPath)

	if r := reportFor(t, plan, "tool"); r.Source != sourceConfig || r.Path != "/opt/tool" {
		t.Errorf("report = %+v", r)
	}
}

func TestPlanCommands_DoesNotShareTheOperatorsTable(t *testing.T) {
	operator := commands.Table{"tool": {Path: "/opt/tool"}}

	plan := planCommands(operator, nukeFound, noPath)
	plan.Table["added"] = commands.Command{Path: "/x"}

	if _, ok := operator["added"]; ok || len(operator) != 1 {
		t.Errorf("the operator's table was changed: %v", operator)
	}
}

func TestPlanCommands_ReportsAreSortedByName(t *testing.T) {
	plan := planCommands(commands.Table{"b": {Path: "/b"}, "a": {Path: "/a"}}, nukeFound, noPath)

	var names []string
	for _, r := range plan.Reports {
		names = append(names, r.Name)
	}
	if !slices.Equal(names, []string{"a", "b", "nuke"}) {
		t.Errorf("report order = %v, want [a b nuke]", names)
	}
}

// TestCommandPlanOption_ReachesTheSessions is the wiring test for worker.commands:
// the option a plan returns, handed to session.NewManager the way start.go does,
// must make the sessions it creates resolve the planned name.
func TestCommandPlanOption_ReachesTheSessions(t *testing.T) {
	plan := planCommands(commands.Table{"nuke": {Path: "/opt/nuke", Args: []string{"--nc"}}}, nil, noPath)
	mgr := session.NewManager(
		filepath.Join(t.TempDir(), "sessions"), false, isolation.NewFake(nil),
		workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, slog.New(slog.DiscardHandler),
		plan.option(slog.New(slog.DiscardHandler)),
	)
	s, err := mgr.Create(context.Background(), &protocol.AssignMsg{JobID: "job-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer mgr.Cleanup(context.Background(), s, false)

	got := s.ResolveAction(&protocol.Action{Command: "nuke", Args: []string{"-x"}})

	if got.Command != "/opt/nuke" || !slices.Equal(got.Args, []string{"--nc", "-x"}) {
		t.Errorf("ResolveAction = %q %q, want /opt/nuke [--nc -x]", got.Command, got.Args)
	}
}

func TestCommandPlanOption_WarnsAboutAnOperatorTargetThatDoesNotExist(t *testing.T) {
	present := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(present, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "gone")
	plan := planCommands(commands.Table{"present": {Path: present}, "missing": {Path: missing}}, nil, noPath)
	var logs bytes.Buffer

	plan.option(slog.New(slog.NewTextHandler(&logs, nil)))

	var warnings []string
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if strings.Contains(line, "level=WARN") {
			warnings = append(warnings, line)
		}
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "command=missing") {
		t.Errorf("want exactly one warning, for the missing target; got %q", warnings)
	}
}

// fakeCheckEnv is a capabilities.CheckEnv with a fixed set of glob matches.
type fakeCheckEnv struct {
	goos  string
	globs map[string][]string
}

func (fakeCheckEnv) LookPath(string) (string, bool) { return "", false }
func (f fakeCheckEnv) Glob(p string) []string       { return f.globs[p] }
func (fakeCheckEnv) Getenv(string) (string, bool)   { return "", false }
func (fakeCheckEnv) RegistryExists(string) bool     { return false }
func (f fakeCheckEnv) GOOS() string                 { return f.goos }

// TestDetectCommandPlan_BuiltInNukeBecomesTheNukeCommand runs the real built-in
// detector through to a plan: an install of Nuke on a worker with no nuke
// command anywhere ends up as the default for "nuke".
func TestDetectCommandPlan_BuiltInNukeBecomesTheNukeCommand(t *testing.T) {
	ds, err := capabilities.BuiltinDetectors()
	if err != nil {
		t.Fatal(err)
	}
	env := fakeCheckEnv{goos: "windows", globs: map[string][]string{}}
	for _, d := range ds {
		if d.Tag == "nuke" {
			for _, g := range d.Command.Globs {
				env.globs[g.PathGlob] = []string{`C:\Program Files\Nuke17.1v2\Nuke17.1.exe`}
			}
		}
	}

	plan, err := detectCommandPlan(workerconfig.Default(), env, noPath)
	if err != nil {
		t.Fatalf("detectCommandPlan: %v", err)
	}

	if got := plan.Table["nuke"]; got.Path != `C:\Program Files\Nuke17.1v2\Nuke17.1.exe` {
		t.Errorf("table[nuke] = %+v, want the detected install", got)
	}
}

func TestCommandPlan_WriteListsEachCommandAndWhereItCameFrom(t *testing.T) {
	plan := planCommands(commands.Table{"tool": {Path: "/opt/tool", Args: []string{"--nc"}}}, nukeFound, noPath)
	var buf bytes.Buffer

	if err := plan.write(&buf); err != nil {
		t.Fatalf("write: %v", err)
	}

	out := buf.String()
	for _, want := range []string{"COMMAND", "tool", "/opt/tool", "--nc", sourceConfig, "nuke", "/opt/Nuke17.1v2/Nuke17.1", sourceDetected, "17.1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestCapabilitiesCmd_PrintsTheCommandTable(t *testing.T) {
	t.Cleanup(func() { persistentFlags.ConfigFile = "" })
	cfgPath := filepath.Join(t.TempDir(), "sqi-worker.yaml")
	tool := filepath.ToSlash(filepath.Join(t.TempDir(), "mytool"))
	if err := os.WriteFile(cfgPath, []byte("worker:\n  commands:\n    mytool:\n      path: \""+tool+"\"\n      args: [\"--flag\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	prepareRoot([]string{"--config", cfgPath, "capabilities"})
	out := captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Errorf("Execute(capabilities): %v", err)
		}
	})

	for _, want := range []string{"COMMAND", "mytool", tool, "--flag", sourceConfig} {
		if !strings.Contains(out, want) {
			t.Errorf("capabilities output lacks %q:\n%s", want, out)
		}
	}
}
