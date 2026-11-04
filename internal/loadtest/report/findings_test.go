// SPDX-License-Identifier: AGPL-3.0-or-later

package report

import (
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
				r := baseRecord()
				r.Series = []measure.Series{ramp("server.goroutines", "count", 30, 20*time.Second, 1000, 5)}
				return r
			},
		},
		{
			name: "goroutines flat", rule: RuleGoroutineGrowth,
			build: func() *Record {
				r := baseRecord()
				r.Series = []measure.Series{ramp("server.goroutines", "count", 30, 20*time.Second, 1000, 0)}
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
				r := baseRecord()
				// 10 MiB per minute is 600 MiB/h, 100 GiB over a week.
				r.Series = []measure.Series{ramp("server.rss_bytes", "bytes", 30, 20*time.Second, 500<<20, 10<<20/3)}
				return r
			},
		},
		{
			name: "rss projects under 1 GiB a week", rule: RuleRSSGrowth,
			build: func() *Record {
				r := baseRecord()
				// 1 KiB per sample is about 180 KiB/h, 30 MiB over a week.
				r.Series = []measure.Series{ramp("server.rss_bytes", "bytes", 30, 20*time.Second, 500<<20, 1024)}
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
	zigzag := rampFrom(steady, "server.rss_bytes", "bytes", 10, 20*time.Second, 0, 0)
	for i := range zigzag.Points {
		zigzag.Points[i].V = float64(i%2) * 2 * (1 << 30)
	}
	for _, tc := range []struct {
		name string
		rule string
		s    measure.Series
		fire bool
	}{
		{"goroutines: 9 points do not fire", RuleGoroutineGrowth, rampFrom(steady, "server.goroutines", "count", 9, 20*time.Second, 1000, 5), false},
		{"goroutines: 10 points fire", RuleGoroutineGrowth, rampFrom(steady, "server.goroutines", "count", 10, 20*time.Second, 1000, 5), true},
		{"rss: 9 points do not fire", RuleRSSGrowth, rampFrom(steady, "server.rss_bytes", "bytes", 9, 20*time.Second, 500<<20, 100<<20), false},
		{"rss: 10 points fire", RuleRSSGrowth, rampFrom(steady, "server.rss_bytes", "bytes", 10, 20*time.Second, 500<<20, 100<<20), true},
		// Alternating 0 and 2 GiB has a positive slope that projects to
		// terabytes a week, but its r² is about 0.03: noise, not a trend.
		{"rss: a steep slope with a low r2 does not fire", RuleRSSGrowth, zigzag, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := baseRecord()
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
			if got := streamCapMB(tc.sc); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
