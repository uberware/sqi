// SPDX-License-Identifier: AGPL-3.0-or-later

package eta

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

var now = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// run is a running task whose latest attempt started elapsed seconds before now.
func run(step string, elapsed int, progress *int) store.RunningTiming {
	return store.RunningTiming{StepID: step, StartedAt: now.Add(-time.Duration(elapsed) * time.Second), Progress: progress}
}

func step(id string, notStarted, running, succeeded, measured int, measuredSeconds float64) store.StepTiming {
	return store.StepTiming{StepID: id, NotStarted: notStarted, Running: running, Succeeded: succeeded, Measured: measured, MeasuredSeconds: measuredSeconds}
}

func TestProgressPercent(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	tests := []struct {
		name    string
		counts  map[store.TaskStatus]int
		running float64
		want    *float64
	}{
		{"half done", map[store.TaskStatus]int{store.TaskStatusSucceeded: 5, store.TaskStatusFailed: 5}, 0, f(50)},
		{"running share", map[store.TaskStatus]int{store.TaskStatusSucceeded: 1, store.TaskStatusRunning: 1}, 0.5, f(75)},
		{"all canceled", map[store.TaskStatus]int{store.TaskStatusCanceled: 3}, 0, nil},
		{"empty", map[store.TaskStatus]int{}, 0, nil},
		{"canceled excluded", map[store.TaskStatus]int{store.TaskStatusSucceeded: 2, store.TaskStatusCanceled: 2}, 0, f(100)},
		{"clamped", map[store.TaskStatus]int{store.TaskStatusSucceeded: 1, store.TaskStatusRunning: 1}, 5, f(100)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ProgressPercent(tt.counts, tt.running)
			if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
				t.Fatalf("ProgressPercent = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestJob(t *testing.T) {
	tests := []struct {
		name      string
		job       store.JobTiming
		want      int64
		wantNil   bool
		wantBasis Basis
	}{
		{"completed", store.JobTiming{Status: store.JobStatusCompleted, EstimatedSeconds: new(500), Running: []store.RunningTiming{run("a", 10, nil)}}, 0, false, BasisFinished},
		{"failed", store.JobTiming{Status: store.JobStatusFailed, EstimatedSeconds: new(500), Running: []store.RunningTiming{run("a", 10, nil)}}, 0, false, BasisFinished},
		{"canceled", store.JobTiming{Status: store.JobStatusCanceled, EstimatedSeconds: new(500), Running: []store.RunningTiming{run("a", 10, nil)}}, 0, false, BasisFinished},

		{"rule2 linear sum", store.JobTiming{
			Status:  store.JobStatusRunning,
			Steps:   []store.StepTiming{step("a", 0, 2, 0, 0, 0)},
			Running: []store.RunningTiming{run("a", 100, new(50)), run("a", 60, new(25))},
		}, 280, false, BasisProgress},
		{"rule2 short elapsed falls through", store.JobTiming{
			Status:  store.JobStatusRunning,
			Steps:   []store.StepTiming{step("a", 0, 2, 0, 0, 0)},
			Running: []store.RunningTiming{run("a", 100, new(50)), run("a", 20, new(25))},
		}, 0, true, ""},
		{"rule2 ready task falls through", store.JobTiming{
			Status:  store.JobStatusRunning,
			Steps:   []store.StepTiming{step("a", 1, 2, 0, 0, 0)},
			Running: []store.RunningTiming{run("a", 100, new(50)), run("a", 60, new(25))},
		}, 0, true, ""},
		{"rule2 paused falls through", store.JobTiming{
			Status:  store.JobStatusPaused,
			Steps:   []store.StepTiming{step("a", 0, 2, 0, 0, 0)},
			Running: []store.RunningTiming{run("a", 100, new(50)), run("a", 60, new(25))},
		}, 0, true, ""},

		{"rule3 running at p0", store.JobTiming{
			Status:  store.JobStatusRunning,
			Steps:   []store.StepTiming{step("a", 3, 1, 2, 2, 150)},
			Running: []store.RunningTiming{run("a", 30, new(0))},
		}, 270, false, BasisMeasured},
		{"rule3 running linear", store.JobTiming{
			Status:  store.JobStatusRunning,
			Steps:   []store.StepTiming{step("a", 3, 1, 2, 2, 150)},
			Running: []store.RunningTiming{run("a", 40, new(50))},
		}, 265, false, BasisMeasured},
		{"rule3 running overdue no progress", store.JobTiming{
			Status:  store.JobStatusRunning,
			Steps:   []store.StepTiming{step("a", 3, 1, 2, 2, 150)},
			Running: []store.RunningTiming{run("a", 100, nil)},
		}, 225, false, BasisMeasured},
		{"rule3 per-step means", store.JobTiming{
			Status: store.JobStatusPending,
			Steps:  []store.StepTiming{step("s1", 4, 0, 1, 1, 10), step("s2", 2, 0, 1, 1, 100)},
		}, 240, false, BasisMeasured},
		{"rule3 succeeded but not measured", store.JobTiming{
			Status: store.JobStatusPending,
			Steps:  []store.StepTiming{step("a", 3, 0, 1, 0, 0)},
		}, 0, false, BasisMeasured},
		{"rule3 not applicable, rule4", store.JobTiming{
			Status: store.JobStatusRunning, EstimatedSeconds: new(1000),
			Steps: []store.StepTiming{step("s1", 1, 0, 1, 1, 10), step("s2", 2, 0, 0, 0, 0)},
		}, 750, false, BasisEstimate},
		{"rule3 not applicable, rule5", store.JobTiming{
			Status: store.JobStatusRunning,
			Steps:  []store.StepTiming{step("s1", 1, 0, 1, 1, 10), step("s2", 2, 0, 0, 0, 0)},
		}, 0, true, ""},

		{"rule4 no tasks", store.JobTiming{
			Status: store.JobStatusPending, EstimatedSeconds: new(400),
			Steps: []store.StepTiming{{StepID: "a", Canceled: 2}},
		}, 0, false, BasisEstimate},
		{"rule5 unknown", store.JobTiming{
			Status: store.JobStatusPending,
			Steps:  []store.StepTiming{step("a", 4, 0, 0, 0, 0)},
		}, 0, true, ""},
		{"negative elapsed is zero", store.JobTiming{
			Status:  store.JobStatusRunning,
			Steps:   []store.StepTiming{step("a", 1, 1, 1, 1, 100)},
			Running: []store.RunningTiming{run("a", -50, nil)},
		}, 200, false, BasisMeasured},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Job(tt.job, now)
			if got.Basis != tt.wantBasis {
				t.Fatalf("basis = %q, want %q", got.Basis, tt.wantBasis)
			}
			if tt.wantNil {
				if got.Seconds != nil {
					t.Fatalf("seconds = %d, want nil", *got.Seconds)
				}
				return
			}
			if got.Seconds == nil || *got.Seconds != tt.want {
				t.Fatalf("seconds = %v, want %d", got.Seconds, tt.want)
			}
		})
	}
}

func TestQueues(t *testing.T) {
	jobs := []store.JobTiming{
		{QueueID: "A", Status: store.JobStatusPending, EstimatedSeconds: new(600), Steps: []store.StepTiming{step("a", 1, 0, 0, 0, 0)}},
		{QueueID: "A", Status: store.JobStatusPending, Steps: []store.StepTiming{step("a", 1, 0, 0, 0, 0)}},
		{
			QueueID: "A", Status: store.JobStatusRunning,
			Steps:   []store.StepTiming{step("a", 0, 2, 0, 0, 0)},
			Running: []store.RunningTiming{run("a", 150, new(50)), run("a", 150, new(50))},
		},
		{QueueID: "B", Status: store.JobStatusBlocked, EstimatedSeconds: new(50), Steps: []store.StepTiming{step("a", 1, 0, 0, 0, 0)}},
	}
	got := Queues(jobs, now)
	if a := got["A"]; a != (Queue{WorkRemainingSeconds: 900, JobsWithoutEstimate: 1, RunningTasks: 2, UnfinishedJobs: 3}) {
		t.Errorf("A = %+v", a)
	}
	if b := got["B"]; b != (Queue{WorkRemainingSeconds: 50, UnfinishedJobs: 1}) {
		t.Errorf("B = %+v", b)
	}
}
