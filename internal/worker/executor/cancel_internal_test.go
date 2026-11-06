// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import (
	"slices"
	"testing"

	"github.com/uberware/sqi/internal/worker/metrics"
)

// TestCancel_NamedAttemptMustMatch pins that a cancel naming an attempt stops
// the task's run only when that is the attempt it runs, and a cancel naming
// none stops whatever run the task has ([protocol.TaskCancelMsg.AttemptID]).
// Both the started path (cancelFunc set) and the early path (cancelRequested)
// are checked.
func TestCancel_NamedAttemptMustMatch(t *testing.T) {
	tests := []struct {
		name      string
		attemptID string
		want      bool
	}{
		{name: "another attempt", attemptID: "attempt-old", want: false},
		{name: "this attempt", attemptID: "attempt-new", want: true},
		{name: "no attempt named", attemptID: "", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name+"/started", func(t *testing.T) {
			e := &Executor{activeTasks: map[string]*taskRun{}}
			calls := 0
			e.activeTasks["attempt-new"] = &taskRun{taskID: "task-1", attemptID: "attempt-new", cancelFunc: func() { calls++ }}

			if got := e.Cancel("task-1", tt.attemptID); got != tt.want {
				t.Fatalf("Cancel = %v, want %v", got, tt.want)
			}
			if (calls == 1) != tt.want {
				t.Fatalf("cancelFunc called %d times, want stopped = %v", calls, tt.want)
			}
		})
		t.Run(tt.name+"/early", func(t *testing.T) {
			e := &Executor{activeTasks: map[string]*taskRun{}}
			run := &taskRun{taskID: "task-1", attemptID: "attempt-new"}
			e.activeTasks["attempt-new"] = run

			if got := e.Cancel("task-1", tt.attemptID); got != tt.want {
				t.Fatalf("Cancel = %v, want %v", got, tt.want)
			}
			if run.cancelRequested != tt.want {
				t.Fatalf("cancelRequested = %v, want %v", run.cancelRequested, tt.want)
			}
		})
	}
}

// TestActiveTasks_TwoAttemptsOfOneTask pins that a worker re-leased a task
// while its stale attempt still runs keeps both runs: the newer one does not
// hide the stale one from a cancel naming it, the stale run's exit does not
// remove the newer one, and a cancel naming no attempt stops both.
func TestActiveTasks_TwoAttemptsOfOneTask(t *testing.T) {
	newRuns := func() (*Executor, map[string]int) {
		e := &Executor{activeTasks: map[string]*taskRun{}, m: metrics.New()}
		calls := map[string]int{}
		for _, a := range []string{"attempt-old", "attempt-new"} {
			e.addActiveTask(&taskRun{taskID: "task-1", attemptID: a, cancelFunc: func() { calls[a]++ }})
		}
		return e, calls
	}
	runOf := func(e *Executor, attemptID string) *taskRun {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.activeTasks[attemptID]
	}

	t.Run("named cancel reaches the stale run", func(t *testing.T) {
		e, calls := newRuns()
		if !e.Cancel("task-1", "attempt-old") {
			t.Fatal("Cancel(attempt-old) = false, want the stale run found")
		}
		if calls["attempt-old"] != 1 || calls["attempt-new"] != 0 {
			t.Fatalf("cancels = %v, want only attempt-old stopped", calls)
		}
	})
	t.Run("stale exit keeps the newer run", func(t *testing.T) {
		e, calls := newRuns()
		e.removeActiveTask(runOf(e, "attempt-old"))
		if got := e.ActiveTaskCount(); got != 1 {
			t.Fatalf("ActiveTaskCount = %d, want 1", got)
		}
		if !e.Cancel("task-1", "attempt-new") || calls["attempt-new"] != 1 {
			t.Fatalf("the newer run was lost with the stale one's exit (cancels %v)", calls)
		}
	})
	t.Run("task-wide cancel stops both", func(t *testing.T) {
		e, calls := newRuns()
		if !e.Cancel("task-1", "") {
			t.Fatal("Cancel(task-1) = false, want runs found")
		}
		if calls["attempt-old"] != 1 || calls["attempt-new"] != 1 {
			t.Fatalf("cancels = %v, want both stopped", calls)
		}
	})
	t.Run("task reported once", func(t *testing.T) {
		e, _ := newRuns()
		if got := e.ActiveTaskCount(); got != 2 {
			t.Fatalf("ActiveTaskCount = %d, want 2 processes", got)
		}
		if got := e.ActiveTaskIDs(); !slices.Equal(got, []string{"task-1"}) {
			t.Fatalf("ActiveTaskIDs = %v, want [task-1]", got)
		}
	})
}
