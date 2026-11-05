// SPDX-License-Identifier: AGPL-3.0-or-later

package report

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// kneeFactor is how many times the baseline p95 a point's first-lease p95 must
// exceed to be the knee.
const kneeFactor = 2

// SweepPoint is one run of a sweep: the value of the varied parameter and the
// record of the run made with it.
type SweepPoint struct {
	Value string
	Rec   *Record
}

// firstLeaseP95 returns a record's p95 submit-to-first-lease latency (the
// histogram under HistFirstLease) and whether it has one.
func firstLeaseP95(r *Record) (time.Duration, bool) {
	if r == nil {
		return 0, false
	}
	h := r.Histograms[HistFirstLease]
	if h == nil || h.Count() == 0 {
		return 0, false
	}
	return h.Quantile(0.95), true
}

// failedInvariants names the invariants that did not hold.
func failedInvariants(r *Record) []string {
	var names []string
	for _, inv := range r.Invariants {
		if !inv.OK {
			names = append(names, inv.Name)
		}
	}
	return names
}

// Knee finds where a sweep's farm stopped coping. points are in ascending
// order of the varied value and the first is the baseline. The knee is the
// first point whose run was INVALID (the generator saturated), whose
// invariants failed, or whose submit-to-first-lease p95 exceeds twice the
// baseline's. It returns the point's index and the reason, or -1 and a note
// when there is no knee. A point with no record is the knee, with the reason
// "missing record". A point with no first-lease histogram is not judged on
// latency, and neither is any point when the baseline has none or a zero p95.
func Knee(points []SweepPoint) (index int, reason string) {
	if len(points) == 0 {
		return -1, "no knee: the sweep has no points"
	}
	// A baseline with no p95, or a zero one, disables the latency rule: there
	// is nothing to be twice.
	base, haveBase := firstLeaseP95(points[0].Rec)
	for i, p := range points {
		if p.Rec == nil {
			return i, "missing record"
		}
		if !p.Rec.Valid {
			return i, "run INVALID: " + orDash(p.Rec.InvalidReason)
		}
		if failed := failedInvariants(p.Rec); len(failed) > 0 {
			return i, "invariant failed: " + strings.Join(failed, ", ")
		}
		if !haveBase || base <= 0 {
			continue
		}
		if got, ok := firstLeaseP95(p.Rec); ok && got > kneeFactor*base {
			return i, fmt.Sprintf("p95 %s× baseline", fmtRatio(float64(got)/float64(base)))
		}
	}
	return -1, "no knee in the tested range"
}

// fmtRatio renders a ratio with up to two decimals: 2.5, 2.04, 3.
func fmtRatio(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// throughput is a record's tasks reaching a terminal status per second: over
// its steady window when it carries a SteadyCounters snapshot, else its run
// average. ok is false when the run has no duration.
func throughput(r *Record) (float64, bool) {
	return r.rate(CounterTasksTerminal, r.hasSteadyBasis())
}

// WriteSweep writes sweep.csv and sweep.md into dir: one row per point with
// the throughput and the first-lease p50, p95 and p99, whether the run was
// valid and whether its invariants held, and the knee called out in the
// table. varied names the parameter that was swept.
func WriteSweep(dir, varied string, pts []SweepPoint) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("report: %w", err)
	}
	knee, reason := Knee(pts)
	if err := writeFile(filepath.Join(dir, "sweep.csv"), func(f *os.File) error { return writeSweepCSV(f, varied, pts) }); err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, "sweep.md"), func(f *os.File) error {
		_, err := f.WriteString(sweepMarkdown(varied, pts, knee, reason))
		return err
	})
}

func writeSweepCSV(f *os.File, varied string, pts []SweepPoint) error {
	cw := csv.NewWriter(f)
	if err := cw.Write([]string{varied, "throughput_per_s", "first_lease_p50_s", "first_lease_p95_s", "first_lease_p99_s", "valid", "invariants_ok"}); err != nil {
		return err
	}
	for _, p := range pts {
		row := []string{p.Value, "", "", "", "", "false", "false"}
		if r := p.Rec; r != nil {
			if tp, ok := throughput(r); ok {
				row[1] = strconv.FormatFloat(tp, 'f', 2, 64)
			}
			if h := r.Histograms[HistFirstLease]; h != nil && h.Count() > 0 {
				row[2] = strconv.FormatFloat(h.Quantile(0.5).Seconds(), 'f', 6, 64)
				row[3] = strconv.FormatFloat(h.Quantile(0.95).Seconds(), 'f', 6, 64)
				row[4] = strconv.FormatFloat(h.Quantile(0.99).Seconds(), 'f', 6, 64)
			}
			row[5] = strconv.FormatBool(r.Valid)
			row[6] = strconv.FormatBool(len(failedInvariants(r)) == 0)
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func sweepMarkdown(varied string, pts []SweepPoint, knee int, reason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Sweep of %s\n\n", varied)
	switch {
	case knee >= 0:
		fmt.Fprintf(&b, "Knee at %s = %s: %s.\n", varied, pts[knee].Value, reason)
		if knee > 0 {
			fmt.Fprintf(&b, "The largest value before it was %s.\n", pts[knee-1].Value)
		} else {
			b.WriteString("The smallest value tried already failed.\n")
		}
	default:
		fmt.Fprintf(&b, "No knee: %s.\n", strings.TrimPrefix(reason, "no knee: "))
	}
	fmt.Fprintf(&b, "\nThe knee is the first value whose run was INVALID, broke an invariant, or whose submit-to-first-lease p95 exceeded %dx the first value's.\n\n", kneeFactor)
	fmt.Fprintf(&b, "| %s | Tasks/s | First lease p50 | p95 | p99 | Valid | Invariants | |\n|---|---:|---:|---:|---:|---|---|---|\n", varied)
	for i, p := range pts {
		tps, p50, p95, p99, valid, inv := "-", "-", "-", "-", "-", "-"
		if r := p.Rec; r != nil {
			if tp, ok := throughput(r); ok {
				tps = strconv.FormatFloat(tp, 'f', 2, 64)
			}
			if h := r.Histograms[HistFirstLease]; h != nil && h.Count() > 0 {
				p50, p95, p99 = fmtDur(h.Quantile(0.5)), fmtDur(h.Quantile(0.95)), fmtDur(h.Quantile(0.99))
			}
			valid, inv = yesNo(r.Valid), yesNo(len(failedInvariants(r)) == 0)
		}
		mark := ""
		if i == knee {
			mark = "**<- knee**"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s |\n", p.Value, tps, p50, p95, p99, valid, inv, mark)
	}
	return b.String()
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}
