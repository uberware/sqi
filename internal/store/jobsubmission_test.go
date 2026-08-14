// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"context"
	"testing"

	"github.com/uberware/sqi/internal/store"
)

// submissionFixture builds a two-step, three-task submission on a fresh farm
// and queue. Both are created first because the job row references them.
func submissionFixture(t *testing.T, ctx context.Context, st store.Store) store.JobSubmission {
	t.Helper()
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "f"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "q"}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	return store.JobSubmission{
		Job: store.Job{
			ID: "job-1", FarmID: "farm-1", QueueID: "queue-1",
			Name: "j", Status: store.JobStatusPending,
		},
		Steps: []store.Step{
			{ID: "step-1", JobID: "job-1", Name: "a", StepOrder: 0, Status: store.StepStatusReady},
			{ID: "step-2", JobID: "job-1", Name: "b", StepOrder: 1, Status: store.StepStatusPending},
		},
		Tasks: []store.Task{
			{ID: "task-1", JobID: "job-1", StepID: "step-1", Name: "a-0", Status: store.TaskStatusReady},
			{ID: "task-2", JobID: "job-1", StepID: "step-1", Name: "a-1", Status: store.TaskStatusReady},
			{ID: "task-3", JobID: "job-1", StepID: "step-2", Name: "b-0", Status: store.TaskStatusPending},
		},
	}
}

// TestJobStore_CreateJobSubmission_WritesEverything pins the happy path on both
// backends: one call produces the job, its steps and its tasks, and returns
// them populated the way the per-row creators do.
func TestJobStore_CreateJobSubmission_WritesEverything(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			sub := submissionFixture(t, ctx, st)

			out, err := st.CreateJobSubmission(ctx, sub)
			if err != nil {
				t.Fatalf("CreateJobSubmission: %v", err)
			}

			if out.Job.ID != "job-1" {
				t.Errorf("returned job ID = %q, want job-1", out.Job.ID)
			}
			if len(out.Steps) != 2 {
				t.Errorf("returned %d steps, want 2", len(out.Steps))
			}
			if len(out.Tasks) != 3 {
				t.Errorf("returned %d tasks, want 3", len(out.Tasks))
			}
			// The rows come back the way the per-row creators return theirs:
			// timestamps populated by the store, not the caller's zero values.
			if out.Job.CreatedAt.IsZero() {
				t.Error("returned job has a zero CreatedAt; it was not populated by the store")
			}
			for _, s := range out.Steps {
				if s.CreatedAt.IsZero() {
					t.Errorf("returned step %s has a zero CreatedAt", s.ID)
				}
			}
			for _, tk := range out.Tasks {
				if tk.CreatedAt.IsZero() {
					t.Errorf("returned task %s has a zero CreatedAt", tk.ID)
				}
			}

			if _, err := st.GetJob(ctx, "job-1"); err != nil {
				t.Errorf("GetJob: %v", err)
			}
			steps, err := st.ListSteps(ctx, "job-1")
			if err != nil || len(steps) != 2 {
				t.Errorf("ListSteps = %d steps, %v; want 2, nil", len(steps), err)
			}
			tasks, err := st.ListTasks(ctx, store.ListTasksOptions{JobID: "job-1"})
			if err != nil {
				t.Fatalf("ListTasks: %v", err)
			}
			if len(tasks.Items) != 3 {
				t.Errorf("ListTasks = %d tasks, want 3", len(tasks.Items))
			}
		})
	}
}

// TestJobStore_CreateJobSubmission_RollsBackEntirely is the whole point of the
// method, and of this change.
//
// A submission that fails partway must leave NOTHING: not the job row, not the
// steps that already inserted, not their tasks. Before this method existed,
// Submit wrote those rows one call at a time and a mid-way failure stranded a
// pending job that no sweep reaps and that checkJobCompletion would later mark
// completed despite missing steps.
//
// The induced failure is a duplicate step name, which violates the (JobID,
// Name) uniqueness both backends enforce (see store/step.go's CreateStep doc).
// It fires on the SECOND step, so the job row and the first step have already
// been written inside the transaction when it hits.
func TestJobStore_CreateJobSubmission_RollsBackEntirely(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			sub := submissionFixture(t, ctx, st)
			sub.Steps[1].Name = sub.Steps[0].Name // duplicate -> conflict

			if _, err := st.CreateJobSubmission(ctx, sub); err == nil {
				t.Fatal("CreateJobSubmission accepted a duplicate step name")
			}

			if _, err := st.GetJob(ctx, "job-1"); err == nil {
				t.Error("the job row survived a failed submission; the write was not rolled back")
			}
			steps, err := st.ListSteps(ctx, "job-1")
			if err == nil && len(steps) != 0 {
				t.Errorf("%d step rows survived a failed submission, want 0", len(steps))
			}
			tasks, err := st.ListTasks(ctx, store.ListTasksOptions{JobID: "job-1"})
			if err == nil && len(tasks.Items) != 0 {
				t.Errorf("%d task rows survived a failed submission, want 0", len(tasks.Items))
			}
		})
	}
}

// TestJobStore_CreateJobSubmission_WritesDependencyEdges pins that the
// dependency edges are part of the same atomic write, which is what lets the
// job be created directly in blocked status (see Task 3): a sweep can never
// observe a blocked job with zero edges if both commit together.
func TestJobStore_CreateJobSubmission_WritesDependencyEdges(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			sub := submissionFixture(t, ctx, st)

			// An upstream job for the edge to point at.
			upstream := sub.Job
			upstream.ID = "job-upstream"
			upstream.Name = "up"
			if _, err := st.CreateJob(ctx, upstream); err != nil {
				t.Fatalf("CreateJob(upstream): %v", err)
			}

			sub.Job.Status = store.JobStatusBlocked
			sub.DependsOn = []string{"job-upstream"}

			if _, err := st.CreateJobSubmission(ctx, sub); err != nil {
				t.Fatalf("CreateJobSubmission: %v", err)
			}

			ids, err := st.ListJobDependencyIDs(ctx, "job-1")
			if err != nil {
				t.Fatalf("ListJobDependencyIDs: %v", err)
			}
			if len(ids) != 1 || ids[0] != "job-upstream" {
				t.Errorf("dependency IDs = %v, want [job-upstream]", ids)
			}

			job, err := st.GetJob(ctx, "job-1")
			if err != nil {
				t.Fatalf("GetJob: %v", err)
			}
			if job.Status != store.JobStatusBlocked {
				t.Errorf("status = %q, want blocked", job.Status)
			}
		})
	}
}

// TestJobStore_CreateJobSubmission_DoesNotAliasCallerMemory pins the defensive
// copying the per-row creators already do: mutating the slices and maps handed
// to CreateJobSubmission after it returns must not change what is stored.
func TestJobStore_CreateJobSubmission_DoesNotAliasCallerMemory(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			sub := submissionFixture(t, ctx, st)
			sub.Steps[1].DependsOn = []string{"a"}
			sub.Tasks[0].Parameters = map[string]string{"frame": "1"}

			if _, err := st.CreateJobSubmission(ctx, sub); err != nil {
				t.Fatalf("CreateJobSubmission: %v", err)
			}

			sub.Steps[1].DependsOn[0] = "mutated"
			sub.Tasks[0].Parameters["frame"] = "mutated"

			steps, err := st.ListSteps(ctx, "job-1")
			if err != nil {
				t.Fatalf("ListSteps: %v", err)
			}
			for _, s := range steps {
				if s.ID == "step-2" && (len(s.DependsOn) != 1 || s.DependsOn[0] != "a") {
					t.Errorf("stored step depends_on = %v, want [a]; the store aliased caller memory", s.DependsOn)
				}
			}
			task, err := st.GetTask(ctx, "task-1")
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if task.Parameters["frame"] != "1" {
				t.Errorf("stored task parameter = %q, want 1; the store aliased caller memory", task.Parameters["frame"])
			}
		})
	}
}
