// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// submissionFixture builds a two-step, three-task submission on a fresh farm
// and queue. Both are created first because the job row references them.
func submissionFixture(ctx context.Context, t *testing.T, st store.Store) store.JobSubmission {
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

// assertFreshTimestamps checks that a row the store just created carries a
// CreatedAt that is actually "now" and an UpdatedAt equal to it, rather than
// merely a non-zero value.
func assertFreshTimestamps(t *testing.T, what string, createdAt, updatedAt time.Time) {
	t.Helper()
	if createdAt.IsZero() {
		t.Errorf("%s has a zero CreatedAt; it was not populated by the store", what)
		return
	}
	if !updatedAt.Equal(createdAt) {
		t.Errorf("%s has UpdatedAt %v, want it equal to CreatedAt %v", what, updatedAt, createdAt)
	}
	if skew := time.Since(createdAt); skew < -time.Second || skew > time.Second {
		t.Errorf("%s has CreatedAt %v, which is %v away from now", what, createdAt, skew)
	}
}

// TestJobStore_CreateJobSubmission_WritesEverything pins the happy path on both
// backends: one call produces the job, its steps and its tasks, and returns
// them populated the way the per-row creators do.
func TestJobStore_CreateJobSubmission_WritesEverything(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			sub := submissionFixture(ctx, t, st)

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
			// A non-zero check alone would pass on a value a century off, or
			// on a CreatedAt stamped without its UpdatedAt, so both backends
			// are held to "now, and the same on both fields".
			assertFreshTimestamps(t, "job "+out.Job.ID, out.Job.CreatedAt, out.Job.UpdatedAt)
			for _, s := range out.Steps {
				assertFreshTimestamps(t, "step "+s.ID, s.CreatedAt, s.UpdatedAt)
			}
			for _, tk := range out.Tasks {
				assertFreshTimestamps(t, "task "+tk.ID, tk.CreatedAt, tk.UpdatedAt)
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
//
// The sqlite subtest carries the whole test. Sabotaged by replacing SQLite's
// deferred Rollback with a Commit, it fails with the job row and exactly ONE
// step row surviving — which is what proves the conflict fires after real
// writes rather than before any. The fake subtest is VACUOUS BY CONSTRUCTION
// and stays green under that same sabotage: validateSubmission runs to
// completion before the first map assignment, so on the failing path the fake
// never wrote anything to roll back. That is the fake's intended design, not
// an oversight, but it means this test's non-vacuity rests entirely on sqlite.
func TestJobStore_CreateJobSubmission_RollsBackEntirely(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			sub := submissionFixture(ctx, t, st)
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
			sub := submissionFixture(ctx, t, st)

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
//
// This is effectively a FAKE-ONLY test wearing a cross-backend harness, and a
// later reader should not over-trust the fact that it passes on both. SQLite
// marshals every one of these fields to JSON on the way in and re-scans it on
// the way out, so aliasing caller memory is impossible there no matter what
// the code does; only the fake, which stores Go values directly, can fail it.
// It is run on both anyway so the contract is stated once rather than twice.
func TestJobStore_CreateJobSubmission_DoesNotAliasCallerMemory(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			sub := submissionFixture(ctx, t, st)
			// Job.Parameters is here because the fake's copyMap on it is one of
			// the deviations from its own per-row CreateJob, which copies
			// nothing; without this the deviation would be untested.
			sub.Job.Parameters = map[string]string{"k": "v"}
			sub.Steps[1].DependsOn = []string{"a"}
			sub.Tasks[0].Parameters = map[string]string{"frame": "1"}

			if _, err := st.CreateJobSubmission(ctx, sub); err != nil {
				t.Fatalf("CreateJobSubmission: %v", err)
			}

			sub.Job.Parameters["k"] = "mutated"
			sub.Steps[1].DependsOn[0] = "mutated"
			sub.Tasks[0].Parameters["frame"] = "mutated"

			job, err := st.GetJob(ctx, "job-1")
			if err != nil {
				t.Fatalf("GetJob: %v", err)
			}
			if job.Parameters["k"] != "v" {
				t.Errorf("stored job parameter = %q, want v; the store aliased caller memory", job.Parameters["k"])
			}

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

// TestJobStore_CreateJobSubmission_RejectsDuplicateIDs pins that a submission
// reusing a step or task ID is REJECTED rather than accepted with rows
// silently dropped.
//
// SQLite gets this from the steps and tasks PRIMARY KEY. The fake had to be
// taught it: its maps are keyed by ID, so a duplicate overwrote, and the call
// returned a JobSubmission of the submitted length while ListSteps returned
// one fewer — reporting success having lost a row. A Submit regression that
// reused an ID would have been green through every fake-backed test in
// internal/openjd, internal/api and internal/scheduler, and ErrConflict only
// in production.
func TestJobStore_CreateJobSubmission_RejectsDuplicateIDs(t *testing.T) {
	cases := map[string]func(sub *store.JobSubmission){
		"duplicate step ID": func(sub *store.JobSubmission) { sub.Steps[1].ID = sub.Steps[0].ID },
		"duplicate task ID": func(sub *store.JobSubmission) { sub.Tasks[1].ID = sub.Tasks[0].ID },
	}
	for caseName, mutate := range cases {
		// newStores is called per case so each subtest gets a store with no
		// farm-1/queue-1 left over from the previous one.
		for name, st := range newStores(t) {
			t.Run(caseName+"/"+name, func(t *testing.T) {
				ctx := context.Background()
				sub := submissionFixture(ctx, t, st)
				mutate(&sub)

				if _, err := st.CreateJobSubmission(ctx, sub); err == nil {
					t.Fatalf("CreateJobSubmission accepted a %s", caseName)
				}
				if _, err := st.GetJob(ctx, "job-1"); err == nil {
					t.Error("the job row survived a rejected submission")
				}
				steps, err := st.ListSteps(ctx, "job-1")
				if err == nil && len(steps) != 0 {
					t.Errorf("%d step rows survived a rejected submission, want 0", len(steps))
				}
			})
		}
	}
}

// TestJobStore_CreateJobSubmission_StampsDistinctRowTimestamps pins that every
// step and task in one submission gets its OWN created_at.
//
// It is sqlite-only on purpose. Two SQLite consumers depend on this — the
// t.created_at tiebreaker in sqlListReadyTasks and ListTasks' single-column
// ORDER BY with LIMIT, which has no secondary key and therefore no stable page
// boundaries when the sort key ties (see insertTasksTx). Neither exists in the
// fake, whose insert loop is also far faster than this platform's wall clock
// advances, so asserting distinctness there would be flaky for no benefit.
//
// A future reintroduction of one shared timestamp for the whole batch must
// fail here rather than pass and quietly change dispatch order.
func TestJobStore_CreateJobSubmission_StampsDistinctRowTimestamps(t *testing.T) {
	st, ok := newStores(t)["sqlite"]
	if !ok {
		t.Fatal("newStores did not provide a sqlite backend")
	}
	ctx := context.Background()
	sub := submissionFixture(ctx, t, st)

	out, err := st.CreateJobSubmission(ctx, sub)
	if err != nil {
		t.Fatalf("CreateJobSubmission: %v", err)
	}

	// task-1 and task-2 are both in step-1, which is exactly where
	// sqlListReadyTasks relies on created_at to break the tie.
	seen := make(map[time.Time]string, len(out.Tasks))
	for _, tk := range out.Tasks {
		if other, dup := seen[tk.CreatedAt]; dup {
			t.Errorf("tasks %s and %s share created_at %v; the ordering tiebreaker is inert",
				other, tk.ID, tk.CreatedAt)
		}
		seen[tk.CreatedAt] = tk.ID
	}

	stepTimes := make(map[time.Time]string, len(out.Steps))
	for _, s := range out.Steps {
		if other, dup := stepTimes[s.CreatedAt]; dup {
			t.Errorf("steps %s and %s share created_at %v", other, s.ID, s.CreatedAt)
		}
		stepTimes[s.CreatedAt] = s.ID
	}

	// The stored rows, not just the returned ones.
	stored, err := st.ListTasks(ctx, store.ListTasksOptions{JobID: "job-1"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	storedTimes := make(map[time.Time]string, len(stored.Items))
	for _, tk := range stored.Items {
		if other, dup := storedTimes[tk.CreatedAt]; dup {
			t.Errorf("stored tasks %s and %s share created_at %v", other, tk.ID, tk.CreatedAt)
		}
		storedTimes[tk.CreatedAt] = tk.ID
	}
}
