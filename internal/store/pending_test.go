// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"slices"
	"testing"

	"github.com/uberware/sqi/internal/store"
)

func TestListJobIDsWithPendingSteps(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			pending := []store.TaskStatus{store.TaskStatusPending}
			live := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning},
				stepSpec{name: "a", status: store.StepStatusPending, tasks: pending})
			paused := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusPaused, share: &live},
				stepSpec{name: "a", status: store.StepStatusPending, tasks: pending})
			seedGraph(t, st, graphOpts{jobStatus: store.JobStatusBlocked, share: &live},
				stepSpec{name: "a", status: store.StepStatusPending, tasks: pending})
			seedGraph(t, st, graphOpts{jobStatus: store.JobStatusCanceled, share: &live},
				stepSpec{name: "a", status: store.StepStatusPending, tasks: pending})
			seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning, share: &live},
				stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusReady}})

			got, err := st.ListJobIDsWithPendingSteps(t.Context())
			if err != nil {
				t.Fatalf("ListJobIDsWithPendingSteps: %v", err)
			}
			want := []string{live.Job.ID, paused.Job.ID}
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("got %v, want %v (blocked, terminal and pending-free jobs excluded)", got, want)
			}
		})
	}
}
