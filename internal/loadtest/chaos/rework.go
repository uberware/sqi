// SPDX-License-Identifier: AGPL-3.0-or-later

package chaos

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/uberware/sqi/internal/loadtest/marker"
	"github.com/uberware/sqi/internal/loadtest/report"
	"github.com/uberware/sqi/internal/loadtest/simworker"
	"github.com/uberware/sqi/internal/store"
)

// maxReworkExamples is how many wasted attempts the report names.
const maxReworkExamples = 20

// wasted is one attempt that ran to completion on a worker and did not count.
type wasted struct {
	// end is when the worker finished it.
	end     time.Time
	seconds float64
	example string
}

// rework measures the work the farm did twice, from the stopped server's
// store.
//
// A simulated worker's attempt is wasted when it recorded success and the
// server's attempt is not succeeded (the server reclaimed it first). A real
// task's execution is wasted when the task's main process completed (it
// exited 0) more times than the server has succeeded attempts: of the task's
// completed main execs, each succeeded attempt credits the one that ended
// nearest its EndedAt, and the rest are wasted. A wasted attempt costs the
// time it ran, and is attributed to the latest fault that began before the
// worker finished it, none when no fault had.
func (f *farmView) rework(ctx context.Context, leased []leasedAttempt, execs []marker.Exec, faults []report.FaultRecord) (report.Rework, error) {
	lost, err := simulatedRework(leased)
	if err != nil {
		return report.Rework{}, err
	}
	realLost, err := f.realRework(ctx, execs)
	if err != nil {
		return report.Rework{}, err
	}
	lost = append(lost, realLost...)
	slices.SortStableFunc(lost, func(a, b wasted) int { return a.end.Compare(b.end) })
	var out report.Rework
	for _, w := range lost {
		out.Attempts++
		out.Seconds += w.seconds
		if len(out.Examples) < maxReworkExamples {
			out.Examples = append(out.Examples, w.example)
		}
		if f, ok := precedingFault(faults, w.end); ok {
			if out.ByFault == nil {
				out.ByFault = map[int]int{}
			}
			out.ByFault[f]++
		}
	}
	return out, nil
}

// precedingFault returns the index of the fault with the latest Start before
// at.
func precedingFault(faults []report.FaultRecord, at time.Time) (int, bool) {
	best := -1
	for i, f := range faults {
		if f.Start.Before(at) && (best < 0 || !f.Start.Before(faults[best].Start)) {
			best = i
		}
	}
	if best < 0 {
		return 0, false
	}
	return faults[best].Index, true
}

// simulatedRework finds the ledger's succeeded attempts the server did not
// record as succeeded. An attempt the store does not have is left to
// executions-recorded.
func simulatedRework(leased []leasedAttempt) ([]wasted, error) {
	var out []wasted
	for _, la := range leased {
		if la.Outcome() != simworker.OutcomeSucceeded {
			continue
		}
		switch {
		case errors.Is(la.err, store.ErrNotFound):
			continue
		case la.err != nil:
			return nil, fmt.Errorf("read attempt %s of task %s: %w", la.ID, la.TaskID, la.err)
		}
		if la.status == store.AttemptStatusSucceeded {
			continue
		}
		d := max(la.End.At.Sub(la.Leased), 0)
		out = append(out, wasted{
			end:     la.End.At,
			seconds: d.Seconds(),
			example: fmt.Sprintf("attempt %s of task %s: worker %s reported succeeded after %s, the server recorded %s",
				la.ID, la.TaskID, la.WorkerID, d.Round(time.Millisecond), la.status),
		})
	}
	return out, nil
}

// realRework finds the real tasks' completed main execs the server did not
// credit.
func (f *farmView) realRework(ctx context.Context, execs []marker.Exec) ([]wasted, error) {
	completed := map[string][]marker.Exec{}
	for _, e := range execs {
		if e.Role == marker.RoleMain && e.End != nil && e.ExitCode == 0 {
			completed[e.Key] = append(completed[e.Key], e)
		}
	}
	if len(completed) == 0 {
		return nil, nil
	}
	byKey := f.tasksByKey()
	var out []wasted
	for _, key := range slices.Sorted(maps.Keys(completed)) {
		tasks, ok := byKey[key]
		if !ok {
			continue // executions-recorded names a key that is no task
		}
		var credits []time.Time
		for _, id := range tasks {
			attempts, err := f.taskAttempts(ctx, id)
			if err != nil {
				return nil, fmt.Errorf("list the attempts of task %s: %w", id, err)
			}
			for _, a := range attempts {
				if a.Status == store.AttemptStatusSucceeded && a.EndedAt != nil {
					credits = append(credits, *a.EndedAt)
				}
			}
		}
		for _, e := range uncredited(completed[key], credits) {
			d := e.End.Sub(e.Start)
			out = append(out, wasted{
				end:     *e.End,
				seconds: d.Seconds(),
				example: fmt.Sprintf("task %s: exec %s (incarnation %s, pid %d) completed after %s, but the server credited %d succeeded attempts to the task's %d completed execs",
					key, e.ID, e.Incarnation, e.PID, d.Round(time.Millisecond), len(credits), len(completed[key])),
			})
		}
	}
	return out, nil
}

// uncredited removes, for each succeeded attempt, the completed exec that
// ended nearest its end, and returns the execs left.
func uncredited(execs []marker.Exec, credits []time.Time) []marker.Exec {
	left := slices.Clone(execs)
	slices.SortFunc(left, func(a, b marker.Exec) int { return a.End.Compare(*b.End) })
	for _, at := range credits {
		if len(left) == 0 {
			break
		}
		nearest := 0
		for i, e := range left {
			if absDuration(e.End.Sub(at)) < absDuration(left[nearest].End.Sub(at)) {
				nearest = i
			}
		}
		left = slices.Delete(left, nearest, nearest+1)
	}
	return left
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
