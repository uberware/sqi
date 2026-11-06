// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import "testing"

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
			e.activeTasks["task-1"] = &taskRun{taskID: "task-1", attemptID: "attempt-new", cancelFunc: func() { calls++ }}

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
			e.activeTasks["task-1"] = run

			if got := e.Cancel("task-1", tt.attemptID); got != tt.want {
				t.Fatalf("Cancel = %v, want %v", got, tt.want)
			}
			if run.cancelRequested != tt.want {
				t.Fatalf("cancelRequested = %v, want %v", run.cancelRequested, tt.want)
			}
		})
	}
}
