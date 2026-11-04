// SPDX-License-Identifier: AGPL-3.0-or-later

package runner

import (
	"testing"

	"github.com/uberware/sqi/internal/loadtest/client"
)

// TestTaskCounts reads the server's task_counts as GET /jobs serves them:
// with total and unschedulable beside the per-status counts, so summing every
// key would count each task twice.
func TestTaskCounts(t *testing.T) {
	jobs := map[string]client.Job{
		"j1": {TaskCounts: map[string]int{
			"total": 10, "pending": 1, "ready": 3, "assigned": 0, "running": 1,
			"succeeded": 3, "failed": 1, "canceled": 1, "unschedulable": 2,
		}},
		"j2": {TaskCounts: map[string]int{"total": 4, "succeeded": 4}},
	}
	if got := allTasks(jobs); got != 14 {
		t.Errorf("allTasks = %d, want 14", got)
	}
	if got := terminalTasks(jobs); got != 9 {
		t.Errorf("terminalTasks = %d, want 9", got)
	}
}
