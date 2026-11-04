// SPDX-License-Identifier: AGPL-3.0-or-later

package observe

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/loadtest/measure"
)

// Generator validity thresholds: a run is invalid when the generator itself
// was the bottleneck for more than maxSaturatedFraction of the steady-state
// window.
const (
	maxCPUFraction       = 0.85
	maxLagP99            = time.Second
	maxSaturatedFraction = 0.10
)

// lagPeriod is the ticker period the schedule-lag sampler measures against.
const lagPeriod = 100 * time.Millisecond

type healthPoint struct {
	t    time.Time
	cpu  float64 // cores
	lagS float64 // p99 schedule lag over the interval, seconds
}

// SelfHealth accumulates the generator's own CPU and schedule lag and decides
// whether the run it observed was valid. It is safe for concurrent use.
type SelfHealth struct {
	mu     sync.Mutex
	points []healthPoint
}

// NewSelfHealth returns an empty SelfHealth.
func NewSelfHealth() *SelfHealth { return &SelfHealth{} }

// Record adds one interval: the generator's CPU use in cores and the p99
// lateness of its timers in seconds.
func (h *SelfHealth) Record(t time.Time, cpuCores, lagP99 float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.points = append(h.points, healthPoint{t: t, cpu: cpuCores, lagS: lagP99})
}

// Valid reports whether the generator kept up, judged over the samples taken
// at or after steadyFrom on a machine with the given number of cores
// (non-positive means runtime.NumCPU). A run is invalid when CPU use exceeded 85% of cores, or
// p99 schedule lag exceeded one second, in more than 10% of those samples. The
// reason is empty for a valid run, and names every threshold hit, and by how
// much, for an invalid one. With no samples to judge it reports valid and says
// so.
func (h *SelfHealth) Valid(steadyFrom time.Time, cores int) (valid bool, reason string) {
	if cores <= 0 {
		cores = runtime.NumCPU()
	}
	h.mu.Lock()
	var window []healthPoint
	for _, p := range h.points {
		if !p.t.Before(steadyFrom) {
			window = append(window, p)
		}
	}
	h.mu.Unlock()
	if len(window) == 0 {
		return true, "no generator samples in the steady-state window; saturation not checked"
	}

	limitCPU := maxCPUFraction * float64(cores)
	var cpuOver, lagOver int
	var peakCPU, peakLag float64
	for _, p := range window {
		if p.cpu > limitCPU {
			cpuOver++
		}
		if p.lagS > maxLagP99.Seconds() {
			lagOver++
		}
		peakCPU = math.Max(peakCPU, p.cpu)
		peakLag = math.Max(peakLag, p.lagS)
	}

	var reasons []string
	if frac := float64(cpuOver) / float64(len(window)); frac > maxSaturatedFraction {
		reasons = append(reasons, fmt.Sprintf(
			"generator CPU above %.0f%% of %d cores (%.2f cores) in %.0f%% of %d steady-state samples (limit %.0f%%); peak %.2f cores",
			maxCPUFraction*100, cores, limitCPU, frac*100, len(window), maxSaturatedFraction*100, peakCPU,
		))
	}
	if frac := float64(lagOver) / float64(len(window)); frac > maxSaturatedFraction {
		reasons = append(reasons, fmt.Sprintf(
			"generator schedule lag p99 above %s in %.0f%% of %d steady-state samples (limit %.0f%%); peak %.2f s",
			maxLagP99, frac*100, len(window), maxSaturatedFraction*100, peakLag,
		))
	}
	if len(reasons) == 0 {
		return true, ""
	}
	return false, strings.Join(reasons, "; ")
}

// percentile returns the q quantile of xs by nearest rank, 0 for none.
func percentile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	rank := max(int(math.Ceil(q*float64(len(s)))), 1)
	return s[min(rank, len(s))-1]
}

// lagSampler measures how late a timer fires: it sleeps to successive
// deadlines one period apart and records how far past each it woke.
type lagSampler struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	lateness []float64
}

// startLagSampler starts the sampler's goroutine; stop ends it.
func startLagSampler(ctx context.Context, period time.Duration) *lagSampler {
	ctx, cancel := context.WithCancel(ctx)
	l := &lagSampler{cancel: cancel, done: make(chan struct{})}
	go l.run(ctx, period)
	return l
}

func (l *lagSampler) run(ctx context.Context, period time.Duration) {
	defer close(l.done)
	timer := time.NewTimer(period)
	defer timer.Stop()
	deadline := time.Now().Add(period)
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		now := time.Now()
		late := max(now.Sub(deadline), 0)
		l.mu.Lock()
		l.lateness = append(l.lateness, late.Seconds())
		l.mu.Unlock()

		// Keep the cadence; if the wakeup was more than a period late, the
		// missed deadlines are not owed.
		deadline = deadline.Add(period)
		if deadline.Before(now) {
			deadline = now.Add(period)
		}
		timer.Reset(time.Until(deadline))
	}
}

// drain returns the p99 lateness, in seconds, of the wakeups since the last
// drain, and how many there were.
func (l *lagSampler) drain() (p99 float64, n int) {
	l.mu.Lock()
	xs := l.lateness
	l.lateness = nil
	l.mu.Unlock()
	return percentile(xs, 0.99), len(xs)
}

func (l *lagSampler) stop() {
	l.cancel()
	<-l.done
}

// genSampler samples the load generator itself: its CPU, goroutines and
// schedule lag.
type genSampler struct {
	rec    *measure.Recorder
	health *SelfHealth
	lag    *lagSampler

	// now and cpu are the clock and the process CPU source; tests replace
	// them. sample reads both back to back, so a rate's divisor is the time
	// between the two CPU reads and nothing else.
	now func() time.Time
	cpu func() (time.Duration, error)

	havePrev bool
	prevAt   time.Time
	prevCPU  time.Duration
}

func newGenSampler(ctx context.Context, rec *measure.Recorder, health *SelfHealth, period time.Duration) *genSampler {
	return &genSampler{rec: rec, health: health, lag: startLagSampler(ctx, period), now: time.Now, cpu: processCPUTime}
}

func (g *genSampler) stop() { g.lag.stop() }

// sample records the generator's goroutine count and, from the second sample,
// its CPU use and schedule lag over the interval since the previous one. It
// takes its own timestamp, at the instant it reads the process CPU time: the
// caller may have spent seconds in network calls beforehand, and a timestamp
// from before them would turn that wait into a wrong CPU rate.
func (g *genSampler) sample() {
	cpu, err := g.cpu()
	t := g.now()
	g.rec.Add("gen.goroutines", "count", t, float64(runtime.NumGoroutine()))
	lagP99, lagN := g.lag.drain()
	if err == nil && g.havePrev {
		if dt := t.Sub(g.prevAt).Seconds(); dt > 0 {
			cores := (cpu - g.prevCPU).Seconds() / dt
			g.rec.Add("gen.cpu_cores", "cores", t, cores)
			if lagN > 0 {
				g.rec.Add("gen.sched_lag_p99_s", "s", t, lagP99)
				g.health.Record(t, cores, lagP99)
			}
		}
	}
	if err == nil {
		g.havePrev, g.prevAt, g.prevCPU = true, t, cpu
	}
}
