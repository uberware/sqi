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
			ctx := t.Context()
			farm, err := st.CreateFarm(ctx, store.Farm{ID: uuid.NewString(), Name: uuid.NewString()})
			if err != nil {
				t.Fatalf("CreateFarm: %v", err)
			}
			queue := func() string {
				t.Helper()
				q, err := st.CreateQueue(ctx, store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: uuid.NewString()})
				if err != nil {
					t.Fatalf("CreateQueue: %v", err)
				}
				return q.ID
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
					got, err := st.ListReadyTasks(ctx, farm.ID, tc.queues, time.Now().UTC(), 2)
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
	sub := store.JobSubmission{Job: store.Job{
		ID: uuid.NewString(), FarmID: farmID, QueueID: queueID, Name: "job", Priority: priority,
		Status: store.JobStatusRunning, TemplateFormat: store.TemplateFormatJSON,
	}}
	step := store.Step{ID: uuid.NewString(), JobID: sub.Job.ID, Name: "s", Status: store.StepStatusReady}
	sub.Steps = []store.Step{step}
	ids := make([]string, 0, n)
	for range n {
		task := store.Task{ID: uuid.NewString(), JobID: sub.Job.ID, StepID: step.ID, Name: "t", Status: store.TaskStatusReady}
		sub.Tasks = append(sub.Tasks, task)
		ids = append(ids, task.ID)
	}
	storetest.Submit(t, st, sub)
	return ids
}
