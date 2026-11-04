// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/report"
	"github.com/uberware/sqi/internal/loadtest/runner"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/target"
	"github.com/uberware/sqi/internal/loadtest/verify"
)

// fakeRunner stands in for the real runner: it returns a canned record and
// remembers the options it was given.
type fakeRunner struct {
	rec    *report.Record
	err    error
	points []report.SweepPoint

	got      runner.Options
	gotPath  string
	gotVals  []string
	runCalls int

	cleaned    target.Cleaned
	cleanErr   error
	gotCleanup *target.CleanupConfig
}

func (f *fakeRunner) Cleanup(_ context.Context, cfg target.CleanupConfig) (target.Cleaned, error) {
	f.gotCleanup = &cfg
	return f.cleaned, f.cleanErr
}

func (f *fakeRunner) Run(_ context.Context, o runner.Options) (*report.Record, error) {
	f.runCalls++
	f.got = o
	return f.rec, f.err
}

func (f *fakeRunner) Sweep(_ context.Context, o runner.Options, path string, values []string) ([]report.SweepPoint, error) {
	f.got, f.gotPath, f.gotVals = o, path, values
	return f.points, f.err
}

func record(complete, valid bool, failed ...string) *report.Record {
	rec := &report.Record{RunID: "r", Complete: complete, Valid: valid}
	if !valid {
		rec.InvalidReason = "generator CPU above 85%"
	}
	for _, name := range []string{verify.NameTerminal, verify.NameOutcomes, verify.NameSingleAttempt, verify.NameLogs, verify.NameWorkersConverge, verify.NameCaps} {
		ok := true
		for _, f := range failed {
			if f == name {
				ok = false
			}
		}
		rec.Invariants = append(rec.Invariants, verify.Result{Name: name, OK: ok})
	}
	return rec
}

func runCLI(t *testing.T, r Runner, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = execute(t.Context(), args, &out, &errOut, r)
	return code, out.String(), errOut.String()
}

func TestCLI_ExitCodes(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		rec        *report.Record
		err        error
		want       int
		wantStderr string
	}{
		{name: "valid run", args: []string{"run", "--scenario", "smoke"}, rec: record(true, true), want: exitOK},
		{name: "invariant failure", args: []string{"run", "--scenario", "smoke"}, rec: record(true, true, verify.NameLogs), want: exitInvariant},
		{name: "invalid run", args: []string{"run", "--scenario", "smoke"}, rec: record(true, false), want: exitInvalid},
		{name: "invalid but allowed", args: []string{"run", "--scenario", "smoke", "--allow-saturated"}, rec: record(true, false), want: exitOK},
		{name: "invalid and invariant failure: the invariant wins", args: []string{"run", "--scenario", "smoke"}, rec: record(true, false, verify.NameCaps), want: exitInvariant},
		{name: "interrupted run", args: []string{"run", "--scenario", "smoke"}, rec: record(false, true), want: exitInvalid},
		{name: "interrupted run is not excused by allow-saturated", args: []string{"soak", "--scenario", "smoke", "--allow-saturated"}, rec: record(false, true), want: exitInvalid},
		{name: "runner setup error", args: []string{"run", "--scenario", "smoke"}, err: errors.New("target: sqi-server not ready"), want: exitUsage, wantStderr: "sqi-server not ready"},
		{name: "teardown error after a clean run", args: []string{"run", "--scenario", "smoke"}, rec: record(true, true), err: errors.New("target: close"), want: exitUsage},
		{name: "teardown error does not hide an invariant failure", args: []string{"run", "--scenario", "smoke"}, rec: record(true, true, verify.NameTerminal), err: errors.New("target: close"), want: exitInvariant},
		{name: "unknown scenario", args: []string{"run", "--scenario", "no-such-scenario"}, want: exitUsage, wantStderr: strings.Join(scenario.BuiltinNames(), ", ")},
		{name: "bad override", args: []string{"run", "--scenario", "smoke", "--set", "workers[0].count=many"}, want: exitUsage, wantStderr: "workers[0].count"},
		{name: "override breaks validation", args: []string{"run", "--scenario", "smoke", "--duration", "1s"}, want: exitUsage, wantStderr: "warmup"},
		{name: "bad shard", args: []string{"run", "--scenario", "smoke", "--server", "http://h", "--nats", "nats://h", "--no-auth", "--shard", "2/2"}, want: exitUsage, wantStderr: "shard"},
		{name: "shard of a managed run", args: []string{"run", "--scenario", "smoke", "--shard", "0/2"}, want: exitUsage, wantStderr: "--shard needs --server"},
		{name: "bad run id", args: []string{"run", "--scenario", "smoke", "--run-id", "a:b"}, want: exitUsage, wantStderr: "run ID"},
		{name: "attach without nats", args: []string{"run", "--scenario", "smoke", "--server", "http://127.0.0.1:8080"}, want: exitUsage, wantStderr: "--nats"},
		{name: "attach with a managed flag", args: []string{"run", "--scenario", "smoke", "--server", "http://h", "--nats", "nats://h", "--no-auth", "--server-bin", "x"}, want: exitUsage, wantStderr: "--server-bin"},
		{name: "unknown flag", args: []string{"run", "--scenario", "smoke", "--frobnicate"}, want: exitUsage},
		{name: "postgres DSN on a sqlite run", args: []string{"run", "--scenario", "smoke", "--postgres-dsn", "postgres://x/postgres"}, want: exitUsage, wantStderr: "--set server.store=postgres"},
		{name: "unknown command", args: []string{"frobnicate"}, want: exitUsage},
		{name: "missing scenario", args: []string{"run"}, want: exitUsage, wantStderr: "--scenario"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRunner{rec: tc.rec, err: tc.err}
			code, _, stderr := runCLI(t, f, tc.args...)
			if code != tc.want {
				t.Errorf("exit code %d, want %d; stderr:\n%s", code, tc.want, stderr)
			}
			if tc.wantStderr != "" && !strings.Contains(stderr, tc.wantStderr) {
				t.Errorf("stderr does not mention %q:\n%s", tc.wantStderr, stderr)
			}
		})
	}
}

func TestCLI_RunOptions(t *testing.T) {
	f := &fakeRunner{rec: record(true, true)}
	out := t.TempDir()
	code, stdout, stderr := runCLI(t, f, "run", "--scenario", "smoke", "--set", "workers[0].count=7", "--duration", "30s",
		"--out", out, "--run-id", "fixed-id", "--sample", "2s", "--set", "server.store=postgres", "--postgres-dsn", "postgres://x/postgres")
	if code != exitOK {
		t.Fatalf("exit %d; stderr:\n%s", code, stderr)
	}
	o := f.got
	switch {
	case o.Scenario.Workers[0].Count != 7:
		t.Errorf("--set not applied: count %d", o.Scenario.Workers[0].Count)
	case o.Scenario.Duration.Duration != 30*time.Second:
		t.Errorf("--duration not applied: %s", o.Scenario.Duration)
	case o.RunID != "fixed-id" || o.OutDir != out:
		t.Errorf("run id %q out %q", o.RunID, o.OutDir)
	case o.Seed != runner.SeedFor("fixed-id"):
		t.Errorf("seed %d, want the one derived from the run ID", o.Seed)
	case o.Sample != 2*time.Second:
		t.Errorf("sample %s", o.Sample)
	case o.Mode != report.ModeLoad:
		t.Errorf("mode %q", o.Mode)
	case o.Managed == nil || o.Managed.PostgresDSN != "postgres://x/postgres" || o.Attach != nil:
		t.Errorf("managed %+v attach %+v", o.Managed, o.Attach)
	}
	if !strings.Contains(stdout+stderr, "fixed-id") || !strings.Contains(stdout+stderr, "seed") {
		t.Errorf("the run ID and seed are not printed at start:\n%s%s", stdout, stderr)
	}
}

func TestCLI_SoakAndAttach(t *testing.T) {
	f := &fakeRunner{rec: record(true, true)}
	code, _, stderr := runCLI(t, f, "soak", "--scenario", "soak-steady", "--duration", "6h",
		"--server", "https://farm:8443", "--nats", "nats://farm:4222", "--api-key", "k", "--join-token", "tok", "--keep", "--seed", "42", "--ca-file", "ca.crt",
		"--shard", "1/3")
	if code != exitOK {
		t.Fatalf("exit %d; stderr:\n%s", code, stderr)
	}
	o := f.got
	if o.Mode != report.ModeSoak || o.Seed != 42 || o.Managed != nil || o.Attach == nil || o.Shard != (report.ShardSpec{Index: 1, Of: 3}) {
		t.Fatalf("options %+v", o)
	}
	a := o.Attach
	if a.HTTPURL != "https://farm:8443" || a.NATSURL != "nats://farm:4222" || a.APIKey != "k" || a.JoinToken != "tok" || !a.Keep || a.NoAuth || a.TLS.CAFile != "ca.crt" {
		t.Errorf("attach config %+v", a)
	}
	if o.Scenario.Duration.Duration != 6*time.Hour {
		t.Errorf("a soak accepts any duration; got %s", o.Scenario.Duration)
	}
}

func TestCLI_Sweep(t *testing.T) {
	out := t.TempDir()
	f := &fakeRunner{points: []report.SweepPoint{{Value: "1", Rec: record(true, true)}, {Value: "2", Rec: record(true, true, verify.NameCaps)}}}
	code, stdout, stderr := runCLI(t, f, "sweep", "--scenario", "workers-ramp", "--vary", "workers[0].count=500,1000", "--out", out, "--run-id", "sw")
	if code != exitOK {
		t.Fatalf("exit %d; stderr:\n%s", code, stderr)
	}
	if f.gotPath != "workers[0].count" || strings.Join(f.gotVals, ",") != "500,1000" || f.got.RunID != "sw" {
		t.Errorf("sweep got path %q values %v run id %q", f.gotPath, f.gotVals, f.got.RunID)
	}
	if !strings.Contains(stdout, filepath.Join(out, "sw")) {
		t.Errorf("the sweep directory is not printed:\n%s", stdout)
	}

	for _, args := range [][]string{
		{"sweep", "--scenario", "workers-ramp"},
		{"sweep", "--scenario", "workers-ramp", "--vary", "workers[0].count"},
		{"sweep", "--scenario", "workers-ramp", "--vary", "workers[0].count=5,x"},
		{"sweep", "--scenario", "workers-ramp", "--vary", "workers[0].count=5", "--server", "http://h", "--nats", "nats://h"},
	} {
		if code, _, _ := runCLI(t, &fakeRunner{}, args...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}
}

func TestCLI_Scenarios(t *testing.T) {
	code, stdout, _ := runCLI(t, &fakeRunner{}, "scenarios")
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	for _, name := range scenario.BuiltinNames() {
		if !strings.Contains(stdout, name) {
			t.Errorf("scenarios does not list %s", name)
		}
	}
	if !strings.Contains(stdout, "all axes, tiny") {
		t.Errorf("scenarios does not show descriptions:\n%s", stdout)
	}

	code, stdout, _ = runCLI(t, &fakeRunner{}, "scenarios", "--show", "smoke")
	if code != exitOK {
		t.Fatalf("--show exit %d", code)
	}
	sc, err := scenario.Parse([]byte(stdout))
	if err != nil {
		t.Fatalf("--show printed YAML that does not parse back: %v\n%s", err, stdout)
	}
	want, err := scenario.Builtin("smoke")
	if err != nil {
		t.Fatal(err)
	}
	if sc.Name != want.Name || sc.Duration != want.Duration || sc.Jobs[0].Task.Duration != want.Jobs[0].Task.Duration {
		t.Errorf("--show round trip differs:\n%s", stdout)
	}

	code, _, stderr := runCLI(t, &fakeRunner{}, "scenarios", "--show", "nope")
	if code != exitUsage || !strings.Contains(stderr, "smoke") {
		t.Errorf("--show nope: exit %d, stderr %q", code, stderr)
	}
}

func TestCLI_CompareAndMerge(t *testing.T) {
	dir := t.TempDir()
	a, b := record(true, true), record(true, true, verify.NameCaps)
	a.Counters = map[string]int64{report.CounterTasksTerminal: 100}
	b.Counters = map[string]int64{report.CounterTasksTerminal: 200}
	a.Shard, b.Shard = "0/2", "1/2"
	a.Started, b.Started = time.Unix(0, 0), time.Unix(0, 0)
	a.Ended, b.Ended = time.Unix(100, 0), time.Unix(100, 0)
	pa, pb := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := a.Write(pa); err != nil {
		t.Fatal(err)
	}
	if err := b.Write(pb); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI(t, &fakeRunner{}, "compare", filepath.Join(pa, "run.json"), pb)
	if code != exitOK || !strings.Contains(stdout, report.CounterTasksTerminal) {
		t.Errorf("compare exit %d stdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if strings.Contains(stdout, "WARNING") {
		t.Errorf("compare of two valid, complete runs warns:\n%s", stdout)
	}
	inv := record(true, false)
	inv.RunID = "sat"
	pi := filepath.Join(dir, "invalid")
	if err := inv.Write(pi); err != nil {
		t.Fatal(err)
	}
	if _, stdout, _ := runCLI(t, &fakeRunner{}, "compare", pa, pi); !strings.Contains(stdout, "WARNING: B (sat) is INVALID") {
		t.Errorf("compare against an INVALID run does not warn:\n%s", stdout)
	}
	if code, _, _ := runCLI(t, &fakeRunner{}, "compare", pa); code != exitUsage {
		t.Errorf("compare with one record: exit %d, want %d", code, exitUsage)
	}

	merged := filepath.Join(dir, "merged")
	code, _, stderr = runCLI(t, &fakeRunner{}, "merge", merged, pa, pb)
	if code != exitInvariant {
		t.Errorf("merge of a shard with a failed invariant: exit %d, want %d; stderr:\n%s", code, exitInvariant, stderr)
	}
	m, err := report.Read(merged)
	if err != nil {
		t.Fatalf("merged record: %v", err)
	}
	if m.Counters[report.CounterTasksTerminal] != 300 {
		t.Errorf("merged tasks.terminal = %d, want 300", m.Counters[report.CounterTasksTerminal])
	}
}

func TestVerdict(t *testing.T) {
	cases := []struct {
		name  string
		rec   *report.Record
		allow bool
		want  int
	}{
		{"clean", record(true, true), false, exitOK},
		{"failed", record(true, true, verify.NameTerminal), false, exitInvariant},
		{"invalid", record(true, false), false, exitInvalid},
		{"invalid allowed", record(true, false), true, exitOK},
		{"incomplete", record(false, true), true, exitInvalid},
		{"both", record(true, false, verify.NameLogs), false, exitInvariant},
		{"nil", nil, false, exitUsage},
		{"no submission accepted", noneAccepted(), false, exitInvalid},
		{"no submission accepted is not excused by allow-saturated", noneAccepted(), true, exitInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := verdict(tc.rec, tc.allow); got != tc.want {
				t.Errorf("verdict = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCLI_Cleanup(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		cleaned    target.Cleaned
		err        error
		want       int
		wantOut    string
		wantCalled bool
	}{
		{
			name: "removes a run's leftovers", args: []string{"cleanup", "--server", "https://farm:8443", "--run-id", "big-1", "--api-key", "k", "--ca-file", "ca.crt"},
			cleaned: target.Cleaned{Farms: []string{"load-big-1"}, Pools: []string{"load_big_1_arnold"}},
			want:    exitOK, wantOut: "removed 1 farms (load-big-1) and 1 usage pools (load_big_1_arnold)", wantCalled: true,
		},
		{name: "nothing to remove", args: []string{"cleanup", "--server", "http://h", "--run-id", "r"}, want: exitOK, wantOut: "removed 0 farms (none)", wantCalled: true},
		{name: "a failed cleanup", args: []string{"cleanup", "--server", "http://h", "--run-id", "r"}, err: errors.New("target: list farms: HTTP 403"), want: exitUsage, wantCalled: true},
		{name: "no server", args: []string{"cleanup", "--run-id", "r"}, want: exitUsage},
		{name: "no run id", args: []string{"cleanup", "--server", "http://h"}, want: exitUsage},
		{name: "bad run id", args: []string{"cleanup", "--server", "http://h", "--run-id", "a:b"}, want: exitUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRunner{cleaned: tc.cleaned, cleanErr: tc.err}
			code, stdout, stderr := runCLI(t, f, tc.args...)
			if code != tc.want {
				t.Errorf("exit %d, want %d; stderr:\n%s", code, tc.want, stderr)
			}
			if !strings.Contains(stdout, tc.wantOut) {
				t.Errorf("stdout does not say %q:\n%s", tc.wantOut, stdout)
			}
			if (f.gotCleanup != nil) != tc.wantCalled {
				t.Fatalf("cleanup called = %v, want %v", f.gotCleanup != nil, tc.wantCalled)
			}
		})
	}
	f := &fakeRunner{}
	runCLI(t, f, "cleanup", "--server", "https://farm:8443", "--run-id", "big-1", "--api-key", "k", "--ca-file", "ca.crt")
	if c := f.gotCleanup; c == nil || c.HTTPURL != "https://farm:8443" || c.RunID != "big-1" || c.APIKey != "k" || c.TLS.CAFile != "ca.crt" {
		t.Errorf("cleanup config %+v", c)
	}
}

// noneAccepted is a complete record whose scenario submits jobs but whose
// server accepted none: INVALID, every invariant holding vacuously.
func noneAccepted() *report.Record {
	rec := record(true, false)
	rec.InvalidReason = "no submission accepted"
	rec.Scenario = &scenario.Scenario{Jobs: []scenario.JobClass{{Name: "shot"}}}
	rec.Counters = map[string]int64{report.CounterJobsSubmitted: 0, report.CounterSubmitStatusPrefix + "403": 50}
	return rec
}
