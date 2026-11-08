// SPDX-License-Identifier: AGPL-3.0-or-later

// Package eta computes how much work a job or queue has left, from the task
// counts, measured run times and attempt progress the store reports.
package eta

import (
	"math"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// LinearMinElapsed is how long an attempt must have run before its progress is
// extrapolated linearly; earlier readings are too noisy to project from.
const LinearMinElapsed = 30 * time.Second

// Basis names what a remaining-time figure was derived from.
type Basis string

const (
	// BasisFinished is a terminal job: nothing remains.
	BasisFinished Basis = "finished"
	// BasisProgress extrapolates every running task's own progress.
	BasisProgress Basis = "progress"
	// BasisMeasured uses the mean run time of each step's succeeded tasks.
	BasisMeasured Basis = "measured"
	// BasisEstimate scales the job's submitted estimate by its progress.
	BasisEstimate Basis = "estimate"
)

// Remaining is a job's remaining work in seconds. Seconds is nil, and Basis
// empty, when nothing is known.
type Remaining struct {
	Seconds *int64
	Basis   Basis
}

// Queue totals the remaining work of a queue's unfinished jobs.
type Queue struct {
	WorkRemainingSeconds int64
	JobsWithoutEstimate  int
	RunningTasks         int
	UnfinishedJobs       int
}

// ProgressPercent is the job's completion as a percent: succeeded tasks plus
// the running tasks' progress (a fraction of one task each), over every task
// that has not been canceled. It is nil when no such task exists.
func ProgressPercent(counts map[store.TaskStatus]int, running float64) *float64 {
	total := 0
	for _, n := range counts {
		total += n
	}
	denom := total - counts[store.TaskStatusCanceled]
	if denom <= 0 {
		return nil
	}
	pct := (float64(counts[store.TaskStatusSucceeded]) + running) / float64(denom) * 100
	pct = max(0, min(100, pct))
	return &pct
}

func known(sec float64, b Basis) Remaining {
	s := int64(math.Round(sec))
	return Remaining{Seconds: &s, Basis: b}
}

// Job computes a job's remaining work, trying in order: finished, running
// tasks' linear progress, measured step means, the submitted estimate.
func Job(t store.JobTiming, now time.Time) Remaining {
	if t.Status.IsTerminal() {
		return known(0, BasisFinished)
	}
	if sec, ok := fromProgress(t, now); ok {
		return known(sec, BasisProgress)
	}
	if sec, ok := fromMeasured(t, now); ok {
		return known(sec, BasisMeasured)
	}
	if sec, ok := fromEstimate(t); ok {
		return known(sec, BasisEstimate)
	}
	return Remaining{}
}

func elapsed(r store.RunningTiming, now time.Time) time.Duration {
	return max(0, now.Sub(r.StartedAt))
}

// linearLeft is the time a running task has left if it keeps its pace; ok is
// false until it has progress and has run for [LinearMinElapsed].
func linearLeft(r store.RunningTiming, now time.Time) (float64, bool) {
	e := elapsed(r, now)
	if r.Progress == nil || *r.Progress < 1 || e < LinearMinElapsed {
		return 0, false
	}
	p := float64(min(*r.Progress, 100))
	return e.Seconds() * (100 - p) / p, true
}

// fromProgress applies when a running job has only running tasks left, each
// with a linear projection.
func fromProgress(t store.JobTiming, now time.Time) (float64, bool) {
	if t.Status != store.JobStatusRunning || len(t.Running) == 0 {
		return 0, false
	}
	running := 0
	for _, s := range t.Steps {
		if s.NotStarted > 0 {
			return 0, false
		}
		running += s.Running
	}
	// A running task enters t.Running only through an attempt row; one without
	// is invisible there, so claiming "only running tasks left" would ignore it.
	if running != len(t.Running) {
		return 0, false
	}
	sum := 0.0
	for _, r := range t.Running {
		left, ok := linearLeft(r, now)
		if !ok {
			return 0, false
		}
		sum += left
	}
	return sum, true
}

// stepMean is a step's mean measured run time; a step with Succeeded > 0 but
// Measured == 0 (a bad row) has mean 0, so its not-started tasks add nothing.
func stepMean(s store.StepTiming) float64 {
	if s.Measured == 0 {
		return 0
	}
	return s.MeasuredSeconds / float64(s.Measured)
}

// fromMeasured applies when every step with unfinished tasks has a measured
// succeeded task to take a mean from. A succeeded task with no usable duration
// (no ended_at, an ended_at before its start, or superseded by a later attempt)
// is not enough: a step with only such tasks has no mean, and pricing its work
// at zero would report the job as all but done and override its estimate.
func fromMeasured(t store.JobTiming, now time.Time) (float64, bool) {
	means := make(map[string]float64, len(t.Steps))
	unfinished := false
	sum := 0.0
	for _, s := range t.Steps {
		if s.NotStarted+s.Running == 0 {
			continue
		}
		if s.Measured == 0 {
			return 0, false
		}
		unfinished = true
		means[s.StepID] = stepMean(s)
		sum += means[s.StepID] * float64(s.NotStarted)
	}
	if !unfinished {
		return 0, false
	}
	for _, r := range t.Running {
		if left, ok := linearLeft(r, now); ok {
			sum += left
		} else {
			sum += max(0, means[r.StepID]-elapsed(r, now).Seconds())
		}
	}
	return sum, true
}

func fromEstimate(t store.JobTiming) (float64, bool) {
	if t.EstimatedSeconds == nil {
		return 0, false
	}
	counts := map[store.TaskStatus]int{}
	for _, s := range t.Steps {
		counts[store.TaskStatusPending] += s.NotStarted
		counts[store.TaskStatusRunning] += s.Running
		counts[store.TaskStatusSucceeded] += s.Succeeded
		counts[store.TaskStatusFailed] += s.Failed
		counts[store.TaskStatusCanceled] += s.Canceled
	}
	running := 0.0
	for _, r := range t.Running {
		if r.Progress != nil {
			running += float64(max(0, min(*r.Progress, 100))) / 100
		}
	}
	pct := ProgressPercent(counts, running)
	if pct == nil {
		return 0, true
	}
	return float64(*t.EstimatedSeconds) * (1 - *pct/100), true
}

// Queues totals remaining work per queue over the given jobs.
func Queues(jobs []store.JobTiming, now time.Time) map[string]Queue {
	out := map[string]Queue{}
	for _, j := range jobs {
		if j.Status.IsTerminal() {
			continue
		}
		q := out[j.QueueID]
		q.UnfinishedJobs++
		q.RunningTasks += len(j.Running)
		if r := Job(j, now); r.Seconds != nil {
			q.WorkRemainingSeconds += *r.Seconds
		} else {
			q.JobsWithoutEstimate++
		}
		out[j.QueueID] = q
	}
	return out
}
