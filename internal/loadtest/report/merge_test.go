// SPDX-License-Identifier: AGPL-3.0-or-later

package report

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/measure"
	"github.com/uberware/sqi/internal/loadtest/verify"
)

func shardRecord(shard string, jobs, terminal int64, latencies ...time.Duration) *Record {
	r := baseRecord()
	r.Shard = shard
	r.Counters["jobs.submitted"] = jobs
	r.Counters["tasks.terminal"] = terminal
	r.Histograms[HistFirstLease] = hist(latencies...)
	r.Series = []measure.Series{values("server.rss_bytes", "bytes", time.Minute, 1, 2)}
	r.Invariants = []verify.Result{okResult("terminal"), okResult("logs")}
	return r
}

func TestMerge(t *testing.T) {
	s0 := shardRecord("0/2", 10, 100, 10*time.Millisecond, 20*time.Millisecond)
	s1 := shardRecord("1/2", 5, 50, 30*time.Millisecond, 40*time.Millisecond)
	s1.Series = []measure.Series{values("server.rss_bytes", "bytes", time.Minute, 999)}
	s1.Started, s1.Ended = t0.Add(time.Second), t0.Add(11*time.Minute)

	got, err := Merge([]*Record{s1, s0}) // order does not matter
	if err != nil {
		t.Fatal(err)
	}
	if got.Counters["jobs.submitted"] != 15 || got.Counters["tasks.terminal"] != 150 {
		t.Errorf("counters = %v", got.Counters)
	}
	if h := got.Histograms[HistFirstLease]; h.Count() != 4 || h.Min() != 10*time.Millisecond || h.Max() != 40*time.Millisecond {
		t.Errorf("histogram: n=%d min=%v max=%v", h.Count(), h.Min(), h.Max())
	}
	if len(got.Series) != 1 || got.Series[0].Points[1].V != 2 {
		t.Errorf("series must come from shard 0/2: %+v", got.Series)
	}
	if !got.Started.Equal(t0) || !got.Ended.Equal(t0.Add(11*time.Minute)) {
		t.Errorf("window = %v .. %v", got.Started, got.Ended)
	}
	if !got.Valid || !got.Complete {
		t.Errorf("valid=%v complete=%v", got.Valid, got.Complete)
	}
	if got.RunID != "run-1" {
		t.Errorf("run id = %q", got.RunID)
	}
	// The inputs are untouched.
	if s0.Counters["tasks.terminal"] != 100 || s0.Histograms[HistFirstLease].Count() != 2 {
		t.Error("Merge modified its input")
	}
}

func TestMerge_Errors(t *testing.T) {
	for _, tc := range []struct {
		name string
		rs   func() []*Record
		want string
	}{
		{"none", func() []*Record { return nil }, "no records"},
		{"mismatched run id", func() []*Record {
			a, b := shardRecord("0/2", 1, 1), shardRecord("1/2", 1, 1)
			b.RunID = "other"
			return []*Record{a, b}
		}, "run id"},
		{"no shard 0", func() []*Record {
			return []*Record{shardRecord("1/3", 1, 1), shardRecord("2/3", 1, 1)}
		}, "0/"},
		{"duplicate shard", func() []*Record {
			return []*Record{shardRecord("0/2", 1, 1), shardRecord("0/2", 1, 1)}
		}, "twice"},
		{"nil record", func() []*Record { return []*Record{nil} }, "nil"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Merge(tc.rs())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestMerge_InvariantsValidityAndMissingShards(t *testing.T) {
	s0 := shardRecord("0/3", 1, 1)
	s1 := shardRecord("1/3", 1, 1)
	s1.Invariants = []verify.Result{okResult("terminal"), {Name: "logs", OK: false, Evidence: []string{"lost a line"}}}
	s1.Valid, s1.InvalidReason = false, "generator saturated"
	s1.Complete = false
	s0.Unavailable = []string{"pg.db_bytes"}
	s1.Unavailable = []string{"pg.db_bytes", "js.bytes"}

	got, err := Merge([]*Record{s0, s1})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]verify.Result{}
	for _, inv := range got.Invariants {
		by[inv.Name] = inv
	}
	if !by["terminal"].OK || by["logs"].OK {
		t.Errorf("invariants = %+v", got.Invariants)
	}
	if ev := by["logs"].Evidence; len(ev) != 1 || !strings.Contains(ev[0], "lost a line") || !strings.Contains(ev[0], "1/3") {
		t.Errorf("evidence = %v", ev)
	}
	if got.Valid || !strings.Contains(got.InvalidReason, "generator saturated") {
		t.Errorf("valid=%v reason=%q", got.Valid, got.InvalidReason)
	}
	if got.Complete {
		t.Error("an incomplete shard makes the merge incomplete")
	}
	joined := strings.Join(got.Unavailable, "|")
	if strings.Count(joined, "pg.db_bytes") != 1 || !strings.Contains(joined, "js.bytes") {
		t.Errorf("unavailable = %v", got.Unavailable)
	}
	if !strings.Contains(joined, "2 of 3 shards") {
		t.Errorf("a missing shard must be reported: %v", got.Unavailable)
	}
}

func TestMerge_SteadyCounters(t *testing.T) {
	s0, s1 := shardRecord("0/2", 10, 100), shardRecord("1/2", 5, 50)
	s0.SteadyCounters = map[string]int64{"tasks.terminal": 20, "jobs.submitted": 2}
	s1.SteadyCounters = map[string]int64{"tasks.terminal": 10}
	got, err := Merge([]*Record{s0, s1})
	if err != nil {
		t.Fatal(err)
	}
	if got.SteadyCounters["tasks.terminal"] != 30 || got.SteadyCounters["jobs.submitted"] != 2 {
		t.Errorf("steady counters = %v", got.SteadyCounters)
	}
	// A shard without a snapshot makes the merged rates run averages.
	s1.SteadyCounters = nil
	got, err = Merge([]*Record{s0, s1})
	if err != nil {
		t.Fatal(err)
	}
	if got.SteadyCounters != nil {
		t.Errorf("steady counters = %v, want none", got.SteadyCounters)
	}
}

func TestMerge_EvidenceCap(t *testing.T) {
	s0, s1 := shardRecord("0/2", 1, 1), shardRecord("1/2", 1, 1)
	var ev []string
	for i := range 15 {
		ev = append(ev, fmt.Sprintf("e%d", i))
	}
	s0.Invariants = []verify.Result{{Name: "logs", Evidence: ev}}
	s1.Invariants = []verify.Result{{Name: "logs", Evidence: ev}}
	got, err := Merge([]*Record{s0, s1})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(got.Invariants[0].Evidence); n != maxEvidence {
		t.Fatalf("evidence entries = %d, want %d", n, maxEvidence)
	}
	if last := got.Invariants[0].Evidence[maxEvidence-1]; !strings.Contains(last, "11 more") {
		t.Fatalf("last entry = %q", last)
	}
}

func TestMerge_SingleUnshardedRecord(t *testing.T) {
	r := shardRecord("", 3, 30, time.Millisecond)
	got, err := Merge([]*Record{r})
	if err != nil {
		t.Fatal(err)
	}
	if got.Counters["tasks.terminal"] != 30 {
		t.Fatalf("%v", got.Counters)
	}
}

func TestMerge_WindowCounters(t *testing.T) {
	a, b := baseRecord(), baseRecord()
	a.Shard, b.Shard = "0/2", "1/2"
	a.WindowEnd, b.WindowEnd = t0.Add(8*time.Minute), t0.Add(8*time.Minute+time.Second)
	a.WindowCounters = map[string]int64{CounterJobsSubmitted: 10}
	b.WindowCounters = map[string]int64{CounterJobsSubmitted: 15}
	m, err := Merge([]*Record{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if m.WindowCounters[CounterJobsSubmitted] != 25 || !m.WindowEnd.Equal(b.WindowEnd) {
		t.Errorf("merged window counters %v ending %s, want 25 ending at the later shard's %s", m.WindowCounters, m.WindowEnd, b.WindowEnd)
	}
	b.WindowCounters = nil
	if m, err = Merge([]*Record{a, b}); err != nil || m.WindowCounters != nil {
		t.Errorf("a shard without window counters: merged %v (err %v), want nil", m.WindowCounters, err)
	}
}
