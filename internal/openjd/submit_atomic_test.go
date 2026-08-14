// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"context"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// submitSpy wraps the fake store and records every row-creating call a
// submission makes. It delegates each one, so the store still behaves exactly
// like the fake; the counters only observe.
//
// It exists because a failed submission must leave no rows AND, once expansion
// runs to completion first, must not have attempted a write at all. The former
// is observable from the store; the latter is not, because a perfectly
// rolled-back write is indistinguishable from no write by inspection alone.
type submitSpy struct {
	*fake.Store

	// jobIDs collects the ID of every job whose creation was attempted, by
	// either the per-row or the bulk path. Steps have no store-wide listing, so
	// this is the only handle on which job's steps to look for.
	jobIDs []string
	// writes counts every attempted row-creating call of any kind.
	writes int
	// submissions counts CreateJobSubmission calls specifically.
	submissions int
}

func (s *submitSpy) CreateJob(ctx context.Context, job store.Job) (store.Job, error) {
	s.jobIDs = append(s.jobIDs, job.ID)
	s.writes++
	return s.Store.CreateJob(ctx, job)
}

func (s *submitSpy) CreateJobDependencies(ctx context.Context, jobID string, dependsOn []string) error {
	s.writes++
	return s.Store.CreateJobDependencies(ctx, jobID, dependsOn)
}

func (s *submitSpy) CreateStep(ctx context.Context, step store.Step) (store.Step, error) {
	// Record the job ID here too, not just in CreateJob/CreateJobSubmission.
	//
	// Without this, a regression that writes steps per-step and never reaches
	// the bulk call leaves jobIDs EMPTY, so the surviving-step-rows loop in
	// TestSubmit_FailedSubmissionLeavesNoRows never executes and the test
	// passes while a step row genuinely survives -- defect 2 exactly. That was
	// demonstrated by sabotage during review, not theorized.
	s.jobIDs = append(s.jobIDs, step.JobID)
	s.writes++
	return s.Store.CreateStep(ctx, step)
}

func (s *submitSpy) CreateTask(ctx context.Context, task store.Task) (store.Task, error) {
	s.jobIDs = append(s.jobIDs, task.JobID)
	s.writes++
	return s.Store.CreateTask(ctx, task)
}

func (s *submitSpy) CreateJobSubmission(ctx context.Context, sub store.JobSubmission) (store.JobSubmission, error) {
	s.jobIDs = append(s.jobIDs, sub.Job.ID)
	s.writes++
	s.submissions++
	return s.Store.CreateJobSubmission(ctx, sub)
}

// twoStepsSecondOverTaskCap returns a two-step template whose SECOND step
// cannot expand.
//
// The over-cap step must fail at EXPANSION, not at validation, or the test
// proves nothing: validation already precedes every write today, so a template
// rejected there never reaches the store either way. Two INT parameters of
// 1024 values each are individually legal — maxTaskParamValues is 1024 and the
// check is "greater than" — but their Cartesian product is 1,048,576, over
// expand.go's always-on maxTasksPerStep of 1,000,000. countCombNode multiplies
// rather than materializing, so this is fast, and it is the exact case that
// constant's own doc comment cites as its rationale.
func twoStepsSecondOverTaskCap(name string) string {
	return `{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "` + name + `",
  "steps": [
    {
      "name": "Step1",
      "script": { "actions": { "onRun": { "command": "echo", "args": ["hello"] } } }
    },
    {
      "name": "Step2",
      "script": { "actions": { "onRun": { "command": "echo", "args": ["world"] } } },
      "parameterSpace": {
        "taskParameterDefinitions": [
          { "name": "Frame", "type": "INT", "range": "1-1024" },
          { "name": "Layer", "type": "INT", "range": "1-1024" }
        ]
      }
    }
  ]
}`
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestSubmit_FailedSubmissionLeavesNoRows is the proof for BOTH defects this
// change exists to fix.
//
// Defect 1, orphaned pending jobs: a failed submission used to leave a job row
// that no sweep reaps — retention deletes only terminal statuses,
// demoteStalledJobs needs a running job with live tasks, and the handler never
// learns the job ID because Submit returns nil on error.
//
// Defect 2, a truncated job reported as success: a submission that failed on a
// later step left the earlier steps persisted, and checkJobCompletion derives
// job status from the steps that EXIST — so the job was marked completed
// having silently lost work.
//
// Both are properties of partial creation. Asserting that a failed submission
// leaves ZERO rows is what proves both gone; a step-count guard would only
// prove the guard works.
func TestSubmit_FailedSubmissionLeavesNoRows(t *testing.T) {
	inner := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, inner)
	st := &submitSpy{Store: inner}
	sub := openjd.NewSubmitter(st)

	_, err := sub.Submit(t.Context(), twoStepsSecondOverTaskCap("PartialJob"), store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
		Owner:   "alice",
	})
	if err == nil {
		t.Fatal("Submit accepted a template whose second step cannot expand")
	}
	// Guard the fixture itself: the failure must come from the task-count cap
	// applied during expansion. If it ever starts failing in validation the
	// test still errors, but it stops saying anything about partial writes.
	if !strings.Contains(err.Error(), "too many tasks") {
		t.Fatalf("expected the step to fail at expansion (maxTasksPerStep), got: %v", err)
	}

	jobs, err := st.ListJobs(t.Context(), store.ListJobsOptions{})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("%d job rows survived a failed submission, want 0", len(jobs.Items))
	}

	tasks, err := st.ListTasks(t.Context(), store.ListTasksOptions{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks.Items) != 0 {
		t.Errorf("%d task rows survived a failed submission, want 0", len(tasks.Items))
	}

	// Steps have no store-wide listing, so they are checked against every job
	// ID whose creation was attempted. When nothing was attempted there is
	// nothing to look up — which is itself the property under test, and is
	// asserted directly by TestSubmit_ExpansionFailureNeverTouchesTheStore.
	for _, jobID := range st.jobIDs {
		steps, err := st.ListSteps(t.Context(), jobID)
		if err != nil {
			t.Fatalf("ListSteps(%s): %v", jobID, err)
		}
		if len(steps) != 0 {
			t.Errorf("%d step rows survived a failed submission for job %s, want 0", len(steps), jobID)
		}
	}
}

// TestSubmit_ExpansionFailureNeverTouchesTheStore pins the ordering property
// that makes the above hold for free: expansion now completes entirely before
// the single write, so the common bad-template case never reaches the store.
//
// Without this, a future change could restore per-step writes and still pass
// the test above by getting the rollback right — while reintroducing the long
// window this ordering removes.
func TestSubmit_ExpansionFailureNeverTouchesTheStore(t *testing.T) {
	inner := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, inner)
	st := &submitSpy{Store: inner}
	sub := openjd.NewSubmitter(st)

	if _, err := sub.Submit(t.Context(), twoStepsSecondOverTaskCap("NoWriteJob"), store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
	}); err == nil {
		t.Fatal("Submit accepted a template whose second step cannot expand")
	}

	if st.writes != 0 {
		t.Errorf("a failed expansion attempted %d row-creating store calls, want 0", st.writes)
	}
}

// TestSubmit_PersistsInASingleCall pins that a successful submission reaches
// the store exactly once, through the atomic creator. A submission spread over
// several calls is what made a partial write possible at all, so "one call" is
// the property, not an implementation detail.
func TestSubmit_PersistsInASingleCall(t *testing.T) {
	inner := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, inner)
	st := &submitSpy{Store: inner}
	sub := openjd.NewSubmitter(st)

	result, err := sub.Submit(t.Context(), minimalJSON("SingleWriteJob"), store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if st.submissions != 1 {
		t.Errorf("CreateJobSubmission called %d times, want 1", st.submissions)
	}
	if st.writes != 1 {
		t.Errorf("%d row-creating store calls, want exactly 1 (the atomic submission)", st.writes)
	}

	// The result must still carry the stored rows, unchanged in shape.
	if len(result.Steps) != 1 || len(result.Tasks) != 1 {
		t.Fatalf("result has %d steps and %d tasks, want 1 and 1", len(result.Steps), len(result.Tasks))
	}
	if result.Steps[0].JobID != result.Job.ID {
		t.Errorf("step.JobID = %q, want the job's ID %q", result.Steps[0].JobID, result.Job.ID)
	}
	if result.Tasks[0].StepID != result.Steps[0].ID {
		t.Errorf("task.StepID = %q, want the step's ID %q", result.Tasks[0].StepID, result.Steps[0].ID)
	}
}
