// SPDX-License-Identifier: AGPL-3.0-or-later

package report

import (
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/loadtest/measure"
)

// gaugeCounters are counters that hold a level, not a count of events, so a
// per-second rate of them means nothing.
var gaugeCounters = map[string]bool{CounterWorkersLive: true}

// growthSeries are the series whose slope the Growth section reports, as exact
// names or, ending in "{", as the prefix of a labeled family.
var growthSeries = []string{
	"server.rss_bytes", "server.goroutines", "server.open_fds",
	"disk.sqlite_db_bytes", "disk.sqlite_wal_bytes", "disk.nats_bytes", "pg.db_bytes",
	"js.bytes{",
}

// resourceSeries are the series the Server resources section reports, in the
// same notation as growthSeries.
var resourceSeries = []string{
	"server.", "disk.", "pg.", "js.bytes{", "http.p95_s{",
	"workers.online", "sched.idle_workers", "sched.queue_depth{",
}

func matchesAny(name string, patterns []string) bool {
	for _, p := range patterns {
		if name == p || (strings.HasSuffix(p, "{") || strings.HasSuffix(p, ".")) && strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// writeSummary renders the record as summary.md.
func writeSummary(w io.Writer, r *Record) error {
	var b strings.Builder
	summaryHeader(&b, r)
	summaryValidity(&b, r)
	summaryThroughput(&b, r)
	summaryLatency(&b, r)
	summaryResources(&b, r)
	summaryGrowth(&b, r)
	summaryInvariants(&b, r)
	summaryFindings(&b, r)
	summaryUnavailable(&b, r)
	_, err := io.WriteString(w, b.String())
	return err
}

func summaryHeader(b *strings.Builder, r *Record) {
	fmt.Fprintf(b, "# sqi-load run %s\n\n", r.RunID)
	name := "(none)"
	if r.Scenario != nil && r.Scenario.Name != "" {
		name = r.Scenario.Name
	}
	fmt.Fprintf(b, "- Scenario: %s (%s)\n", name, r.Mode)
	if r.Shard != "" {
		fmt.Fprintf(b, "- Shard: %s\n", r.Shard)
	}
	fmt.Fprintf(b, "- Run: %s to %s (%s); warmup %s\n",
		r.Started.UTC().Format(time.RFC3339), r.Ended.UTC().Format(time.RFC3339),
		fmtDur(r.Ended.Sub(r.Started)), fmtDur(r.warmup()))
	if !r.WindowEnd.IsZero() {
		fmt.Fprintf(b, "- Submissions stopped: %s (%s in); drain %s\n",
			r.WindowEnd.UTC().Format(time.RFC3339), fmtDur(r.WindowEnd.Sub(r.Started)), fmtDur(r.Drain()))
	}
	fmt.Fprintf(b, "- Environment: %s/%s, %d CPUs, sqi %s (%s), store %s\n",
		r.Env.OS, r.Env.Arch, r.Env.CPUs, orDash(r.Env.SQIVersion), orDash(r.Env.Commit), orDash(r.Env.Store))
	if len(r.Env.Flags) > 0 {
		fmt.Fprintf(b, "- Flags: %s\n", strings.Join(r.Env.Flags, " "))
	}
	b.WriteString("\n")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func seriesPeak(s *measure.Series) (peak float64, ok bool) {
	if s == nil || len(s.Points) == 0 {
		return 0, false
	}
	peak = math.Inf(-1)
	for _, p := range s.Points {
		peak = math.Max(peak, p.V)
	}
	return peak, true
}

func summaryValidity(b *strings.Builder, r *Record) {
	b.WriteString("## Validity\n\n")
	if r.Valid {
		b.WriteString("- VALID: the load generator kept up.\n")
	} else {
		fmt.Fprintf(b, "- INVALID: %s\n", orDash(r.InvalidReason))
		b.WriteString("  The numbers below describe a saturated or failed generator, not the server.\n")
	}
	if !r.Complete {
		b.WriteString("- INCOMPLETE: the run ended before its planned duration.\n")
	}
	var gen []string
	if v, ok := seriesPeak(findSeries(r, "gen.cpu_cores")); ok {
		gen = append(gen, fmt.Sprintf("peak CPU %.2f cores of %d", v, r.Env.CPUs))
	}
	if v, ok := seriesPeak(findSeries(r, "gen.sched_lag_p99_s")); ok {
		gen = append(gen, "peak p99 schedule lag "+fmtDur(secondsToDuration(v)))
	}
	if len(gen) > 0 {
		fmt.Fprintf(b, "- Generator: %s.\n", strings.Join(gen, ", "))
	}
	b.WriteString("\n")
}

func summaryThroughput(b *strings.Builder, r *Record) {
	steady := r.hasSteadyBasis()
	b.WriteString("## Throughput\n\n")
	if steady {
		fmt.Fprintf(b, "Rates are steady-window rates: each counter's growth from the end of warmup to the end of submissions, divided by that window (%s). ", fmtDur(r.steadyWindow()))
	} else {
		fmt.Fprintf(b, "Rates are run averages: each counter's count from the start to the end of submissions (%s), warmup included, because no counters were snapshotted at the end of warmup. ", fmtDur(r.MeasuredEnd().Sub(r.Started)))
	}
	b.WriteString("Total is the count at the end of the run, drain included; In window is the part the rate is taken over.\n\n")
	if len(r.Counters) == 0 {
		b.WriteString("No counters were recorded.\n\n")
		return
	}
	b.WriteString("| Counter | Total | In window | Per second |\n|---|---:|---:|---:|\n")
	for _, k := range slices.Sorted(maps.Keys(r.Counters)) {
		in, rate := "-", "-"
		if v, ok := r.rate(k, steady); ok && !gaugeCounters[k] {
			in, rate = strconv.FormatInt(r.inWindow(k, steady), 10), strconv.FormatFloat(v, 'f', 2, 64)
		}
		fmt.Fprintf(b, "| %s | %d | %s | %s |\n", k, r.Counters[k], in, rate)
	}
	b.WriteString("\n")
}

func summaryLatency(b *strings.Builder, r *Record) {
	b.WriteString("## Latency\n\n")
	b.WriteString("Histograms cover the whole run, warmup included.\n\n")
	if len(r.Histograms) == 0 {
		b.WriteString("No histograms were recorded.\n\n")
		return
	}
	b.WriteString("| Histogram | n | p50 | p95 | p99 | max |\n|---|---:|---:|---:|---:|---:|\n")
	for _, k := range slices.Sorted(maps.Keys(r.Histograms)) {
		h := r.Histograms[k]
		if h == nil || h.Count() == 0 {
			fmt.Fprintf(b, "| %s | 0 | - | - | - | - |\n", k)
			continue
		}
		fmt.Fprintf(b, "| %s | %d | %s | %s | %s | %s |\n", k, h.Count(),
			fmtDur(h.Quantile(0.5)), fmtDur(h.Quantile(0.95)), fmtDur(h.Quantile(0.99)), fmtDur(h.Max()))
	}
	b.WriteString("\n")
}

func summaryResources(b *strings.Builder, r *Record) {
	b.WriteString("## Server resources\n\n")
	var rows []*measure.Series
	series := r.runSeries()
	for i := range series {
		if matchesAny(series[i].Name, resourceSeries) && len(series[i].Points) > 0 {
			rows = append(rows, &series[i])
		}
	}
	if len(rows) == 0 {
		b.WriteString("No server series were recorded.\n\n")
		return
	}
	slices.SortStableFunc(rows, func(x, y *measure.Series) int { return strings.Compare(x.Name, y.Name) })
	b.WriteString("| Series | Peak | End |\n|---|---:|---:|\n")
	sticky := false
	for _, s := range rows {
		peak, _ := seriesPeak(s)
		end := fmtValue(s.Unit, s.Points[len(s.Points)-1].V)
		if strings.HasPrefix(s.Name, "sched.queue_depth{") {
			// The server keeps a drained queue's last non-zero depth, so the
			// end value of a queue says nothing about whether it drained.
			end, sticky = "-", true
		}
		fmt.Fprintf(b, "| %s | %s | %s |\n", s.Name, fmtValue(s.Unit, peak), end)
	}
	b.WriteString("\n")
	if sticky {
		b.WriteString("A queue's end depth is not shown: the server keeps a drained queue's last non-zero value.\n\n")
	}
}

func summaryGrowth(b *strings.Builder, r *Record) {
	from := r.SteadyFrom()
	b.WriteString("## Growth\n\n")
	fmt.Fprintf(b, "Least-squares slope per hour from %s (the end of warmup).\n\n", from.UTC().Format(time.RFC3339))
	type row struct {
		name, slope string
		r2          float64
	}
	var rows []row
	series := r.runSeries()
	for i := range series {
		s := &series[i]
		if !matchesAny(s.Name, growthSeries) || pointsFrom(s, from) < 2 {
			continue
		}
		slope, r2 := measure.Slope(s.Points, from)
		rows = append(rows, row{s.Name, fmtSigned(s.Unit, slope) + "/h", r2})
	}
	if len(rows) == 0 {
		b.WriteString("No series had two samples after warmup.\n\n")
		return
	}
	slices.SortStableFunc(rows, func(x, y row) int { return strings.Compare(x.name, y.name) })
	b.WriteString("| Series | Slope | R² |\n|---|---:|---:|\n")
	for _, x := range rows {
		fmt.Fprintf(b, "| %s | %s | %.2f |\n", x.name, x.slope, x.r2)
	}
	b.WriteString("\n")
}

func summaryInvariants(b *strings.Builder, r *Record) {
	b.WriteString("## Invariants\n\n")
	if len(r.Invariants) == 0 {
		if !r.Complete {
			b.WriteString("Not run (interrupted): the run ended before its invariants were checked.\n\n")
		} else {
			b.WriteString("No invariants were checked.\n\n")
		}
		return
	}
	for _, inv := range r.Invariants {
		status := "PASS"
		if !inv.OK {
			status = "FAIL"
		}
		fmt.Fprintf(b, "- %s %s\n", status, inv.Name)
		for _, e := range inv.Evidence {
			fmt.Fprintf(b, "  - %s\n", e)
		}
	}
	b.WriteString("\n")
}

func summaryFindings(b *strings.Builder, r *Record) {
	b.WriteString("## Findings\n\n")
	if len(r.Findings) == 0 {
		b.WriteString("None.\n\n")
		return
	}
	for _, f := range r.Findings {
		fmt.Fprintf(b, "- [%s] %s: %s\n", f.Severity, f.Rule, f.Message)
	}
	b.WriteString("\n")
}

func summaryUnavailable(b *strings.Builder, r *Record) {
	b.WriteString("## Unavailable\n\n")
	if len(r.Unavailable) == 0 {
		b.WriteString("None.\n")
		return
	}
	for _, u := range r.Unavailable {
		fmt.Fprintf(b, "- %s\n", u)
	}
}

func secondsToDuration(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// fmtDur renders a duration for people: 1.234s, 850ms, 12.5ms, 1m30.4s.
func fmtDur(d time.Duration) string {
	switch {
	case d >= time.Second || d <= -time.Second:
		return d.Round(time.Millisecond).String()
	case d >= time.Millisecond || d <= -time.Millisecond:
		return d.Round(time.Microsecond).String()
	}
	return d.String()
}

// fmtBytes renders a byte count with a binary unit: 512 B, 1.5 KiB, 200.0 MiB.
func fmtBytes(v float64) string {
	abs := math.Abs(v)
	if abs < 1024 {
		return fmt.Sprintf("%.0f B", v)
	}
	unit := "KiB"
	v /= 1024
	for _, next := range []string{"MiB", "GiB", "TiB"} {
		if math.Abs(v) < 1024 {
			break
		}
		v /= 1024
		unit = next
	}
	return fmt.Sprintf("%.1f %s", v, unit)
}

// fmtValue renders a sample by its unit.
func fmtValue(unit string, v float64) string {
	switch unit {
	case "bytes":
		return fmtBytes(v)
	case "s":
		return fmtDur(secondsToDuration(v))
	case "cores":
		return strconv.FormatFloat(v, 'f', 2, 64)
	}
	if v == math.Trunc(v) {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// fmtSigned is fmtValue with a leading + on a positive value.
func fmtSigned(unit string, v float64) string {
	s := fmtValue(unit, v)
	if v > 0 {
		return "+" + s
	}
	return s
}
