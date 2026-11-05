// SPDX-License-Identifier: AGPL-3.0-or-later

package report

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/measure"
)

func deltaFor(t *testing.T, ds []Delta, metric string) Delta {
	t.Helper()
	for _, d := range ds {
		if d.Metric == metric {
			return d
		}
	}
	t.Fatalf("no delta for %q in %+v", metric, ds)
	return Delta{}
}

// recWith returns a record whose first-lease latency is one observation of
// lat and whose tasks.terminal counter is n over a ten minute run with two
// minutes of warmup.
func recWith(lat time.Duration, n int64) *Record {
	r := baseRecord()
	r.Histograms[HistFirstLease] = hist(lat)
	r.Counters["tasks.terminal"] = n
	return r
}

func TestCompare_NoiseBand(t *testing.T) {
	for _, tc := range []struct {
		name        string
		metric      string
		a, b        *Record
		wantVerdict string
		wantPct     float64
	}{
		{"latency +4% is noise", "first_lease.p95", recWith(100*time.Millisecond, 1000), recWith(104*time.Millisecond, 1000), VerdictNoChange, 4},
		{"latency +6% is worse", "first_lease.p95", recWith(100*time.Millisecond, 1000), recWith(106*time.Millisecond, 1000), VerdictWorse, 6},
		{"latency -6% is better", "first_lease.p95", recWith(100*time.Millisecond, 1000), recWith(94*time.Millisecond, 1000), VerdictBetter, -6},
		{"latency +5% is inside the band", "first_lease.p95", recWith(100*time.Millisecond, 1000), recWith(105*time.Millisecond, 1000), VerdictNoChange, 5},
		{"throughput +6% is better", "tasks.terminal/s", recWith(time.Second, 1000), recWith(time.Second, 1060), VerdictBetter, 6},
		{"throughput -6% is worse", "tasks.terminal/s", recWith(time.Second, 1000), recWith(time.Second, 940), VerdictWorse, -6},
		{"throughput +4% is noise", "tasks.terminal/s", recWith(time.Second, 1000), recWith(time.Second, 1040), VerdictNoChange, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := deltaFor(t, Compare(tc.a, tc.b), tc.metric)
			if d.Verdict != tc.wantVerdict {
				t.Errorf("verdict = %q, want %q (%+v)", d.Verdict, tc.wantVerdict, d)
			}
			if math.Abs(d.Pct-tc.wantPct) > 0.6 { // a quantile is within 0.4% of the recorded value
				t.Errorf("pct = %v, want about %v", d.Pct, tc.wantPct)
			}
		})
	}
}

// warmupRecord is a ten minute run with the given warmup whose tasks.terminal
// counter climbs at 1/s through warmup and at steadyRate/s after it, with the
// SteadyCounters snapshot taken when warmup ends.
func warmupRecord(warmup time.Duration, steadyRate int64) *Record {
	r := baseRecord()
	r.Scenario.Warmup.Duration = warmup
	atSteady := int64(warmup.Seconds())
	steadySecs := int64((10*time.Minute - warmup).Seconds())
	r.SteadyCounters = map[string]int64{CounterTasksTerminal: atSteady}
	r.Counters[CounterTasksTerminal] = atSteady + steadyRate*steadySecs
	return r
}

func TestCompare_DifferentWarmupSameSteadyRate(t *testing.T) {
	a, b := warmupRecord(5*time.Minute, 10), warmupRecord(time.Minute, 10)
	d := deltaFor(t, Compare(a, b), CounterTasksTerminal+"/s")
	if d.Verdict != VerdictNoChange || d.A != 10 || d.B != 10 {
		t.Fatalf("identical steady-state rates must compare equal: %+v", d)
	}
	// The run averages differ a lot (5.5/s against 9.1/s), so a comparison
	// that ignored the snapshot would call this an improvement.
	b.SteadyCounters = nil
	d = deltaFor(t, Compare(a, b), CounterTasksTerminal+"/s")
	if d.Verdict != VerdictBetter || math.Abs(d.A-5.5) > 1e-9 {
		t.Fatalf("without both snapshots both sides are run averages: %+v", d)
	}
}

func TestCompare_ErrorCountersAreLowerBetter(t *testing.T) {
	a, b := baseRecord(), baseRecord()
	a.Counters[CounterWSDrops] = 100
	b.Counters[CounterWSDrops] = 150
	if d := deltaFor(t, Compare(a, b), CounterWSDrops+"/s"); d.Verdict != VerdictWorse {
		t.Fatalf("more drops: %+v", d)
	}
	a.Counters[CounterSubmitStatusPrefix+"201"] = 100
	b.Counters[CounterSubmitStatusPrefix+"201"] = 150
	if d := deltaFor(t, Compare(a, b), CounterSubmitStatusPrefix+"201/s"); d.Verdict != VerdictBetter {
		t.Fatalf("more accepted submissions: %+v", d)
	}
}

func TestCompare_SeriesPeaksAndUnknowns(t *testing.T) {
	a, b := baseRecord(), baseRecord()
	a.Series = []measure.Series{values("server.rss_bytes", "bytes", time.Minute, 100, 200)}
	b.Series = []measure.Series{values("server.rss_bytes", "bytes", time.Minute, 100, 300), values("sched.queue_depth{x}", "tasks", time.Minute, 1)}
	a.Counters["mystery"], b.Counters["mystery"] = 1, 100
	a.Counters[CounterWorkersLive], b.Counters[CounterWorkersLive] = 100, 200
	ds := Compare(a, b)
	if d := deltaFor(t, ds, "peak.server.rss_bytes"); d.Verdict != VerdictWorse || d.Pct != 50 {
		t.Fatalf("rss peak: %+v", d)
	}
	for _, d := range ds {
		if strings.Contains(d.Metric, "mystery") || strings.Contains(d.Metric, "workers.live") || strings.Contains(d.Metric, "queue_depth") {
			t.Errorf("unknown metric compared: %+v", d)
		}
	}
}

func TestCompare_ZeroBaseline(t *testing.T) {
	for _, tc := range []struct {
		name        string
		a, b        int64
		counter     string
		wantVerdict string
		wantInf     bool
	}{
		{"0 to 0", 0, 0, CounterWSDrops, VerdictNoChange, false},
		{"errors appear", 0, 5, CounterWSDrops, VerdictWorse, true},
		{"throughput appears", 0, 5, "tasks.terminal", VerdictBetter, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := baseRecord(), baseRecord()
			a.Counters[tc.counter], b.Counters[tc.counter] = tc.a, tc.b
			d := deltaFor(t, Compare(a, b), tc.counter+"/s")
			if d.Verdict != tc.wantVerdict || math.IsInf(d.Pct, 1) != tc.wantInf {
				t.Fatalf("%+v", d)
			}
			if !tc.wantInf && d.Pct != 0 {
				t.Fatalf("pct = %v, want 0", d.Pct)
			}
		})
	}
}

func TestRenderDeltas(t *testing.T) {
	out := RenderDeltas([]Delta{
		{Metric: "first_lease.p95", A: 0.1, B: 0.106, Pct: 6, Verdict: VerdictWorse},
		{Metric: "ws.drops/s", A: 0, B: 1, Pct: math.Inf(1), Verdict: VerdictWorse},
	})
	for _, want := range []string{"first_lease.p95", "+6.0%", "new", "worse"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestCompareWarnings(t *testing.T) {
	ok := baseRecord()
	invalid := baseRecord()
	invalid.RunID, invalid.Valid, invalid.InvalidReason = "run-2", false, "generator CPU above 85%"
	partial := baseRecord()
	partial.RunID, partial.Complete = "run-3", false
	if ws := CompareWarnings(ok, ok); len(ws) != 0 {
		t.Errorf("two valid, complete runs: warnings %v, want none", ws)
	}
	ws := strings.Join(CompareWarnings(invalid, partial), "\n")
	for _, want := range []string{"A (run-2) is INVALID (generator CPU above 85%)", "B (run-3) is INCOMPLETE"} {
		if !strings.Contains(ws, want) {
			t.Errorf("warnings %q lack %q", ws, want)
		}
	}
}
