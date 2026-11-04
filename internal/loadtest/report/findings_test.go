// SPDX-License-Identifier: AGPL-3.0-or-later

package report

import (
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/measure"
	"github.com/uberware/sqi/internal/loadtest/scenario"
)

func withCap(r *Record, mb any) *Record {
	r.Scenario.Server.Config = map[string]any{"nats": map[string]any{"embedded": map[string]any{"max_store_mb": mb}}}
	return r
}

func TestFindings(t *testing.T) {
	steady := t0.Add(2 * time.Minute)
	logs := "js.bytes{" + streamLogs + "}"

	for _, tc := range []struct {
		name     string
		rule     string
		severity string
		build    func() *Record
		fire     bool
	}{
		{
			name: "goroutines grow linearly", rule: RuleGoroutineGrowth, severity: SeverityWarn, fire: true,
			build: func() *Record {
				r := longRecord()
				r.Series = []measure.Series{ramp("server.goroutines", "count", 30, 2*time.Minute, 1000, 5)}
				return r
			},
		},
		{
			name: "one submission in a thousand refused", rule: RuleRejected, severity: SeverityWarn, fire: true,
			build: func() *Record {
				r := baseRecord()
				r.Counters = map[string]int64{"submit.status.201": 999, "submit.status.503": 1}
				return r
			},
		},
		{
			name: "every submission refused", rule: RuleRejected, severity: SeverityError, fire: true,
			build: func() *Record {
				r := baseRecord()
				r.Counters = map[string]int64{"submit.status.403": 40, "submit.status.0": 2}
				return r
			},
		},
		{
			name: "accepted and undecodable submissions are not rejections", rule: RuleRejected,
			build: func() *Record {
				r := baseRecord()
				r.Counters = map[string]int64{"submit.status.201": 50, "submit.status.-1": 1}
				return r
			},
		},
		{
			name: "a drain over a quarter of the duration", rule: RuleFellBehind, severity: SeverityWarn, fire: true,
			build: func() *Record { return drained(3*time.Minute, nil) },
		},
		{
			name: "a drain under a quarter of the duration", rule: RuleFellBehind,
			build: func() *Record {
				return drained(2*time.Minute, &Backlog{Open: 50, TailCompleted: 120, TailFrom: t0.Add(9 * time.Minute)})
			},
		},
		{
			name: "more open at the end than finished in the last tenth", rule: RuleFellBehind, severity: SeverityWarn, fire: true,
			build: func() *Record {
				return drained(time.Minute, &Backlog{Open: 500, TailCompleted: 120, TailFrom: t0.Add(9 * time.Minute)})
			},
		},
		{
			name: "goroutines flat", rule: RuleGoroutineGrowth,
			build: func() *Record {
				r := longRecord()
				r.Series = []measure.Series{ramp("server.goroutines", "count", 30, 2*time.Minute, 1000, 0)}
				return r
			},
		},
		{
			name: "goroutine growth only inside warmup", rule: RuleGoroutineGrowth,
			build: func() *Record {
				r := baseRecord()
				s := ramp("server.goroutines", "count", 12, 10*time.Second, 100, 50) // 0..110s, all before steady
				for i := range 20 {
					s.Points = append(s.Points, measure.Point{T: steady.Add(time.Duration(i) * 20 * time.Second), V: 650})
				}
				r.Series = []measure.Series{s}
				return r
			},
		},
		{
			name: "rss projects past 1 GiB a week", rule: RuleRSSGrowth, severity: SeverityWarn, fire: true,
			build: func() *Record {
				r := longRecord()
				// 10 MiB per minute is 600 MiB/h, 100 GiB over a week.
				r.Series = []measure.Series{ramp("server.rss_bytes", "bytes", 30, 2*time.Minute, 500<<20, 20<<20)}
				return r
			},
		},
		{
			name: "rss projects under 1 GiB a week", rule: RuleRSSGrowth,
			build: func() *Record {
				r := longRecord()
				// 1 KiB per sample is 30 KiB/h, 5 MiB over a week.
				r.Series = []measure.Series{ramp("server.rss_bytes", "bytes", 30, 2*time.Minute, 500<<20, 1024)}
				return r
			},
		},
		{
			name: "wal grows and never shrinks", rule: RuleWALNeverShrank, severity: SeverityWarn, fire: true,
			build: func() *Record {
				r := baseRecord()
				r.Series = []measure.Series{ramp("disk.sqlite_wal_bytes", "bytes", 10, time.Minute, 1<<20, 1<<20)}
				return r
			},
		},
		{
			name: "wal checkpoints", rule: RuleWALNeverShrank,
			build: func() *Record {
				r := baseRecord()
				r.Series = []measure.Series{values("disk.sqlite_wal_bytes", "bytes", time.Minute, 1<<20, 4<<20, 8<<20, 100, 2<<20, 3<<20)}
				return r
			},
		},
		{
			name: "wal on a run shorter than five minutes", rule: RuleWALNeverShrank,
			build: func() *Record {
				r := baseRecord()
				r.Series = []measure.Series{ramp("disk.sqlite_wal_bytes", "bytes", 5, time.Minute, 1<<20, 1<<20)}
				return r
			},
		},
		{
			name: "wal never grew", rule: RuleWALNeverShrank,
			build: func() *Record {
				r := baseRecord()
				r.Series = []measure.Series{ramp("disk.sqlite_wal_bytes", "bytes", 10, time.Minute, 0, 0)}
				return r
			},
		},
		{
			name: "ws drops", rule: RuleWSDrops, severity: SeverityError, fire: true,
			build: func() *Record {
				r := baseRecord()
				r.Counters[CounterWSDrops] = 2
				return r
			},
		},
		{
			name: "no ws drops", rule: RuleWSDrops,
			build: func() *Record {
				r := baseRecord()
				r.Counters[CounterWSDrops] = 0
				return r
			},
		},
		{
			name: "rate limited", rule: RuleRateLimited, severity: SeverityError, fire: true,
			build: func() *Record {
				r := baseRecord()
				r.Counters[CounterSubmitStatusPrefix+"429"] = 17
				return r
			},
		},
		{
			name: "not rate limited", rule: RuleRateLimited,
			build: func() *Record {
				r := baseRecord()
				r.Counters[CounterSubmitStatusPrefix+"201"] = 17
				return r
			},
		},
		{
			name: "logs stream within 5% of the configured cap", rule: RuleStreamAtCap, severity: SeverityWarn, fire: true,
			build: func() *Record {
				r := withCap(baseRecord(), 100)
				r.Series = []measure.Series{values(logs, "bytes", time.Minute, 10<<20, 96<<20, 96<<20)}
				return r
			},
		},
		{
			name: "logs stream at the server default cap", rule: RuleStreamAtCap, severity: SeverityWarn, fire: true,
			build: func() *Record {
				r := baseRecord() // no override: nats.embedded.max_store_mb defaults to 1024
				r.Series = []measure.Series{values(logs, "bytes", time.Minute, 10<<20, 1000<<20)}
				return r
			},
		},
		{
			name: "logs stream well under the cap", rule: RuleStreamAtCap,
			build: func() *Record {
				r := withCap(baseRecord(), 100)
				r.Series = []measure.Series{values(logs, "bytes", time.Minute, 10<<20, 90<<20)}
				return r
			},
		},
		{
			name: "logs stream under the default cap", rule: RuleStreamAtCap,
			build: func() *Record {
				r := baseRecord()
				r.Series = []measure.Series{values(logs, "bytes", time.Minute, 10<<20, 96<<20)}
				return r
			},
		},
		{
			name: "workers dip with no churn", rule: RuleWorkersOffline, severity: SeverityError, fire: true,
			build: func() *Record {
				r := baseRecord()
				r.Counters[CounterWorkersLive] = 100
				r.Series = []measure.Series{ramp("workers.online", "workers", 20, 30*time.Second, 100, 0)}
				r.Series[0].Points[15].V = 90
				return r
			},
		},
		{
			name: "workers dip under churn", rule: RuleWorkersOffline,
			build: func() *Record {
				r := baseRecord()
				r.Scenario.Workers[0].Churn = scenario.Churn{CrashPerHour: 2}
				r.Counters[CounterWorkersLive] = 100
				r.Series = []measure.Series{ramp("workers.online", "workers", 20, 30*time.Second, 100, 0)}
				r.Series[0].Points[15].V = 90
				return r
			},
		},
		{
			name: "workers below the fleet only during ramp", rule: RuleWorkersOffline,
			build: func() *Record {
				r := baseRecord()
				r.Counters[CounterWorkersLive] = 100
				r.Series = []measure.Series{ramp("workers.online", "workers", 20, 30*time.Second, 100, 0)}
				r.Series[0].Points[1].V = 40 // inside the warmup
				return r
			},
		},
		{
			name: "workers offline only after the run ended", rule: RuleWorkersOffline,
			build: func() *Record {
				// The observer's final sample is taken after the fleet stops.
				r := baseRecord()
				r.Counters[CounterWorkersLive] = 100
				r.Series = []measure.Series{ramp("workers.online", "workers", 20, 30*time.Second, 100, 0)}
				r.Series[0].Points = append(r.Series[0].Points, measure.Point{T: r.Ended.Add(30 * time.Second), V: 0})
				return r
			},
		},
		{
			name: "queue depth that never drains is not a finding", rule: "any",
			build: func() *Record {
				// F5: the server keeps a drained queue's last non-zero depth.
				r := baseRecord()
				r.Series = []measure.Series{values("sched.queue_depth{default}", "tasks", time.Minute, 5, 900, 900, 900, 900, 900)}
				return r
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Findings(tc.build(), steady)
			var hit *Finding
			for i := range got {
				if tc.rule == "any" || got[i].Rule == tc.rule {
					hit = &got[i]
				}
			}
			if tc.fire {
				if hit == nil {
					t.Fatalf("rule %s did not fire; findings: %+v", tc.rule, got)
				}
				if hit.Severity != tc.severity || hit.Message == "" {
					t.Fatalf("finding = %+v, want severity %s and a message", *hit, tc.severity)
				}
				return
			}
			if hit != nil {
				t.Fatalf("rule %s fired: %+v", tc.rule, *hit)
			}
		})
	}
}

// rampFrom is ramp starting at start instead of t0.
func rampFrom(start time.Time, name, unit string, n int, step time.Duration, v0, inc float64) measure.Series {
	s := ramp(name, unit, n, step, v0, inc)
	for i := range s.Points {
		s.Points[i].T = start.Add(time.Duration(i) * step)
	}
	return s
}

func TestFindings_GrowthGuards(t *testing.T) {
	steady := t0.Add(2 * time.Minute)
	zigzag := rampFrom(steady, "server.rss_bytes", "bytes", 10, 4*time.Minute, 0, 0)
	for i := range zigzag.Points {
		zigzag.Points[i].V = float64(i%2) * 2 * (1 << 30)
	}
	for _, tc := range []struct {
		name string
		rule string
		s    measure.Series
		fire bool
	}{
		{"goroutines: 9 points do not fire", RuleGoroutineGrowth, rampFrom(steady, "server.goroutines", "count", 9, 4*time.Minute, 1000, 5), false},
		{"goroutines: 10 points fire", RuleGoroutineGrowth, rampFrom(steady, "server.goroutines", "count", 10, 4*time.Minute, 1000, 5), true},
		{"rss: 9 points do not fire", RuleRSSGrowth, rampFrom(steady, "server.rss_bytes", "bytes", 9, 4*time.Minute, 500<<20, 100<<20), false},
		{"rss: 10 points fire", RuleRSSGrowth, rampFrom(steady, "server.rss_bytes", "bytes", 10, 4*time.Minute, 500<<20, 100<<20), true},
		// 29 minutes of steady state is too short to project, however steep.
		{"goroutines: under 30 minutes does not fire", RuleGoroutineGrowth, rampFrom(steady, "server.goroutines", "count", 30, time.Minute, 1000, 5), false},
		{"rss: under 30 minutes does not fire", RuleRSSGrowth, rampFrom(steady, "server.rss_bytes", "bytes", 30, time.Minute, 500<<20, 100<<20), false},
		// Alternating 0 and 2 GiB has a positive slope that projects to
		// terabytes a week, but its r² is about 0.03: noise, not a trend.
		{"rss: a steep slope with a low r2 does not fire", RuleRSSGrowth, zigzag, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := longRecord()
			r.Series = []measure.Series{tc.s}
			fired := false
			for _, f := range Findings(r, steady) {
				fired = fired || f.Rule == tc.rule
			}
			if fired != tc.fire {
				t.Fatalf("fired = %v, want %v", fired, tc.fire)
			}
		})
	}
}

func TestFindings_NilAndEmpty(t *testing.T) {
	if got := Findings(nil, t0); len(got) != 0 {
		t.Fatalf("nil record: %+v", got)
	}
	r := baseRecord()
	r.Scenario = nil
	if got := Findings(r, t0); len(got) != 0 {
		t.Fatalf("empty record: %+v", got)
	}
}

func TestStreamCapMB(t *testing.T) {
	for _, tc := range []struct {
		name string
		sc   *scenario.Scenario
		want float64
	}{
		{"nil scenario", nil, float64(defaultMaxStoreMB)},
		{"no config", &scenario.Scenario{}, float64(defaultMaxStoreMB)},
		{"int", &scenario.Scenario{Server: scenario.ServerSpec{Config: map[string]any{"nats": map[string]any{"embedded": map[string]any{"max_store_mb": 64}}}}}, 64},
		{"float", &scenario.Scenario{Server: scenario.ServerSpec{Config: map[string]any{"nats": map[string]any{"embedded": map[string]any{"max_store_mb": 64.0}}}}}, 64},
		{"wrong type", &scenario.Scenario{Server: scenario.ServerSpec{Config: map[string]any{"nats": map[string]any{"embedded": map[string]any{"max_store_mb": "big"}}}}}, float64(defaultMaxStoreMB)},
		{"wrong nesting", &scenario.Scenario{Server: scenario.ServerSpec{Config: map[string]any{"nats": "x"}}}, float64(defaultMaxStoreMB)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := StreamCapMB(tc.sc); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// longRecord is baseRecord running an hour, long enough for the growth rules.
func longRecord() *Record {
	r := baseRecord()
	r.Ended = t0.Add(time.Hour)
	return r
}

// TestFindings_RSSGrowthNeedsThirtyMinutes: the same steep, perfectly linear
// RSS growth fires on a 31-minute steady state and not on a 1-minute run.
func TestFindings_RSSGrowthNeedsThirtyMinutes(t *testing.T) {
	const perSecond = 2 << 20 // 7 GiB/h
	for _, tc := range []struct {
		name  string
		span  time.Duration
		step  time.Duration
		fires bool
	}{
		{"1-minute run", time.Minute, 5 * time.Second, false},
		{"31-minute run", 31 * time.Minute, time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := baseRecord()
			r.Scenario.Warmup.Duration = 5 * time.Second
			steady := r.SteadyFrom()
			n := int(tc.span/tc.step) + 1
			r.Ended = steady.Add(tc.span)
			r.Series = []measure.Series{rampFrom(steady, "server.rss_bytes", "bytes", n, tc.step, 500<<20, perSecond*tc.step.Seconds())}
			fired := false
			for _, f := range Findings(r, steady) {
				fired = fired || f.Rule == RuleRSSGrowth
			}
			if fired != tc.fires {
				t.Fatalf("rss-growth fired = %v, want %v", fired, tc.fires)
			}
		})
	}
}

// TestRecord_PointsAfterEndedIgnored: the observer's last sample, taken after
// the fleet stopped (after Ended), moves no peak, end value, slope or rule.
func TestRecord_PointsAfterEndedIgnored(t *testing.T) {
	build := func(withLate bool) *Record {
		r := longRecord()
		gor := ramp("server.goroutines", "count", 40, time.Minute, 100, 0)
		cpu := ramp("server.cpu_cores", "cores", 40, time.Minute, 1, 0)
		if withLate {
			late := r.Ended.Add(200 * time.Millisecond)
			gor.Points = append(gor.Points, measure.Point{T: late, V: 99999})
			cpu.Points = append(cpu.Points, measure.Point{T: late, V: 77})
		}
		r.Series = []measure.Series{gor, cpu}
		return r
	}
	with, without := build(true), build(false)
	steady := with.SteadyFrom()
	if p, _ := seriesPeak(findSeries(with, "server.cpu_cores")); p != 1 {
		t.Errorf("peak cpu = %v, want 1: the late point counted", p)
	}
	if slope, _ := measure.Slope(findSeries(with, "server.goroutines").Points, steady); slope != 0 {
		t.Errorf("goroutine slope = %v, want 0", slope)
	}
	for _, f := range Findings(with, steady) {
		if f.Rule == RuleGoroutineGrowth {
			t.Errorf("the late point raised %+v", f)
		}
	}
	var a, b strings.Builder
	if err := writeSummary(&a, with); err != nil {
		t.Fatal(err)
	}
	if err := writeSummary(&b, without); err != nil {
		t.Fatal(err)
	}
	if a.String() != b.String() {
		t.Errorf("a point after Ended changed the summary:\n%s\nvs\n%s", a.String(), b.String())
	}
	for _, d := range Compare(without, with) {
		if strings.HasPrefix(d.Metric, "peak.") && d.Verdict != VerdictNoChange {
			t.Errorf("compare %s = %+v, want no change", d.Metric, d)
		}
	}
}

func TestFindings_RejectedMessage(t *testing.T) {
	r := baseRecord()
	r.Counters = map[string]int64{"submit.status.201": 96, "submit.status.429": 3, "submit.status.0": 1}
	for _, f := range Findings(r, t0) {
		if f.Rule != RuleRejected {
			continue
		}
		for _, want := range []string{"4 of 100", "4.0%", "429 x3", "0 (no answer) x1"} {
			if !strings.Contains(f.Message, want) {
				t.Errorf("message %q lacks %q", f.Message, want)
			}
		}
		if f.Severity != SeverityError {
			t.Errorf("4%% refused is %s, want error", f.Severity)
		}
		return
	}
	t.Fatal("submissions-rejected did not fire")
}

func TestRecord_NoneAccepted(t *testing.T) {
	withJobs := func(submitted int64) *Record {
		r := baseRecord()
		r.Scenario.Jobs = []scenario.JobClass{{Name: "shot"}}
		r.Counters = map[string]int64{CounterJobsSubmitted: submitted}
		return r
	}
	noJobs := baseRecord()
	for _, tc := range []struct {
		name string
		r    *Record
		want bool
	}{
		{"jobs offered, none accepted", withJobs(0), true},
		{"jobs offered and accepted", withJobs(3), false},
		{"a scenario with no jobs", noJobs, false},
	} {
		if got := tc.r.NoneAccepted(); got != tc.want {
			t.Errorf("%s: NoneAccepted = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// drained is a ten minute run whose jobs took drain to finish after
// submissions stopped, with backlog.
func drained(drain time.Duration, backlog *Backlog) *Record {
	r := baseRecord()
	r.Scenario.Duration.Duration = 10 * time.Minute
	r.WindowEnd = t0.Add(10 * time.Minute)
	r.Ended = r.WindowEnd.Add(drain)
	r.Backlog = backlog
	return r
}

func TestFindings_FellBehindMessage(t *testing.T) {
	r := drained(4*time.Minute, &Backlog{Open: 900, TailCompleted: 60, TailFrom: t0.Add(9 * time.Minute)})
	for _, f := range Findings(r, t0) {
		if f.Rule != RuleFellBehind {
			continue
		}
		for _, want := range []string{"4m0s to drain", "25% of the 10m0s duration", "900 tasks were still open", "the 60 it finished in the window's last 1m0s"} {
			if !strings.Contains(f.Message, want) {
				t.Errorf("message %q lacks %q", f.Message, want)
			}
		}
		return
	}
	t.Fatal("server-fell-behind did not fire")
}
