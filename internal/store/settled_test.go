// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"slices"
	"testing"

	"github.com/uberware/sqi/internal/store"
)

func TestListJobIDsWithAllStepsTerminal(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			succeeded := []store.TaskStatus{store.TaskStatusSucceeded}
			settled := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning},
				stepSpec{name: "a", status: store.StepStatusCompleted, tasks: succeeded},
				stepSpec{name: "b", status: store.StepStatusFailed, tasks: []store.TaskStatus{store.TaskStatusFailed}})
			paused := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusPaused, share: &settled},
				stepSpec{name: "a", status: store.StepStatusCanceled, tasks: []store.TaskStatus{store.TaskStatusCanceled}})
			// open: one step still running.
			seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning, share: &settled},
				stepSpec{name: "a", status: store.StepStatusCompleted, tasks: succeeded},
				stepSpec{name: "b", status: store.StepStatusRunning, tasks: []store.TaskStatus{store.TaskStatusRunning}})
			// done: the job itself is terminal.
			seedGraph(t, st, graphOpts{jobStatus: store.JobStatusCompleted, share: &settled},
				stepSpec{name: "a", status: store.StepStatusCompleted, tasks: succeeded})
			// pendingOnly: a step that has not started.
			seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning, share: &settled},
				stepSpec{name: "a", status: store.StepStatusPending, tasks: []store.TaskStatus{store.TaskStatusPending}})

			got, err := st.ListJobIDsWithAllStepsTerminal(t.Context())
			if err != nil {
				t.Fatalf("ListJobIDsWithAllStepsTerminal: %v", err)
			}
			want := []string{settled.Job.ID, paused.Job.ID}
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("got %v, want %v (jobs with an open step and terminal jobs excluded)", got, want)
			}
		})
	}
}
