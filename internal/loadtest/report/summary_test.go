// SPDX-License-Identifier: AGPL-3.0-or-later

package report

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/measure"
	"github.com/uberware/sqi/internal/loadtest/verify"
)

// goldenRecord is a fixed record with every summary section populated.
func goldenRecord() *Record {
	r := baseRecord()
	r.Env = Env{OS: "linux", Arch: "amd64", CPUs: 8, SQIVersion: "v0.4.0", Commit: "abc1234", Store: "sqlite", Flags: []string{"--seed", "7"}}
	r.Ended = t0.Add(12 * time.Minute)
	r.Histograms[HistSubmitLatency] = hist(5*time.Millisecond, 8*time.Millisecond, 12*time.Millisecond)
	r.Histograms[HistFirstLease] = hist(850*time.Millisecond, 1234*time.Millisecond)
	r.Histograms[HistRESTPrefix+"jobs-list"] = hist(40 * time.Millisecond)
	r.Histograms[HistWSLag] = measure.NewHistogram()
	r.Counters = map[string]int64{
		"jobs.submitted":    600,
		"tasks.terminal":    12000,
		CounterWorkersLive:  100,
		"submit.status.201": 600,
	}
	// What the counters held when warmup ended, two minutes in.
	r.SteadyCounters = map[string]int64{
		"jobs.submitted":    120,
		"tasks.terminal":    2400,
		CounterWorkersLive:  100,
		"submit.status.201": 120,
	}
	// Submissions stopped ten minutes in; the jobs drained over two more.
	r.WindowEnd = t0.Add(10 * time.Minute)
	r.WindowCounters = map[string]int64{
		"jobs.submitted":    504,
		"tasks.terminal":    10080,
		CounterWorkersLive:  100,
		"submit.status.201": 504,
	}
	r.Series = []measure.Series{
		ramp("server.rss_bytes", "bytes", 12, time.Minute, 200<<20, 4<<20),
		ramp("server.goroutines", "count", 12, time.Minute, 1000, 0),
		ramp("server.cpu_cores", "cores", 12, time.Minute, 1.0, 0.1),
		values("server.gc_pause_p99_s", "s", time.Minute, 0.001, 0.0035, 0.002),
		values("disk.sqlite_wal_bytes", "bytes", time.Minute, 1000, 5000, 2000, 6000),
		values("sched.queue_depth{default}", "tasks", time.Minute, 10, 500, 0),
		values("gen.cpu_cores", "cores", time.Minute, 0.5, 1.5, 0.75),
		values("gen.sched_lag_p99_s", "s", time.Minute, 0.01, 0.2, 0.05),
	}
	r.Invariants = []verify.Result{
		okResult("terminal"),
		{Name: "logs", OK: false, Evidence: []string{"task t1: sent 10 lines, server returned 9"}},
	}
	r.Unavailable = []string{"pg.db_bytes: not a PostgreSQL run"}
	r.Findings = []Finding{{Rule: "wal-never-shrank", Severity: SeverityWarn, Message: "WAL grew to 5.9 KiB and never shrank"}}
	return r
}

func TestSummary_Golden(t *testing.T) {
	var sb strings.Builder
	if err := writeSummary(&sb, goldenRecord()); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/summary.golden.md")
	if err != nil {
		t.Fatal(err)
	}
	// Git on Windows may check the golden file out with CRLF line endings.
	if got, w := sb.String(), strings.ReplaceAll(string(want), "\r\n", "\n"); got != w {
		t.Fatalf("summary differs from testdata/summary.golden.md\n--- got ---\n%s\n--- want ---\n%s", got, w)
	}
}

func TestSummary_ThroughputMethods(t *testing.T) {
	// Submissions ran for 10 minutes, then the jobs drained for 2. 10080
	// tasks had finished when submissions stopped, 2400 of them by the end of
	// the 2 minute warmup: 7680 tasks over the 480 s steady window is 16/s,
	// where the average from the start to the end of submissions is 10080
	// over 600 s, 16.80/s. Neither counts the drain.
	steady := goldenRecord()
	average := goldenRecord()
	average.SteadyCounters = nil
	for _, tc := range []struct {
		name string
		rec  *Record
		want []string
	}{
		{"steady window", steady, []string{"steady-window rates", "(8m0s)", "| tasks.terminal | 12000 | 7680 | 16.00 |", "| jobs.submitted | 600 | 384 | 0.80 |", "| workers.live | 100 | - | - |", "drain 2m0s"}},
		{"run average", average, []string{"run averages", "| tasks.terminal | 12000 | 10080 | 16.80 |", "warmup included"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sb strings.Builder
			if err := writeSummary(&sb, tc.rec); err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(sb.String(), want) {
					t.Errorf("summary lacks %q:\n%s", want, sb.String())
				}
			}
			if !strings.Contains(sb.String(), "Histograms cover the whole run, warmup included.") {
				t.Error("the Latency section must say its histograms include warmup")
			}
		})
	}
}

func TestSummary_Invalid(t *testing.T) {
	r := baseRecord()
	r.Valid, r.Complete, r.InvalidReason = false, false, "generator saturated: CPU 3.1 cores"
	var sb strings.Builder
	if err := writeSummary(&sb, r); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"INVALID", "generator saturated: CPU 3.1 cores", "INCOMPLETE"} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("summary lacks %q:\n%s", want, sb.String())
		}
	}
}

func TestFmtDur(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{1234 * time.Millisecond, "1.234s"},
		{850 * time.Millisecond, "850ms"},
		{12500 * time.Microsecond, "12.5ms"},
		{0, "0s"},
		{90*time.Second + 400*time.Millisecond, "1m30.4s"},
	} {
		if got := fmtDur(tc.d); got != tc.want {
			t.Errorf("fmtDur(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestFmtBytes(t *testing.T) {
	for _, tc := range []struct {
		v    float64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1536, "1.5 KiB"},
		{200 << 20, "200.0 MiB"},
		{-3 << 30, "-3.0 GiB"},
	} {
		if got := fmtBytes(tc.v); got != tc.want {
			t.Errorf("fmtBytes(%v) = %q, want %q", tc.v, got, tc.want)
		}
	}
}

func TestSummary_InvariantsNotRun(t *testing.T) {
	for _, tc := range []struct {
		name     string
		complete bool
		want     string
	}{
		{"interrupted", false, "Not run (interrupted)"},
		{"complete with none", true, "No invariants were checked."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := baseRecord()
			r.Complete, r.Invariants = tc.complete, nil
			var b strings.Builder
			if err := writeSummary(&b, r); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(b.String(), tc.want) {
				t.Errorf("summary lacks %q:\n%s", tc.want, b.String())
			}
		})
	}
}

// TestSummary_FaultsTableEscapesCells proves an error with a pipe or a line
// break, as errors.Join produces, stays inside its table row.
func TestSummary_FaultsTableEscapesCells(t *testing.T) {
	r := baseRecord()
	r.Faults = []FaultRecord{{
		Index: 0, Kind: "kill", Target: "group a|b", Start: t0.Add(time.Second),
		Error: "kill worker 0: refused\r\nkill worker 1: a | b\nthird",
	}}
	var b strings.Builder
	if err := writeSummary(&b, r); err != nil {
		t.Fatal(err)
	}
	want := "| 0 | kill | group a\\|b | +1s | never | kill worker 0: refused kill worker 1: a \\| b third |\n"
	if !strings.Contains(b.String(), want) {
		t.Errorf("summary lacks %q:\n%s", want, b.String())
	}
}

func TestSummary_RendersFaultsReworkAndKnown(t *testing.T) {
	r := baseRecord()
	r.Faults = []FaultRecord{
		{Index: 0, Kind: "kill", Target: "server", Start: t0.Add(90 * time.Second), Healed: t0.Add(2 * time.Minute)},
		{Index: 1, Kind: "stall", Target: "group render (real 0,1)", Start: t0.Add(5 * time.Minute), Error: "pause failed"},
	}
	r.Rework = &Rework{Attempts: 3, Seconds: 12.5, ByFault: map[int]int{1: 1, 0: 2}}
	r.Reclaims = &Reclaims{Attempts: 2, ByReason: map[string]int{"worker restarted": 1, "worker went offline": 1}, Examples: []string{"attempt a1 of task t1"}}
	r.Invariants = []verify.Result{
		okResult("terminal"),
		{Name: "caps", OK: true, Known: "H13f1", Evidence: []string{"pool over its cap"}},
		{Name: "store-integrity", Evidence: []string{"open the store: boom"}, Unmeasured: true},
	}
	var b strings.Builder
	if err := writeSummary(&b, r); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	for _, want := range []string{
		"## Faults\n",
		"| # | Kind | Target | Start | Healed | Error |",
		"| 0 | kill | server | +1m30s | +2m0s | - |",
		"| 1 | stall | group render (real 0,1) | +5m0s | never | pause failed |",
		"## Rework\n",
		"3 attempts, 12.5s of work; by fault: #0 2, #1 1",
		"- known (H13f1) caps\n  - pool over its cap\n",
		"## Reclaims\n",
		"2 attempts; by reason: worker restarted 1, worker went offline 1",
		"- attempt a1 of task t1\n",
		"- UNMEASURED store-integrity\n  - open the store: boom\n",
		"- PASS terminal\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "## Faults") > strings.Index(got, "## Invariants") {
		t.Error("Faults should come before Invariants")
	}
	// A run without faults or rework has neither section.
	b.Reset()
	if err := writeSummary(&b, baseRecord()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "## Faults") || strings.Contains(b.String(), "## Rework") || strings.Contains(b.String(), "## Reclaims") {
		t.Errorf("a plain run rendered chaos sections:\n%s", b.String())
	}
}
