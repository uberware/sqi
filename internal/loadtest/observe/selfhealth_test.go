// SPDX-License-Identifier: AGPL-3.0-or-later

package observe

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/measure"
)

func TestSelfHealth_Invalid(t *testing.T) {
	const (
		cores  = 4
		points = 100
	)
	// fill builds points samples one second apart; the first over are
	// saturated by the given CPU (in cores) and lag (in seconds).
	fill := func(over int, cpu, lag float64) *SelfHealth {
		h := NewSelfHealth()
		base := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
		for i := range points {
			c, l := 0.5, 0.01
			if i < over {
				c, l = cpu, lag
			}
			h.Record(base.Add(time.Duration(i)*time.Second), c, l)
		}
		return h
	}
	base := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		h          *SelfHealth
		steadyFrom time.Time
		wantValid  bool
		wantInMsg  []string
	}{
		{"CPU 90% of 4 cores for 20% of the window", fill(20, 3.6, 0.01), base, false, []string{"CPU", "20%"}},
		{"lag p99 1.5 s for 5%", fill(5, 0.5, 1.5), base, true, nil},
		{"lag p99 1.5 s for 20%", fill(20, 0.5, 1.5), base, false, []string{"lag", "1.5"}},
		{"both under", fill(0, 0.5, 0.01), base, true, nil},
		{"exactly 10% over is not more than 10%", fill(10, 3.6, 0.01), base, true, nil},
		{"both thresholds hit name both", fill(20, 3.6, 1.5), base, false, []string{"CPU", "lag"}},
		{"saturation before the steady window is ignored", fill(20, 3.6, 0.01), base.Add(20 * time.Second), true, nil},
		{"no samples in the window is valid but says so", NewSelfHealth(), base, true, []string{"no generator samples"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			valid, reason := tt.h.Valid(tt.steadyFrom, cores)
			if valid != tt.wantValid {
				t.Fatalf("Valid = %v (%q), want %v", valid, reason, tt.wantValid)
			}
			for _, want := range tt.wantInMsg {
				if !strings.Contains(reason, want) {
					t.Errorf("reason %q does not mention %q", reason, want)
				}
			}
			if tt.wantValid && len(tt.wantInMsg) == 0 && reason != "" {
				t.Errorf("a plainly valid run gave reason %q", reason)
			}
		})
	}
}

// TestSelfHealth_ValidBetween: samples after the end of the measured window
// (a teardown burst) do not count; with no end they do.
func TestSelfHealth_ValidBetween(t *testing.T) {
	base := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	h := NewSelfHealth()
	for i := range 5 {
		h.Record(base.Add(time.Duration(i)*time.Second), 0.1, 0.01)
	}
	h.Record(base.Add(4200*time.Millisecond), 3.9, 0.01) // after until
	until := base.Add(4 * time.Second)
	if valid, reason := h.ValidBetween(base, until, 4); !valid {
		t.Errorf("ValidBetween = invalid (%s), want valid: the late sample counted", reason)
	}
	if valid, _ := h.Valid(base, 4); valid {
		t.Error("Valid with no end ignored the saturated sample")
	}
}

func TestSelfHealth_ConcurrentRecord(*testing.T) {
	h := NewSelfHealth()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 100 {
				h.Record(time.Unix(int64(i*100+j), 0), 0.1, 0.01)
				h.Valid(time.Unix(0, 0), 4)
			}
		})
	}
	wg.Wait()
}

func TestLagSampler_MeasuresLateness(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	l := startLagSampler(ctx, 5*time.Millisecond)
	defer l.stop()

	// A stall cannot be forced portably, so check what can be: a running
	// sampler yields non-negative lateness and drain empties its window.
	deadline := time.Now().Add(5 * time.Second)
	for {
		p99, n := l.drain()
		if n > 0 {
			if p99 < 0 {
				t.Fatalf("negative p99 lag %v", p99)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lag sampler collected nothing in 5 s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, n := l.drain(); n > 10_000 {
		t.Fatalf("drain did not reset the window: %d", n)
	}
}

func TestPercentile(t *testing.T) {
	tests := []struct {
		name string
		in   []float64
		q    float64
		want float64
	}{
		{"empty", nil, 0.99, 0},
		{"single", []float64{3}, 0.99, 3},
		{"p99 of 100 is the 99th", seq(100), 0.99, 99},
		{"p50 of 4", []float64{4, 1, 3, 2}, 0.5, 2},
		{"max", []float64{1, 9, 5}, 1, 9},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := percentile(tt.in, tt.q); got != tt.want {
				t.Fatalf("percentile = %v, want %v", got, tt.want)
			}
		})
	}
}

func seq(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = float64(i + 1)
	}
	return out
}

func TestGenSampler_RecordsSelfHealth(t *testing.T) {
	rec := measure.NewRecorder()
	h := NewSelfHealth()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	g := newGenSampler(ctx, rec, h, 5*time.Millisecond)
	defer g.stop()

	t0 := time.Now()
	g.sample()
	if !hasSeries(rec, "gen.goroutines") {
		t.Fatal("first sample did not record gen.goroutines")
	}
	if hasSeries(rec, "gen.cpu_cores") {
		t.Fatal("first sample recorded a CPU rate with nothing to compare against")
	}

	// Burn CPU on this goroutine across the interval so the rate is positive.
	spinUntil := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(spinUntil) {
	}
	g.sample()
	if got := lastValue(t, rec, "gen.cpu_cores"); got <= 0 {
		t.Fatalf("gen.cpu_cores = %v after a busy interval, want > 0", got)
	}
	if !hasSeries(rec, "gen.sched_lag_p99_s") {
		t.Fatal("no gen.sched_lag_p99_s after a full interval")
	}
	if pointCount(rec, "gen.goroutines") != 2 {
		t.Fatalf("gen.goroutines has %d points, want 2", pointCount(rec, "gen.goroutines"))
	}
	if valid, reason := h.Valid(t0.Add(-time.Second), 1024); !valid {
		t.Fatalf("a tiny test run on 1024 cores is invalid: %s", reason)
	}
}

func TestGenSampler_RateUsesTheInstantsOfTheReads(t *testing.T) {
	rec := measure.NewRecorder()
	h := NewSelfHealth()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	g := newGenSampler(ctx, rec, h, time.Millisecond)
	defer g.stop()

	base := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	reads := []struct {
		at  time.Duration // clock when the CPU is read
		cpu time.Duration // CPU time the process has used
	}{
		{0, 0},
		{10 * time.Second, 5 * time.Second},  // 0.5 cores
		{12 * time.Second, 5 * time.Second},  // idle for 2 s: 0 cores
		{20 * time.Second, 13 * time.Second}, // 1 core over 8 s
	}
	var i int
	g.now = func() time.Time { return base.Add(reads[i].at) }
	g.cpu = func() (time.Duration, error) { return reads[i].cpu, nil }
	for i = range reads {
		time.Sleep(5 * time.Millisecond) // let the lag sampler collect something
		g.sample()
	}

	var cpu measure.Series
	for _, s := range rec.Series() {
		if s.Name == "gen.cpu_cores" {
			cpu = s
		}
	}
	want := []struct {
		at    time.Duration
		cores float64
	}{{10 * time.Second, 0.5}, {12 * time.Second, 0}, {20 * time.Second, 1}}
	if len(cpu.Points) != len(want) {
		t.Fatalf("gen.cpu_cores has %d points, want %d", len(cpu.Points), len(want))
	}
	for k, w := range want {
		p := cpu.Points[k]
		if !p.T.Equal(base.Add(w.at)) || math.Abs(p.V-w.cores) > 1e-9 {
			t.Errorf("point %d = (%v, %v), want (%v, %v)", k, p.T, p.V, base.Add(w.at), w.cores)
		}
	}
}
