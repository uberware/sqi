// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"time"
)

// StepTiming is one step's task counts and measured run time, the inputs a
// remaining-time estimate reads for that step.
type StepTiming struct {
	StepID     string
	NotStarted int // pending + ready + assigned
	Running    int
	Succeeded  int
	Failed     int
	Canceled   int
	// Measured is how many succeeded tasks have a succeeded attempt with an
	// ended_at; MeasuredSeconds sums those attempts' ended_at − started_at.
	// Only a task's latest attempt (highest attempt number, then greatest ID)
	// is measured, so a task counts at most once. A task left succeeded by an
	// attempt that a later one superseded counts in Succeeded but not in
	// Measured: it under-measures, which only leaves the rate to the tasks
	// that were measured.
	Measured        int
	MeasuredSeconds float64
}

// RunningTiming is one running task: its step, when its latest attempt
// started, and that attempt's progress.
type RunningTiming struct {
	StepID    string
	StartedAt time.Time // the task's latest attempt
	Progress  *int      // that attempt's progress, nil when none reported
}

// JobTiming is the inputs a remaining-time estimate reads for one job.
type JobTiming struct {
	JobID, QueueID   string
	Status           JobStatus
	EstimatedSeconds *int
	Steps            []StepTiming    // ordered by step_id; steps with no tasks omitted
	Running          []RunningTiming // ordered by StartedAt
}

// TimingStore reads the inputs of remaining-time estimates. Each call costs a
// fixed number of queries whatever the number of jobs.
type TimingStore interface {
	// JobTimings returns the inputs for each given job that exists; unknown
	// IDs are absent. Empty input returns an empty map without querying.
	JobTimings(ctx context.Context, jobIDs []string) (map[string]JobTiming, error)
	// QueueJobTimings returns the inputs for every non-terminal job in the
	// given queues. Empty input returns nil without querying.
	QueueJobTimings(ctx context.Context, queueIDs []string) ([]JobTiming, error)
}
