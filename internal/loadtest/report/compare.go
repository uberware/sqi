// SPDX-License-Identifier: AGPL-3.0-or-later

package report

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
)

// Compare verdicts.
const (
	VerdictBetter   = "better"
	VerdictWorse    = "worse"
	VerdictNoChange = "no change"
)

// noiseBandPct is the half-width of the band, in percent, inside which a
// difference is reported as "no change". The spec leaves the band to the
// author; a run's own variance is not modeled (a repeated run is compared
// run against run).
const noiseBandPct = 5.0

// Delta is one metric's change from run A to run B.
type Delta struct {
	Metric string
	A, B   float64
	// Pct is (B-A)/A*100. When A is zero it is 0 if B is zero too, and +Inf
	// otherwise (a metric that is "new" in B).
	Pct     float64
	Verdict string
}

// direction says which way a metric improves.
type direction int

const (
	skip direction = iota
	lowerBetter
	higherBetter
)

// peakSeries are the server series whose peak Compare reports. Every one is a
// resource, so lower is better. Other series (queue depth, worker counts, disk
// sizes, generator health) are not compared: their direction is not a
// property of the server.
var peakSeries = []string{
	"server.cpu_cores", "server.rss_bytes", "server.goroutines",
	"server.open_fds", "server.heap_bytes", "server.gc_pause_p99_s",
}

// counterDirection is the direction table for counters:
//
//   - higher is better: tasks.terminal, jobs.submitted, and a submit.status.<code>
//     counter for a 2xx status (accepted submissions);
//   - lower is better: ws.drops, ws.errors, rest.errors and a submit.status.<code>
//     counter for any other status;
//   - not compared: everything else (workers.live is a fleet size, ws.events
//     has no good direction, and an unknown counter has no defined one).
//
// Every histogram quantile and every peak series is lower-is-better.
func counterDirection(name string) direction {
	switch name {
	case CounterTasksTerminal, CounterJobsSubmitted:
		return higherBetter
	case CounterWSDrops, CounterWSErrors, CounterRESTErrors, CounterWSLagNegative:
		return lowerBetter
	}
	if code, ok := strings.CutPrefix(name, CounterSubmitStatusPrefix); ok {
		if strings.HasPrefix(code, "2") {
			return higherBetter
		}
		return lowerBetter
	}
	return skip
}

// Compare returns the per-metric deltas from a to b: the p50, p95 and p99 of
// every histogram both runs have, every comparable counter (as a per-second
// steady-window rate when both runs carry a SteadyCounters snapshot, as a
// per-second run average when either does not, and as a total when a run has no
// duration) and
// the peaks of the key server series. A difference of at most 5% either way is
// "no change". Deltas come in a fixed order: histograms, counters, peaks.
func Compare(a, b *Record) []Delta {
	var out []Delta
	out = append(out, compareHistograms(a, b)...)
	out = append(out, compareCounters(a, b)...)
	out = append(out, comparePeaks(a, b)...)
	return out
}

func compareHistograms(a, b *Record) []Delta {
	var out []Delta
	for _, k := range slices.Sorted(maps.Keys(a.Histograms)) {
		ha, hb := a.Histograms[k], b.Histograms[k]
		if ha == nil || hb == nil || ha.Count() == 0 || hb.Count() == 0 {
			continue
		}
		for _, q := range []struct {
			label string
			q     float64
		}{{"p50", 0.5}, {"p95", 0.95}, {"p99", 0.99}} {
			out = append(out, newDelta(k+"."+q.label, ha.Quantile(q.q).Seconds(), hb.Quantile(q.q).Seconds(), lowerBetter))
		}
	}
	return out
}

func compareCounters(a, b *Record) []Delta {
	names := map[string]bool{}
	for k := range a.Counters {
		names[k] = true
	}
	for k := range b.Counters {
		names[k] = true
	}
	// Rates are steady-window rates only when both runs can give one; mixing a
	// steady-window rate with a run average would bake the warmup into the
	// difference, so otherwise both are run averages.
	steady := a.hasSteadyBasis() && b.hasSteadyBasis()
	var out []Delta
	for _, k := range slices.Sorted(maps.Keys(names)) {
		dir := counterDirection(k)
		if dir == skip {
			continue
		}
		va, vb, label := float64(a.Counters[k]), float64(b.Counters[k]), k
		ra, okA := a.rate(k, steady)
		rb, okB := b.rate(k, steady)
		if okA && okB {
			va, vb, label = ra, rb, k+"/s"
		}
		out = append(out, newDelta(label, va, vb, dir))
	}
	return out
}

func comparePeaks(a, b *Record) []Delta {
	var out []Delta
	for _, name := range peakSeries {
		pa, okA := seriesPeak(findSeries(a, name))
		pb, okB := seriesPeak(findSeries(b, name))
		if okA && okB {
			out = append(out, newDelta("peak."+name, pa, pb, lowerBetter))
		}
	}
	return out
}

func newDelta(metric string, a, b float64, dir direction) Delta {
	d := Delta{Metric: metric, A: a, B: b}
	switch {
	case a == 0 && b == 0:
		d.Pct = 0
	case a == 0:
		d.Pct = math.Inf(1)
	default:
		d.Pct = (b - a) / a * 100
	}
	switch {
	case math.Abs(d.Pct) <= noiseBandPct+1e-9: // the epsilon absorbs float rounding at the band edge
		d.Verdict = VerdictNoChange
	case (d.Pct > 0) == (dir == higherBetter):
		d.Verdict = VerdictBetter
	default:
		d.Verdict = VerdictWorse
	}
	return d
}

// CompareWarnings returns a warning for each of the two records a comparison
// cannot trust: one that is INVALID, whose numbers describe a saturated
// generator rather than the server, or one that is incomplete, whose counters
// and histograms stop early. The deltas are still computed; the warnings say
// what they are worth.
func CompareWarnings(a, b *Record) []string {
	var out []string
	for _, x := range []struct {
		label string
		r     *Record
	}{{"A", a}, {"B", b}} {
		if !x.r.Valid {
			out = append(out, fmt.Sprintf("%s (%s) is INVALID (%s): its numbers describe the generator as much as the server", x.label, x.r.RunID, orDash(x.r.InvalidReason)))
		}
		if !x.r.Complete {
			out = append(out, fmt.Sprintf("%s (%s) is INCOMPLETE: it ended before its planned duration, so its totals and rates stop early", x.label, x.r.RunID))
		}
	}
	return out
}

// RenderDeltas formats deltas as an aligned text table.
func RenderDeltas(ds []Delta) string {
	var sb strings.Builder
	tw := tabwriter.NewWriter(&sb, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "METRIC\tA\tB\tCHANGE\tVERDICT")
	for _, d := range ds {
		change := fmt.Sprintf("%+.1f%%", d.Pct)
		if math.IsInf(d.Pct, 1) {
			change = "new"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", d.Metric, fmtNum(d.A), fmtNum(d.B), change, d.Verdict)
	}
	tw.Flush()
	return sb.String()
}

func fmtNum(v float64) string { return strconv.FormatFloat(v, 'g', 5, 64) }
