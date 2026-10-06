// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// TestRetryTasks_RevivesReadyUnderAReadyStep pins that retrying a failed
// task while a sibling still runs (so its step is still ready) revives it
// ready, because nothing will release a pending task in a ready step.
// Under a terminal step the task is revived pending as before, and the step
// is reset for ResolveDependencies to release.
func TestRetryTasks_RevivesReadyUnderAReadyStep(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(
				t, st, graphOpts{jobStatus: store.JobStatusRunning},
				stepSpec{name: "live", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusFailed, store.TaskStatusRunning}},
				stepSpec{name: "over", status: store.StepStatusFailed, tasks: []store.TaskStatus{store.TaskStatusFailed}},
			)
			ids := []string{g.Tasks["live"][0].ID, g.Tasks["over"][0].ID}
			revived, err := st.RetryTasks(t.Context(), g.Job.ID, ids, time.Now().UTC())
			if err != nil || len(revived) != 2 {
				t.Fatalf("RetryTasks = (%d, %v), want (2, nil)", len(revived), err)
			}
			want := map[string]store.TaskStatus{ids[0]: store.TaskStatusReady, ids[1]: store.TaskStatusPending}
			for _, r := range revived {
				if r.Status != want[r.ID] {
					t.Errorf("returned task %s = %q, want %q", r.ID, r.Status, want[r.ID])
				}
				if got := mustTask(t, st, r.ID).Status; got != want[r.ID] {
					t.Errorf("stored task %s = %q, want %q", r.ID, got, want[r.ID])
				}
			}
			if got := mustStep(t, st, g.Steps["live"].ID).Status; got != store.StepStatusReady {
				t.Errorf("live step = %q, want ready (untouched)", got)
			}
			if got := mustStep(t, st, g.Steps["over"].ID).Status; got != store.StepStatusPending {
				t.Errorf("over step = %q, want pending (reset for release)", got)
			}
		})
	}
}
