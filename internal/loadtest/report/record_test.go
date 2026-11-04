// SPDX-License-Identifier: AGPL-3.0-or-later

package report

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/measure"
	"github.com/uberware/sqi/internal/loadtest/verify"
)

func fullRecord() *Record {
	r := baseRecord()
	r.Shard = "0/2"
	r.Env = Env{OS: "linux", Arch: "amd64", CPUs: 8, SQIVersion: "v0.4.0", Commit: "abc123", Store: "sqlite", Flags: []string{"--seed", "7"}}
	r.Scenario.Server.Config = map[string]any{"nats": map[string]any{"embedded": map[string]any{"max_store_mb": float64(256)}}}
	r.Histograms[HistFirstLease] = hist(10*time.Millisecond, 20*time.Millisecond, 900*time.Millisecond)
	r.Counters["tasks.terminal"] = 4200
	r.SteadyCounters = map[string]int64{"tasks.terminal": 600}
	r.Series = []measure.Series{ramp("server.rss_bytes", "bytes", 5, time.Minute, 1e8, 1e6)}
	r.Invariants = []verify.Result{okResult("terminal"), {Name: "logs", OK: false, Evidence: []string{"task t1 lost lines"}}}
	r.Unavailable = []string{"pg.db_bytes: no DSN"}
	r.Findings = []Finding{{Rule: "ws-drops", Severity: SeverityError, Message: "3 drops"}}
	return r
}

func TestRecord_RoundTrip(t *testing.T) {
	r := fullRecord()
	dir := t.TempDir()
	if err := r.Write(dir); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"run.json", "summary.md", "series.csv"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("%s not written: %v", f, err)
		}
	}
	got, err := Read(filepath.Join(dir, "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, r) {
		t.Fatalf("round trip differs:\n got %+v\nwant %+v", got, r)
	}
	for _, q := range []float64{0, 0.5, 0.95, 0.99, 1} {
		if g, w := got.Histograms[HistFirstLease].Quantile(q), r.Histograms[HistFirstLease].Quantile(q); g != w {
			t.Errorf("q%v = %v, want %v", q, g, w)
		}
	}
	// A directory reads as its run.json.
	if _, err := Read(dir); err != nil {
		t.Fatalf("Read(dir): %v", err)
	}
}

func TestRead_Errors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "run.json")
	if err := os.WriteFile(bad, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"missing": filepath.Join(dir, "none.json"), "malformed": bad} {
		t.Run(name, func(t *testing.T) {
			if _, err := Read(path); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestSeriesCSV(t *testing.T) {
	series := []measure.Series{
		values("b.series", "s", time.Minute, 1.5, 2),
		values("a.series{x}", "count", time.Minute, 3),
	}
	var sb strings.Builder
	if err := writeSeriesCSV(&sb, series); err != nil {
		t.Fatal(err)
	}
	want := "series,unit,time_rfc3339,value\n" +
		"a.series{x},count,2026-11-03T10:00:00Z,3\n" +
		"b.series,s,2026-11-03T10:00:00Z,1.5\n" +
		"b.series,s,2026-11-03T10:01:00Z,2\n"
	if sb.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", sb.String(), want)
	}
}

// TestRecord_RatesExcludeTheDrain checks that a rate is taken over the
// measured window, from the end of warmup to the end of submissions, and not
// over the drain after it: two runs that differ only in how long their jobs
// took to finish after submissions stopped have the same rates.
func TestRecord_RatesExcludeTheDrain(t *testing.T) {
	rec := func(drain time.Duration) *Record {
		r := baseRecord()
		r.Scenario.Warmup.Duration = time.Minute
		r.WindowEnd = t0.Add(11 * time.Minute) // a 10 minute window after warmup
		r.Ended = r.WindowEnd.Add(drain)
		r.SteadyCounters = map[string]int64{CounterJobsSubmitted: 10, CounterTasksTerminal: 100}
		r.WindowCounters = map[string]int64{CounterJobsSubmitted: 70, CounterTasksTerminal: 700}
		r.Counters = map[string]int64{CounterJobsSubmitted: 70, CounterTasksTerminal: 1000}
		return r
	}
	short, long := rec(time.Minute), rec(8*time.Minute)
	for _, r := range []*Record{short, long} {
		for k, want := range map[string]float64{CounterJobsSubmitted: 0.1, CounterTasksTerminal: 1.0} {
			got, ok := r.rate(k, r.hasSteadyBasis())
			if !ok || math.Abs(got-want) > 1e-9 {
				t.Errorf("drain %s: rate(%s) = %v (ok %v), want %v over the 600 s window", r.Drain(), k, got, ok, want)
			}
		}
	}
	if short.Drain() != time.Minute || long.Drain() != 8*time.Minute {
		t.Errorf("Drain = %s and %s, want 1m and 8m", short.Drain(), long.Drain())
	}
	for _, d := range Compare(short, long) {
		if strings.HasPrefix(d.Metric, CounterJobsSubmitted) || strings.HasPrefix(d.Metric, CounterTasksTerminal) {
			if d.Verdict != VerdictNoChange {
				t.Errorf("compare %s: %v -> %v is %q, want no change: only the drain differs", d.Metric, d.A, d.B, d.Verdict)
			}
		}
	}

	// An older record, with no window, keeps rating up to Ended.
	old := rec(4 * time.Minute)
	old.WindowEnd, old.WindowCounters = time.Time{}, nil
	if got, _ := old.rate(CounterJobsSubmitted, true); math.Abs(got-60.0/(14*60)) > 1e-9 {
		t.Errorf("record without a window: rate = %v, want 60 over the 14 minutes to Ended", got)
	}
	if old.Drain() != 0 {
		t.Errorf("record without a window: Drain = %s, want 0", old.Drain())
	}
}
