// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
)

// TestListReadyTasks_QueueFilter pins that a queue list narrows the ready list
// before the limit applies: a higher-priority job filling the limit from a
// queue outside the list does not hide the listed queues' tasks.
func TestListReadyTasks_QueueFilter(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			farm := mustCreateFarm(t, st, "ready-farm")
			queue := func() string {
				return mustCreateQueue(t, st, store.Queue{FarmID: farm.ID, Name: uuid.NewString()}).ID
			}
			high, low, other := queue(), queue(), queue()
			highTasks := submitReadyJob(t, st, farm.ID, high, 10, 3)
			lowTask := submitReadyJob(t, st, farm.ID, low, 0, 1)[0]
			otherTask := submitReadyJob(t, st, farm.ID, other, 5, 1)[0]

			cases := []struct {
				name   string
				queues []string
				want   []string
			}{
				{"no list is every queue", nil, highTasks[:2]},
				{"empty list is every queue", []string{}, highTasks[:2]},
				{"one queue below the limit's cut", []string{low}, []string{lowTask}},
				{"two queues keep priority order", []string{low, other}, []string{otherTask, lowTask}},
				{"a listed queue appearing twice", []string{low, low}, []string{lowTask}},
				{"an unknown queue matches nothing", []string{uuid.NewString()}, nil},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					got, err := st.ListReadyTasks(t.Context(), farm.ID, tc.queues, time.Now().UTC(), 2)
					if err != nil {
						t.Fatalf("ListReadyTasks: %v", err)
					}
					ids := project(got, func(x store.Task) string { return x.ID })
					if !slices.Equal(ids, tc.want) {
						t.Fatalf("ListReadyTasks(%v) = %v, want %v", tc.queues, ids, tc.want)
					}
				})
			}
		})
	}
}

// submitReadyJob submits a running job of the given priority in farmID's
// queueID with one ready step of n ready tasks, and returns the task IDs in
// creation order.
func submitReadyJob(t *testing.T, st store.Store, farmID, queueID string, priority, n int) []string {
	t.Helper()
	stepID := uuid.NewString()
	seed := storetest.NewJob(uuid.NewString(), farmID, queueID).As(store.JobStatusRunning).Step(stepID, store.StepStatusReady)
	seed.Sub.Job.Priority = priority
	ids := make([]string, n)
	for i := range ids {
		ids[i] = uuid.NewString()
		seed.Task(ids[i], stepID, store.TaskStatusReady)
	}
	seed.Submit(t, st)
	return ids
}
