// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// submitSpy wraps the fake store and records six of its methods: the five row
// creators a submission could use — CreateJob, CreateJobDependencies,
// CreateStep, CreateTask, CreateJobSubmission — and UpdateJobStatus. It
// delegates each one, so the store still behaves exactly like the fake; the
// counters only observe.
//
// It is NOT a general write recorder. It embeds [fake.Store], so every other
// method reaches the fake untouched and is invisible to `writes`: a regression
// that persisted something through some other store method would leave the
// counters unchanged and TestSubmit_PersistsInASingleCall green. The six are
// the ones the pre-atomic Submit used, plus the one that replaced them, which
// is what the counters exist to detect a return to.
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
	// lastSubmission is the argument of the most recent CreateJobSubmission
	// call. It is how a test inspects what Submit asked the store to write, as
	// opposed to what the store then made of it.
	lastSubmission store.JobSubmission
	// statusUpdates counts UpdateJobStatus calls. A submission must make none:
	// every status a submission decides is part of the one atomic write.
	statusUpdates int
	// failStatusUpdate, when non-nil, is returned by UpdateJobStatus instead of
	// delegating. It injects the failure of the write that used to run after
	// the atomic one.
	failStatusUpdate error
	// failSubmission, when non-nil, is returned by CreateJobSubmission instead
	// of delegating. It injects a store failure during the submission write --
	// a client disconnect, a full disk, a transient DB error -- which is the
	// only way to fail a submission that has already passed expansion.
	failSubmission error
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
	s.lastSubmission = sub
	if s.failSubmission != nil {
		return store.JobSubmission{}, s.failSubmission
	}
	return s.Store.CreateJobSubmission(ctx, sub)
}

func (s *submitSpy) UpdateJobStatus(ctx context.Context, id string, status store.JobStatus) error {
	s.statusUpdates++
	if s.failStatusUpdate != nil {
		return s.failStatusUpdate
	}
	return s.Store.UpdateJobStatus(ctx, id, status)
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

// twoStepsBothValid returns a two-step template that expands cleanly, so a
// submission of it reaches the store. It is the fixture for store-failure
// tests, where the point is what the write leaves behind rather than whether
// the template is acceptable.
func twoStepsBothValid(name string) string {
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
      "script": { "actions": { "onRun": { "command": "echo", "args": ["world"] } } }
    }
  ]
}`
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestSubmit_FailedSubmissionLeavesNoRows drives the failure mode of DEFECT 1,
// orphaned pending jobs: a submission that failed partway used to leave a job
// row that no sweep reaps — retention deletes only terminal statuses,
// demoteStalledJobs needs a running job with live tasks, and the handler never
// learns the job ID because Submit returns nil on error.
//
// Be precise about what this fixture demonstrates and what it does not.
// Measured against main, an expansion failure on step 2 left BOTH step rows,
// not one: createStepWithTasks wrote each step row BEFORE expanding its tasks,
// so the job persisted with all its steps and Step2 simply had no tasks — an
// orphan hung in pending, which is defect 1. It is NOT defect 2's mechanism;
// reaching a job that checkJobCompletion mis-reports as completed needs a step
// row to be missing entirely, which needs a STORE failure. That case is
// TestSubmit_StoreFailureLeavesNoRows below.
//
// Both defects are properties of partial creation, and the assertion here is
// the general one that closes them: a failed submission leaves ZERO rows. A
// step-count guard would only prove the guard works.
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

// TestSubmit_StoreFailureLeavesNoRows drives the failure DEFECT 2 actually
// needs: the store failing during the write, as a client disconnect, a full
// disk or a transient DB error would. An expansion failure cannot produce it
// (see TestSubmit_FailedSubmissionLeavesNoRows), which is why this exists as a
// separate case rather than as prose attached to that one.
//
// Reproduced on main by failing the second CreateStep:
//
//	SUBMIT err=openjd: submit: create step "Step2": injected store failure
//	template declares 2 steps; 1 persisted: "Step1" status="ready"
//	DEFECT 2: job that silently lost Step2 has final status = "completed"
//
// checkJobCompletion derives job status from the steps that EXIST, so a job
// missing Step2 entirely was reported completed having silently lost work.
//
// What this pins on HEAD is the property that makes that unconstructible: a
// store failure surfaces to the caller and leaves no job, step or task row, so
// there is no truncated job for checkJobCompletion to see. The rollback itself
// is proven at the store layer on both backends by
// TestJobStore_CreateJobSubmission_RollsBackEntirely; what is proven here is
// that Submit routes the entire submission through that one guarded call, so a
// store failure has nothing partial to leave behind.
//
// The injection is on CreateJobSubmission, which also keeps the test honest
// against a regression: a Submit that went back to per-row writes would never
// call it, would therefore SUCCEED, and would trip the fatal below rather than
// passing vacuously.
func TestSubmit_StoreFailureLeavesNoRows(t *testing.T) {
	inner := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, inner)
	st := &submitSpy{Store: inner, failSubmission: errors.New("injected store failure")}
	sub := openjd.NewSubmitter(st)

	// A well-formed two-step template: it must reach the store, unlike the
	// over-cap fixture, or the store failure never gets a chance to fire.
	_, err := sub.Submit(t.Context(), twoStepsBothValid("StoreFailureJob"), store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
		Owner:   "alice",
	})
	if err == nil {
		t.Fatal("Submit reported success although the store failed the write")
	}
	if !strings.Contains(err.Error(), "injected store failure") {
		t.Fatalf("the store's error did not reach the caller: %v", err)
	}
	if st.submissions != 1 {
		t.Fatalf("CreateJobSubmission called %d times, want 1", st.submissions)
	}

	jobs, err := st.ListJobs(t.Context(), store.ListJobsOptions{})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("%d job rows survived a failed store write, want 0", len(jobs.Items))
	}

	tasks, err := st.ListTasks(t.Context(), store.ListTasksOptions{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks.Items) != 0 {
		t.Errorf("%d task rows survived a failed store write, want 0", len(tasks.Items))
	}

	// Steps have no store-wide listing, so they are checked against every job
	// ID whose creation was attempted -- which here is a real ID, because the
	// submission did reach the store.
	for _, jobID := range st.jobIDs {
		steps, serr := st.ListSteps(t.Context(), jobID)
		if serr != nil {
			t.Fatalf("ListSteps(%s): %v", jobID, serr)
		}
		if len(steps) != 0 {
			t.Errorf("%d step rows survived a failed store write for job %s, want 0", len(steps), jobID)
		}
	}
	// The step rows the failed write would have created must have been part of
	// that one call, not written ahead of it: a job persisted with fewer steps
	// than its template declares is precisely defect 2.
	if got := len(st.lastSubmission.Steps); got != 2 {
		t.Errorf("the submission handed to the store carried %d steps, want both", got)
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

// ── blocked submissions ───────────────────────────────────────────────────────

// newBlockedSubmitFixture returns a spy-wrapped fake store, a submitter over it,
// the farm and queue to submit into, and the ID of an upstream job left pending
// so that anything depending on it is created blocked.
func newBlockedSubmitFixture(t *testing.T) (st *submitSpy, sub *openjd.Submitter, farmID, queueID, upstreamID string) {
	t.Helper()
	inner := fake.New()
	farmID, queueID = seedSubmitPrereqs(t, inner)
	st = &submitSpy{Store: inner}
	sub = openjd.NewSubmitter(st)

	up, err := sub.Submit(t.Context(), minimalJSON("UpstreamJob"), store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
	})
	if err != nil {
		t.Fatalf("Submit(upstream): %v", err)
	}
	return st, sub, farmID, queueID, up.Job.ID
}

// TestSubmit_BlockedStatusIsPartOfTheAtomicWrite pins where the blocked status
// is decided: inside the single [store.JobStore.CreateJobSubmission] call, next
// to the dependency edges that justify it.
//
// It used to be a separate UpdateJobStatus issued after that call, so the two
// halves of one decision — "this job is blocked" and "here is what it is
// blocked on" — committed independently. Asserting that Submit issues no status
// update at all is what makes the coupling structural rather than incidental: a
// change that reintroduces the second write fails here even if it happens to
// leave the end state correct.
func TestSubmit_BlockedStatusIsPartOfTheAtomicWrite(t *testing.T) {
	st, sub, farmID, queueID, upstreamID := newBlockedSubmitFixture(t)

	result, err := sub.Submit(t.Context(), minimalJSON("BlockedJob"), store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:    farmID,
		QueueID:   queueID,
		DependsOn: []string{upstreamID},
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if got := st.lastSubmission.Job.Status; got != store.JobStatusBlocked {
		t.Errorf("the submission handed to the store carried status %q, want blocked", got)
	}
	if got := st.lastSubmission.DependsOn; len(got) != 1 || got[0] != upstreamID {
		t.Errorf("the submission handed to the store carried DependsOn %v, want [%s]", got, upstreamID)
	}
	if st.statusUpdates != 0 {
		t.Errorf("Submit issued %d UpdateJobStatus calls, want 0 (the status belongs to the atomic write)", st.statusUpdates)
	}
	if result.Job.Status != store.JobStatusBlocked {
		t.Errorf("result job status = %q, want blocked", result.Job.Status)
	}
}

// TestSubmit_BlockedStatusIsAtomicWithTheRows is the failure-mode test, and the
// reason folding the status write in was mandatory rather than tidy.
//
// While the status was written separately, a failure of that write left the
// job, its edges, its steps and its tasks all durable with the job stranded in
// pending — and stranded is literal. buildStepWithTasks creates every task
// pending whenever the job is held, so nothing runs; reconcileBlockedJob
// (internal/scheduler/jobdeps.go) early-returns unless status is blocked, so
// neither sweepBlockedJobs nor ReconcileDependents ever revisits the row; and
// the scheduler leases only ready tasks. The job hangs until an operator
// intervenes.
//
// That is why the assertion here is that NO row for the job exists. A test
// written to the failure mode this one was first thought to have — "assert the
// tasks are not ready" — passes without the fix, because they were never ready.
//
// What it actually exercises on HEAD, stated plainly: the injected failure is on
// UpdateJobStatus, which Submit no longer calls, so err is always nil here and
// the err != nil branch — the zero-rows assertion — is DEAD CODE today. The live
// assertion is the success branch: the submission completed and the persisted
// job is blocked. The dead branch is kept deliberately, as the guard for a
// regression that reintroduces a separate status write: such a change would make
// this injection fire again, and the branch would then assert what it was
// written to assert. Either branch is atomic; a job row surviving in the wrong
// status is not.
func TestSubmit_BlockedStatusIsAtomicWithTheRows(t *testing.T) {
	st, sub, farmID, queueID, upstreamID := newBlockedSubmitFixture(t)
	st.failStatusUpdate = errors.New("status write failed")

	result, err := sub.Submit(t.Context(), minimalJSON("BlockedJob"), store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:    farmID,
		QueueID:   queueID,
		DependsOn: []string{upstreamID},
	})

	// The job ID is taken from the spy rather than the result, because a failed
	// Submit returns nil and the caller never learns which row to look for --
	// which is exactly why a stranded row could not be cleaned up.
	jobID := st.lastSubmission.Job.ID
	if jobID == "" {
		t.Fatal("Submit never reached the store; the test cannot observe what it left behind")
	}

	if err != nil {
		if _, gerr := st.GetJob(t.Context(), jobID); !errors.Is(gerr, store.ErrNotFound) {
			t.Fatalf("a job row survived a failed submission (GetJob(%s) = %v), want ErrNotFound", jobID, gerr)
		}
		return
	}

	// The status write is gone, so the submission succeeded: it must be whole.
	job, gerr := st.GetJob(t.Context(), jobID)
	if gerr != nil {
		t.Fatalf("GetJob(%s): %v", jobID, gerr)
	}
	if job.Status != store.JobStatusBlocked {
		t.Errorf("persisted job status = %q, want blocked", job.Status)
	}
	if result.Job.Status != store.JobStatusBlocked {
		t.Errorf("result job status = %q, want blocked", result.Job.Status)
	}
}

// TestSubmit_BlockedJobIsNeverObservableWithoutItsEdges pins the end state that
// the old ordering existed to protect.
//
// Creating a job already-blocked used to let a sweepBlockedJobs tick land after
// the job row existed but before its edges were written, see a blocked job with
// ZERO edges, read that as "nothing left to wait on", and release it — leaving a
// job neither blocked nor scheduled, which the sweep never revisits. Writing the
// status last is how that was avoided; writing status and edges in one
// transaction is how it is avoided now.
//
// The window itself is not observable from a single-threaded test. What is
// observable, and what this asserts, is that the two always arrive together:
// a persisted blocked job has its edges.
func TestSubmit_BlockedJobIsNeverObservableWithoutItsEdges(t *testing.T) {
	st, sub, farmID, queueID, upstreamID := newBlockedSubmitFixture(t)

	result, err := sub.Submit(t.Context(), minimalJSON("BlockedJob"), store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:    farmID,
		QueueID:   queueID,
		DependsOn: []string{upstreamID},
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	blocked, err := st.ListBlockedJobs(t.Context())
	if err != nil {
		t.Fatalf("ListBlockedJobs: %v", err)
	}
	if len(blocked) != 1 || blocked[0].ID != result.Job.ID {
		t.Fatalf("ListBlockedJobs = %d jobs, want just %s", len(blocked), result.Job.ID)
	}

	// This is the read sweepBlockedJobs makes of every job it finds blocked.
	// Zero edges here is what it would misread as "nothing left to wait on".
	ids, err := st.ListJobDependencyIDs(t.Context(), blocked[0].ID)
	if err != nil {
		t.Fatalf("ListJobDependencyIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != upstreamID {
		t.Fatalf("a blocked job's dependency edges = %v, want [%s]", ids, upstreamID)
	}
}
