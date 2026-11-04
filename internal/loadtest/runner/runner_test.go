// SPDX-License-Identifier: AGPL-3.0-or-later

package runner

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/driver"
	"github.com/uberware/sqi/internal/loadtest/internal/proctest"
	"github.com/uberware/sqi/internal/loadtest/report"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
	"github.com/uberware/sqi/internal/loadtest/target"
	"github.com/uberware/sqi/internal/loadtest/verify"
)

var (
	binOnce                     sync.Once
	binDir                      string
	serverBin, workerBin, noBin string
	errBuild                    error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if binDir != "" {
		os.RemoveAll(binDir)
	}
	os.Exit(code)
}

// binaries builds sqi-server, sqi-worker and the no-op command once per test
// process.
func binaries(t *testing.T) *target.ManagedConfig {
	t.Helper()
	binOnce.Do(func() {
		// Shared by every test of the process; TestMain removes it.
		binDir, errBuild = os.MkdirTemp("", "sqi-load-runner-bin-") //nolint:usetesting // shared across tests, see above
		if errBuild != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		serverBin, workerBin, noBin, errBuild = target.BuildBinaries(ctx, binDir)
	})
	if errBuild != nil {
		t.Fatalf("BuildBinaries: %v", errBuild)
	}
	return &target.ManagedConfig{ServerBin: serverBin, WorkerBin: workerBin, NoopBin: noBin}
}

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func smokeScenario(t *testing.T, duration string) *scenario.Scenario {
	t.Helper()
	sc, err := scenario.Builtin("smoke")
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.Set("duration", duration); err != nil {
		t.Fatal(err)
	}
	sc.ApplyDefaults()
	if err := sc.Validate(scenario.TargetManaged); err != nil {
		t.Fatal(err)
	}
	return sc
}

// TestRunner_Smoke runs the smoke scenario end to end against a real
// sqi-server: 50 jobs x 2 steps x 10 tasks on 20 simulated workers with one
// expected crash. Every one of the 1,000 tasks must end terminal on the
// server and every invariant must hold.
func TestRunner_Smoke(t *testing.T) {
	if testing.Short() {
		t.Skip("runs real sqi binaries for about a minute; skipped with -short")
	}
	managed := binaries(t)
	out := t.TempDir()
	began := time.Now()
	rec, err := Run(t.Context(), Options{
		Scenario: smokeScenario(t, "20s"),
		RunID:    "smoke-test",
		OutDir:   out,
		Seed:     1,
		Managed:  managed,
		Logger:   testLogger(t),
	})
	t.Logf("Run took %s", time.Since(began).Round(time.Millisecond))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rec.Complete || !rec.Valid {
		t.Errorf("record complete=%v valid=%v (%s), want both", rec.Complete, rec.Valid, rec.InvalidReason)
	}
	if len(rec.Invariants) != 6 {
		t.Fatalf("%d invariants, want 6: %+v", len(rec.Invariants), rec.Invariants)
	}
	for _, inv := range rec.Invariants {
		if !inv.OK {
			t.Errorf("invariant %s failed: %q", inv.Name, inv.Evidence)
		}
	}
	if got := rec.Counters[report.CounterTasksTerminal]; got != 1000 {
		t.Errorf("tasks.terminal = %d, want 1000", got)
	}
	if got := rec.Counters[report.CounterJobsSubmitted]; got != 50 {
		t.Errorf("jobs.submitted = %d, want 50", got)
	}
	// 50 jobs x 2 steps x 10 tasks x 20 lines: every task's last attempt
	// emitted its 20; a crashed attempt adds the lines it got out.
	if got := rec.Counters[report.CounterLogLines]; got < 20000 {
		t.Errorf("logs.lines = %d, want at least 20000", got)
	}
	if got := rec.Counters[report.CounterWorkersLive]; got != 20 {
		t.Errorf("workers.live = %d, want 20", got)
	}
	if rec.SteadyCounters == nil {
		t.Fatal("SteadyCounters is nil; the run passed its warmup")
	}
	for k := range rec.Counters {
		if _, ok := rec.SteadyCounters[k]; !ok {
			t.Errorf("SteadyCounters lacks %q", k)
		}
		if _, ok := rec.WindowCounters[k]; !ok {
			t.Errorf("WindowCounters lacks %q", k)
		}
	}
	// The measured window ends when submissions stop, 20 s in, before the
	// drain; the record's Ended is after the drain.
	if want := rec.Started.Add(20 * time.Second); !rec.WindowEnd.Equal(want) || rec.WindowEnd.After(rec.Ended) {
		t.Errorf("WindowEnd = %s, want Started + duration (%s), not after Ended %s", rec.WindowEnd, want, rec.Ended)
	}
	if got := rec.WindowCounters[report.CounterJobsSubmitted]; got != 50 {
		t.Errorf("jobs.submitted when submissions stopped = %d, want 50", got)
	}
	if b := rec.Backlog; b == nil || b.Open < 0 || b.TailCompleted < 0 || !b.TailFrom.After(rec.Started) || !b.TailFrom.Before(rec.WindowEnd) {
		t.Errorf("Backlog = %+v, want one read in the window's last tenth", b)
	}
	for _, k := range []string{report.HistSubmitLatency, report.HistFirstLease, report.HistTaskE2E, report.HistTerminalIngest, report.HistJobE2E} {
		if h := rec.Histograms[k]; h == nil || h.Count() == 0 {
			t.Errorf("histogram %s is empty", k)
		}
	}
	if n := rec.Histograms[report.HistJobE2E].Count(); n != 50 {
		t.Errorf("job_e2e has %d samples, want one per job (50)", n)
	}
	if len(rec.Series) == 0 {
		t.Error("no series were recorded")
	}
	if !rec.Ended.After(rec.Started) {
		t.Errorf("Ended %s is not after Started %s", rec.Ended, rec.Started)
	}
	dir := filepath.Join(out, "smoke-test")
	for _, f := range []string{"run.json", "summary.md", "series.csv", filepath.Join("target", "server.log")} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	back, err := report.Read(dir)
	if err != nil {
		t.Fatalf("read back run.json: %v", err)
	}
	if back.Counters[report.CounterTasksTerminal] != 1000 || !back.Complete {
		t.Errorf("run.json round trip: complete=%v tasks.terminal=%d", back.Complete, back.Counters[report.CounterTasksTerminal])
	}
}

// TestRunner_InterruptWritesPartialRecord cancels a run 5 s in, as Ctrl-C
// does: the record must still be written, marked incomplete, and no process
// the run started may outlive it.
func TestRunner_InterruptWritesPartialRecord(t *testing.T) {
	if testing.Short() {
		t.Skip("runs real sqi binaries; skipped with -short")
	}
	managed := binaries(t)
	out := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	time.AfterFunc(5*time.Second, cancel)
	var pids []int
	began := time.Now()
	rec, err := Run(ctx, Options{
		Scenario: smokeScenario(t, "1m"),
		RunID:    "interrupted",
		OutDir:   out,
		Managed:  managed,
		Logger:   testLogger(t),
		OnTarget: func(tg target.Target) {
			if p, ok := tg.(interface{ PIDs() []int }); ok {
				pids = p.PIDs()
			}
		},
	})
	t.Logf("Run returned %s after it started", time.Since(began).Round(time.Millisecond))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.Complete {
		t.Error("an interrupted run's record says complete")
	}
	if len(pids) == 0 {
		t.Fatal("OnTarget saw no process ids")
	}
	for _, pid := range pids {
		if proctest.Alive(pid) {
			t.Errorf("process %d of the run is still alive", pid)
		}
	}
	b, err := os.ReadFile(filepath.Join(out, "interrupted", "run.json"))
	if err != nil {
		t.Fatalf("run.json: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("run.json: %v", err)
	}
	if c, ok := raw["complete"].(bool); !ok || c {
		t.Errorf(`run.json "complete" = %v, want false`, raw["complete"])
	}
}

// TestRunner_BeforeVerify proves the hook reaches the verifier: a ledger made
// to disagree with the server fails the outcomes invariant.
func TestRunner_BeforeVerify(t *testing.T) {
	if testing.Short() {
		t.Skip("runs real sqi binaries; skipped with -short")
	}
	managed := binaries(t)
	sc := smokeScenario(t, "10s")
	sc.Workers[0].Churn = scenario.Churn{}
	sc.Jobs[0].Total = 5
	rec, err := Run(t.Context(), Options{
		Scenario: sc, RunID: "before-verify", OutDir: t.TempDir(), Seed: 2, Managed: managed, Logger: testLogger(t),
		BeforeVerify: func(in *verify.Input) {
			for _, e := range in.Ledger.Events() {
				if e.Kind == simworker.KindTerminal {
					in.Ledger.SetOutcome(e.TaskID, simworker.OutcomeFailed)
					return
				}
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, inv := range rec.Invariants {
		if inv.Name == verify.NameOutcomes && inv.OK {
			t.Error("outcomes held although BeforeVerify made the ledger disagree")
		}
	}
}

func TestNewRunID(t *testing.T) {
	at := time.Date(2026, 11, 4, 9, 5, 7, 0, time.FixedZone("PST", -8*3600))
	id := NewRunID(at)
	if !regexp.MustCompile(`^20261104-170507-[0-9a-f]{4}$`).MatchString(id) {
		t.Errorf("NewRunID = %q, want 20261104-170507-xxxx in UTC", id)
	}
	if err := ValidRunID(id); err != nil {
		t.Errorf("ValidRunID(%q) = %v", id, err)
	}
	if other := NewRunID(at); other == id {
		t.Logf("two run IDs in the same second collided (%s); possible, 1 in 65536", id)
	}
}

func TestValidRunID(t *testing.T) {
	cases := []struct {
		id string
		ok bool
	}{
		{"20261104-170507-ab12", true},
		{"nightly-ci-7", true},
		{"nightly_ci", false},
		{"big-s1", false},
		{"big-s", true},
		{"big-s1x", true},
		{"", false},
		{"a.b", false},
		{"a b", false},
		{"a:b", false},
		{"a/b", false},
		{`a\b`, false},
		{"a*", false},
		{"a>", false},
		{"..", false},
		{string(make([]byte, 65)), false},
	}
	for _, tc := range cases {
		if err := ValidRunID(tc.id); (err == nil) != tc.ok {
			t.Errorf("ValidRunID(%q) = %v, want ok %v", tc.id, err, tc.ok)
		}
	}
}

func TestSeedFor(t *testing.T) {
	a, b := SeedFor("20261104-170507-ab12"), SeedFor("20261104-170507-ab13")
	if a == 0 || b == 0 || a == b {
		t.Errorf("SeedFor gave %d and %d; want two different non-zero seeds", a, b)
	}
	if again := SeedFor("20261104-170507-ab12"); again != a {
		t.Errorf("SeedFor is not deterministic: %d then %d", a, again)
	}
}

func TestLedgerHistograms(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }
	jobs := []driver.SubmittedJob{
		{ID: "j1", SubmittedAt: at(0), AcceptedAt: at(1), Status: 201},
		{ID: "j2", SubmittedAt: at(10), AcceptedAt: at(11), Status: 201},
		{ID: "j3", SubmittedAt: at(20), AcceptedAt: at(21), Status: 201}, // never leased
	}
	events := []simworker.Event{
		{At: at(3), JobID: "j1", TaskID: "t1", AttemptID: "a1", Kind: simworker.KindLeased},
		{At: at(2), JobID: "j1", TaskID: "t2", AttemptID: "a2", Kind: simworker.KindLeased},
		{At: at(5), JobID: "j1", TaskID: "t1", AttemptID: "a1", Kind: simworker.KindTerminal, Outcome: simworker.OutcomeSucceeded},
		{At: at(6), JobID: "j1", TaskID: "t2", AttemptID: "a2", Kind: simworker.KindCrashed},
		{At: at(14), JobID: "j2", TaskID: "t3", AttemptID: "a3", Kind: simworker.KindLeased},
		{At: at(15), JobID: "j2", TaskID: "t3", AttemptID: "a3", Kind: simworker.KindCanceled},
		{At: at(16), JobID: "j2", TaskID: "t4", AttemptID: "a4", Kind: simworker.KindLeased},
		{At: at(16.5), JobID: "j2", TaskID: "t4", AttemptID: "a4", Kind: simworker.KindTerminal, Outcome: "worker_shutdown"},
	}
	m := driver.NewMetrics(nil)
	ledgerHistograms(events, jobs, m)
	// First lease: j1 at 2 s after its submission, j2 at 4 s; j3 none.
	if n := m.FirstLease.Count(); n != 2 {
		t.Errorf("first_lease has %d samples, want 2", n)
	}
	if lo, hi := m.FirstLease.Min(), m.FirstLease.Max(); !near(lo, 2*time.Second) || !near(hi, 4*time.Second) {
		t.Errorf("first_lease min %s max %s, want 2s and 4s", lo, hi)
	}
	// Task e2e: a1 (2 s, succeeded) and a3 (1 s, canceled); a crash and a
	// shutdown give-up are not ends of a task.
	if n := m.TaskE2E.Count(); n != 2 {
		t.Errorf("task_e2e has %d samples, want 2", n)
	}
	if lo, hi := m.TaskE2E.Min(), m.TaskE2E.Max(); !near(lo, time.Second) || !near(hi, 2*time.Second) {
		t.Errorf("task_e2e min %s max %s, want 1s and 2s", lo, hi)
	}
}

func near(got, want time.Duration) bool {
	d := got - want
	if d < 0 {
		d = -d
	}
	return d <= want/50
}

func TestCompleteSteady(t *testing.T) {
	final := map[string]int64{"a": 5, "b": 7, "submit.status.503": 2}
	steady := map[string]int64{"a": 1, "b": 3}
	got := completeSteady(steady, final)
	if len(got) != 3 || got["a"] != 1 || got["b"] != 3 {
		t.Errorf("completeSteady = %v", got)
	}
	if v, ok := got["submit.status.503"]; !ok || v != 0 {
		t.Errorf(`a key first seen after the snapshot must read 0, got %v (present %v)`, v, ok)
	}
	if completeSteady(nil, final) != nil {
		t.Error("no snapshot must stay nil")
	}
}

func TestConvergeWithin(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]any
		hb   time.Duration
		want time.Duration
	}{
		{name: "server default", hb: 15 * time.Second, want: 30*time.Second + 15*time.Second + convergeMargin},
		{name: "scenario override", cfg: map[string]any{"scheduler": map[string]any{"heartbeat_timeout": "45s"}}, hb: 5 * time.Second, want: 45*time.Second + 5*time.Second + convergeMargin},
		{name: "unparseable override falls back", cfg: map[string]any{"scheduler": map[string]any{"heartbeat_timeout": 7}}, want: 30*time.Second + convergeMargin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := &scenario.Scenario{Server: scenario.ServerSpec{Config: tc.cfg}}
			if tc.hb > 0 {
				sc.Workers = []scenario.WorkerGroup{{Heartbeat: scenario.Duration{Duration: tc.hb}}}
			}
			if got := convergeWithin(sc); got != tc.want {
				t.Errorf("convergeWithin = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestLogOverflowExpected(t *testing.T) {
	heavy, err := scenario.Builtin("logs-heavy")
	if err != nil {
		t.Fatal(err)
	}
	smoke, err := scenario.Builtin("smoke")
	if err != nil {
		t.Fatal(err)
	}
	if !logOverflowExpected(heavy) {
		t.Error("logs-heavy plans more log bytes than the stream holds, but overflow is not expected")
	}
	if logOverflowExpected(smoke) {
		t.Error("smoke expects log overflow")
	}
}

func TestSampleShare(t *testing.T) {
	sc := &scenario.Scenario{Jobs: []scenario.JobClass{{Name: "a", Steps: 2, TasksPerStep: 10}, {Name: "b", Steps: 1, TasksPerStep: 1000}}}
	cases := []struct {
		name string
		jobs []driver.SubmittedJob
		want float64
	}{
		{name: "none", want: 1},
		{name: "small", jobs: []driver.SubmittedJob{{Class: "a"}, {Class: "a"}}, want: 1},
		{name: "large", jobs: []driver.SubmittedJob{{Class: "b"}, {Class: "b"}, {Class: "b"}, {Class: "b"}}, want: 0.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sampleShare(sc, tc.jobs); got != tc.want {
				t.Errorf("sampleShare = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTerminalIngest(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }
	events := []simworker.Event{
		{At: at(1), TaskID: "t1", AttemptID: "a1", Kind: simworker.KindLeased},
		{At: at(5), TaskID: "t1", AttemptID: "a1", Kind: simworker.KindTerminal, Outcome: simworker.OutcomeSucceeded},
		// Failed, then retried and succeeded: the last report counts.
		{At: at(2), TaskID: "t2", AttemptID: "a2", Kind: simworker.KindTerminal, Outcome: simworker.OutcomeFailed},
		{At: at(8), TaskID: "t2", AttemptID: "a3", Kind: simworker.KindTerminal, Outcome: simworker.OutcomeSucceeded},
		// The server disagrees with the last report: left out.
		{At: at(3), TaskID: "t3", AttemptID: "a4", Kind: simworker.KindTerminal, Outcome: simworker.OutcomeSucceeded},
		// Canceled: the server's cancel commits before the worker's report.
		{At: at(4), TaskID: "t4", AttemptID: "a5", Kind: simworker.KindCanceled},
		// A server clock behind the generator's: recorded as 0.
		{At: at(9), TaskID: "t5", AttemptID: "a6", Kind: simworker.KindTerminal, Outcome: simworker.OutcomeFailed},
	}
	tasks := map[string]taskEnd{
		"t1": {status: "succeeded", updatedAt: at(5.25)},
		"t2": {status: "succeeded", updatedAt: at(8.5)},
		"t3": {status: "failed", updatedAt: at(3.1)},
		"t4": {status: "canceled", updatedAt: at(3.9)},
		"t5": {status: "failed", updatedAt: at(8.9)},
		"t6": {status: "succeeded", updatedAt: at(1)}, // a real worker's: no ledger event
		"t7": {status: "succeeded"},                   // no updated_at
	}
	m := driver.NewMetrics(nil)
	if n := terminalIngest(events, tasks, m); n != 3 {
		t.Errorf("terminalIngest measured %d tasks, want 3 (t1, t2, t5)", n)
	}
	if lo, hi := m.TerminalIngest.Min(), m.TerminalIngest.Max(); lo > time.Millisecond || !near(hi, 500*time.Millisecond) {
		t.Errorf("terminal_ingest min %s max %s, want about 0 (t5, clamped) and 500ms (t2)", lo, hi)
	}
}

// TestRecord_NoSubmissionAccepted checks that a complete run whose scenario
// submits jobs, but whose submissions the server all refused, is INVALID with
// a submissions-rejected error, and not a run in which every invariant held.
func TestRecord_NoSubmissionAccepted(t *testing.T) {
	for _, tc := range []struct {
		name      string
		accepted  bool
		wantValid bool
	}{
		{"all refused", false, false},
		{"one accepted", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := newRun(Options{Scenario: smokeScenario(t, "20s"), AllowSaturated: true})
			if err != nil {
				t.Fatal(err)
			}
			r.complete = true
			r.invariants = []verify.Result{{Name: verify.NameTerminal, OK: true}}
			r.metrics.Status(403).Add(49)
			if tc.accepted {
				r.metrics.Status(201).Add(1)
				r.jobs.Record(driver.SubmittedJob{ID: "j1", Class: "shot", Status: 201})
			}
			rec := r.record()
			if rec.Valid != tc.wantValid {
				t.Errorf("Valid = %v (%s), want %v", rec.Valid, rec.InvalidReason, tc.wantValid)
			}
			if !tc.wantValid && !strings.Contains(rec.InvalidReason, "no submission accepted") {
				t.Errorf("InvalidReason = %q, want it to say no submission was accepted", rec.InvalidReason)
			}
			var rules []string
			for _, f := range rec.Findings {
				rules = append(rules, f.Rule+"/"+f.Severity)
			}
			if !slices.Contains(rules, report.RuleRejected+"/"+report.SeverityError) {
				t.Errorf("findings %v, want a submissions-rejected error", rules)
			}
			if slices.Contains(rules, report.RuleGeneratorSaturated+"/"+report.SeverityWarn) {
				t.Errorf("findings %v: refused submissions are not a saturated generator", rules)
			}
		})
	}
}

// TestRunner_ResumesPausedQueues runs the smoke scenario with its only queue
// paused part way and never resumed by the scenario: the runner must resume
// it once submissions stop, so every job still finishes and every invariant
// holds, rather than the paused jobs reading as stranded by the server.
func TestRunner_ResumesPausedQueues(t *testing.T) {
	if testing.Short() {
		t.Skip("runs real sqi binaries for about a minute; skipped with -short")
	}
	sc := smokeScenario(t, "20s")
	sc.Actions = []scenario.Action{{At: scenario.Duration{Duration: 5 * time.Second}, PauseQueue: "render"}}
	// Short, so a paused queue fails the run quickly rather than in 10 min.
	sc.DrainTimeout.Duration = 45 * time.Second
	if err := sc.Validate(scenario.TargetManaged); err != nil {
		t.Fatal(err)
	}
	rec, err := Run(t.Context(), Options{
		Scenario: sc, RunID: "resume-test", OutDir: t.TempDir(), Seed: 1, Managed: binaries(t), Logger: testLogger(t),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rec.Complete {
		t.Fatal("the run is incomplete")
	}
	for _, inv := range rec.Invariants {
		if !inv.OK {
			t.Errorf("invariant %s failed: %q", inv.Name, inv.Evidence)
		}
	}
	if got := rec.Counters[report.CounterTasksTerminal]; got != 1000 {
		t.Errorf("tasks.terminal = %d, want 1000", got)
	}
}

func TestAttach_IgnoredServerSettings(t *testing.T) {
	sc := smokeScenario(t, "20s")
	sc.Server.BrokerAuth = true
	sc.Server.Config = map[string]any{"scheduler": map[string]any{"heartbeat_timeout": "45s"}}
	if got := serverSettings(sc); !slices.Equal(got, []string{"server.broker_auth=true", "server.config.scheduler"}) {
		t.Errorf("serverSettings = %v, want broker_auth and the config key", got)
	}
	if got := serverSettings(smokeScenario(t, "20s")); len(got) != 0 {
		t.Errorf("serverSettings of a default scenario = %v, want none", got)
	}
}

func TestOptions_Normalize(t *testing.T) {
	sc := smokeScenario(t, "20s")
	o := Options{Scenario: sc, RunID: "n1", Mode: report.ModeSoak}
	if err := o.Normalize(); err != nil {
		t.Fatal(err)
	}
	switch {
	case o.Managed == nil || o.Attach != nil || o.TargetKind() != scenario.TargetManaged:
		t.Errorf("target: managed %+v attach %+v, want a default managed one", o.Managed, o.Attach)
	case o.OutDir != DefaultOutDir || o.Seed != SeedFor("n1") || o.Sample != DefaultSample:
		t.Errorf("defaults not filled: out %q seed %d sample %s", o.OutDir, o.Seed, o.Sample)
	case o.ProfileEvery != DefaultSoakProfileEvery:
		t.Errorf("a soak's profile period = %s, want %s", o.ProfileEvery, DefaultSoakProfileEvery)
	}
	again := o
	if err := again.Normalize(); err != nil || again.Seed != o.Seed || again.Managed != o.Managed {
		t.Errorf("Normalize is not idempotent: err %v, %+v", err, again)
	}
	endsOnly := Options{Scenario: sc, Mode: report.ModeSoak, ProfileEvery: -1}
	for range 2 {
		if err := endsOnly.Normalize(); err != nil || endsOnly.ProfileEvery >= 0 {
			t.Errorf("a soak profiling at its start and end only: err %v, period %s", err, endsOnly.ProfileEvery)
		}
	}

	bad := sc.Clone()
	if err := bad.Set("duration", "1s"); err != nil {
		t.Fatal(err)
	}
	o.Scenario = bad
	if err := o.Normalize(); err == nil || !strings.Contains(err.Error(), "warmup") {
		t.Errorf("a new, invalid scenario: err = %v, want it validated", err)
	}

	for _, tc := range []struct {
		name string
		o    Options
		want string
	}{
		{"no scenario", Options{}, "no scenario"},
		{"both targets", Options{Scenario: sc, Managed: &target.ManagedConfig{}, Attach: &target.AttachConfig{}}, "not both"},
		{"bad mode", Options{Scenario: sc, Mode: "sprint"}, "mode"},
		{"bad run id", Options{Scenario: sc, RunID: "a:b"}, "run ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.o.Normalize(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}
