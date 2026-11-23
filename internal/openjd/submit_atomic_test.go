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

// submitSpy wraps the fake store and records its one row-creating method,
// CreateJobSubmission. It delegates it, so the store still behaves exactly
// like the fake; the counters only observe.
//
// It is not a general write recorder. It embeds [fake.Store], so every other
// method reaches the fake untouched and is invisible to `writes`: a change
// that persisted something through another store method would leave the
// counters unchanged and TestSubmit_PersistsInASingleCall green. [store.Store]
// has no per-row creators for the job row, its dependency edges, a step or a
// task, so Submit cannot spread a submission over several calls, and
// CreateJobSubmission is the only row-creating call to count, which makes
// `writes` and `submissions` move together.
// A separate job-status write is likewise not among them: [store.Store] has no
// generic job-status writer, so a Submit holding one cannot call it at all.
//
// A failed submission must leave no rows and, when expansion fails, must not
// have attempted a write at all. The first is observable from the store; the
// second needs the spy, because a rolled-back write looks the same as no write
// on inspection.
type submitSpy struct {
	*fake.Store

	// jobIDs collects the ID of every job whose creation was attempted. Steps
	// have no store-wide listing, so this is the only handle on which job's
	// steps to look for.
	jobIDs []string
	// writes equals submissions: CreateJobSubmission is the only
	// row-creating call, so both count the same calls.
	writes int
	// submissions counts CreateJobSubmission calls specifically.
	submissions int
	// lastSubmission is the argument of the most recent CreateJobSubmission
	// call. It is how a test inspects what Submit asked the store to write, as
	// opposed to what the store then made of it.
	lastSubmission store.JobSubmission
	// failSubmission, when non-nil, is returned by CreateJobSubmission instead
	// of delegating. It injects a store failure during the submission write
	// (a client disconnect, a full disk, a transient DB error), the only way
	// to fail a submission that has already passed expansion.
	failSubmission error
	// beforeSubmission, when non-nil, runs once at the top of
	// CreateJobSubmission, before it delegates. It lets a test change store
	// state between Submit's dependency pre-read and the submission write.
	beforeSubmission func()
}

func (s *submitSpy) CreateJobSubmission(ctx context.Context, sub store.JobSubmission) (store.JobSubmission, error) {
	s.jobIDs = append(s.jobIDs, sub.Job.ID)
	s.writes++
	s.submissions++
	s.lastSubmission = sub
	if s.beforeSubmission != nil {
		s.beforeSubmission()
	}
	if s.failSubmission != nil {
		return store.JobSubmission{}, s.failSubmission
	}
	return s.Store.CreateJobSubmission(ctx, sub)
}

// twoStepsSecondOverTaskCap returns a two-step template whose second step
// cannot expand.
//
// The over-cap step must fail at expansion: validation precedes every write,
// so a template rejected there never reaches the store either way. Two INT
// parameters of 1024 values each are individually legal (maxTaskParamValues
// is 1024 and the check is "greater than"), but their Cartesian product is
// 1,048,576, over expand.go's always-on maxTasksPerStep of 1,000,000.
// countCombNode multiplies instead of materializing, so this is fast; it is
// the case that constant's doc comment cites.
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
// tests, which check what the write leaves behind.
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

// TestSubmit_FailedSubmissionLeavesNoRows drives the orphaned-pending-job
// failure mode: a submission that fails partway must not leave a job row,
// because nothing reaps one: retention deletes only terminal statuses,
// demoteStalledJobs needs a running job with live tasks, and the handler never
// learns the job ID because Submit returns nil on error.
//
// If each step row were written before its tasks were expanded, an expansion
// failure on step 2 would leave both step rows, with Step2 having no tasks,
// and the job would hang in pending. A step row missing entirely, which
// checkJobCompletion would read as a completed job, needs a store failure
// instead; that case is TestSubmit_StoreFailureLeavesNoRows.
//
// Both are properties of partial creation, and the assertion here rules them
// out together: a failed submission leaves zero rows.
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
	// Guard the fixture: the failure must come from the task-count cap applied
	// during expansion. A failure in validation would say nothing about
	// partial writes.
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
	// nothing to look up; TestSubmit_ExpansionFailureNeverTouchesTheStore
	// asserts that directly.
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

// TestSubmit_StoreFailureLeavesNoRows drives the failure mode that loses a
// step: the store failing during the write, as a client disconnect, a full
// disk or a transient DB error would. An expansion failure cannot produce it
// (see TestSubmit_FailedSubmissionLeavesNoRows).
//
// If rows were written one at a time, a store failure on the second step's
// write would leave a job with only Step1 persisted. checkJobCompletion
// derives job status from the steps that exist, so that job would be reported
// completed having lost Step2's work.
//
// A store failure must surface to the caller and leave no job, step or task
// row. The rollback is tested at the store layer on both backends by
// TestJobStore_CreateJobSubmission_RollsBackEntirely; this test pins that
// Submit routes the whole submission through that one call.
//
// The injection is on CreateJobSubmission, so a Submit that wrote rows through
// any other path would succeed and trip the fatal below.
func TestSubmit_StoreFailureLeavesNoRows(t *testing.T) {
	inner := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, inner)
	st := &submitSpy{Store: inner, failSubmission: errors.New("injected store failure")}
	sub := openjd.NewSubmitter(st)

	// A well-formed two-step template, so that the submission reaches the
	// store and the injected failure fires.
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
	// ID whose creation was attempted; here there is one, because the
	// submission reached the store.
	for _, jobID := range st.jobIDs {
		steps, serr := st.ListSteps(t.Context(), jobID)
		if serr != nil {
			t.Fatalf("ListSteps(%s): %v", jobID, serr)
		}
		if len(steps) != 0 {
			t.Errorf("%d step rows survived a failed store write for job %s, want 0", len(steps), jobID)
		}
	}
	// Both step rows must be part of that one call and not written ahead of
	// it: a job persisted with fewer steps than its template declares is the
	// step-losing failure mode above.
	if got := len(st.lastSubmission.Steps); got != 2 {
		t.Errorf("the submission handed to the store carried %d steps, want both", got)
	}
}

// TestSubmit_ExpansionFailureNeverTouchesTheStore pins the ordering:
// expansion completes before the single write, so a bad template never
// reaches the store. Per-step writes with a correct rollback would pass the
// row-count tests above but fail this one.
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
// several calls could be left partially written, so the single call is the
// property under test.
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

	// The result carries the stored rows.
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
// A separate status write issued after that call would commit "this job is
// blocked" and "here is what it is blocked on" independently. [store.Store]
// has no generic job-status writer, so Submit cannot issue one. This test
// observes the other side: the submission handed to the store carries the
// blocked status, and it is the only row-creating call Submit makes. A change
// that adds a second write fails here even if the end state is correct.
func TestSubmit_BlockedStatusIsPartOfTheAtomicWrite(t *testing.T) {
	st, sub, farmID, queueID, upstreamID := newBlockedSubmitFixture(t)

	// The fixture's own upstream submission has already been counted.
	writesBefore, submissionsBefore := st.writes, st.submissions

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
	if writes, submissions := st.writes-writesBefore, st.submissions-submissionsBefore; writes != 1 || submissions != 1 {
		t.Errorf("Submit made %d row-creating calls (%d of them CreateJobSubmission), want exactly 1: the status belongs to the atomic write",
			writes, submissions)
	}
	if result.Job.Status != store.JobStatusBlocked {
		t.Errorf("result job status = %q, want blocked", result.Job.Status)
	}
}

// TestSubmit_BlockedStatusIsAtomicWithTheRows is the failure-mode test for
// the blocked status.
//
// If the status were written separately, a failure of that write would leave
// the job, its edges, its steps and its tasks all durable with the job
// stranded in pending. buildStepWithTasks creates every task pending whenever
// the job is held, so nothing runs; ReconcileBlockedJob
// (internal/scheduler/jobdeps.go) early-returns unless status is blocked, so
// neither sweepBlockedJobs nor ReconcileDependents ever revisits the row; and
// the scheduler leases only ready tasks. The job hangs until an operator
// intervenes.
//
// So the assertion is that no row for the job exists. Asserting that the
// tasks are not ready would pass even with the stranded job, because they are
// never ready.
//
// The two subtests go through CreateJobSubmission, the one write call Submit
// makes: when it succeeds the persisted job is blocked and whole, and when it
// fails Submit leaves no job row, because it has no second write path (a
// compensating status write, a per-row fallback) that could strand one. The
// spy returns the injected failure without delegating, so the store writes
// nothing either way; the failure case pins that Submit itself writes nothing
// else. The rollback is tested at the store layer by
// TestJobStore_CreateJobSubmission_RollsBackEntirely.
func TestSubmit_BlockedStatusIsAtomicWithTheRows(t *testing.T) {
	t.Run("success is whole", func(t *testing.T) {
		st, sub, farmID, queueID, upstreamID := newBlockedSubmitFixture(t)

		result, err := sub.Submit(t.Context(), minimalJSON("BlockedJob"), store.TemplateFormatJSON, openjd.SubmitOptions{
			FarmID:    farmID,
			QueueID:   queueID,
			DependsOn: []string{upstreamID},
		})
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}

		job, gerr := st.GetJob(t.Context(), result.Job.ID)
		if gerr != nil {
			t.Fatalf("GetJob(%s): %v", result.Job.ID, gerr)
		}
		if job.Status != store.JobStatusBlocked {
			t.Errorf("persisted job status = %q, want blocked", job.Status)
		}
		if result.Job.Status != store.JobStatusBlocked {
			t.Errorf("result job status = %q, want blocked", result.Job.Status)
		}
	})

	t.Run("failure leaves no rows", func(t *testing.T) {
		st, sub, farmID, queueID, upstreamID := newBlockedSubmitFixture(t)
		st.failSubmission = errors.New("submission write failed")

		_, err := sub.Submit(t.Context(), minimalJSON("BlockedJob"), store.TemplateFormatJSON, openjd.SubmitOptions{
			FarmID:    farmID,
			QueueID:   queueID,
			DependsOn: []string{upstreamID},
		})
		if err == nil {
			t.Fatal("Submit reported success although the store failed the write")
		}

		// The job ID is taken from the spy rather than the result, because a
		// failed Submit returns nil and the caller never learns which row to
		// look for, so a stranded row could not be cleaned up.
		jobID := st.lastSubmission.Job.ID
		if jobID == "" {
			t.Fatal("Submit never reached the store; the test cannot observe what it left behind")
		}
		if _, gerr := st.GetJob(t.Context(), jobID); !errors.Is(gerr, store.ErrNotFound) {
			t.Fatalf("a job row survived a failed submission (GetJob(%s) = %v), want ErrNotFound", jobID, gerr)
		}
		edges, lerr := st.ListJobDependencyIDs(t.Context(), jobID)
		if lerr != nil {
			t.Fatalf("ListJobDependencyIDs: %v", lerr)
		}
		if len(edges) != 0 {
			t.Errorf("dependency edges %v survived a failed submission, want none", edges)
		}
	})
}

// TestSubmit_UpstreamLostBeforeTheWrite_IsAValidationError pins the
// submitter's handling of an upstream lost mid-submission: resolveDependencies
// reads each upstream before the submission transaction, so an upstream can be
// canceled or deleted in between. The store refuses the write with
// [store.ErrDependencyUnsatisfiable], and Submit reports it as a
// *SubmitValidationError, the client-fault class (HTTP 422) that
// resolveDependencies itself uses, rather than as an internal error (HTTP 500).
func TestSubmit_UpstreamLostBeforeTheWrite_IsAValidationError(t *testing.T) {
	for name, lose := range map[string]func(t *testing.T, st *submitSpy, upstreamID string){
		"canceled": func(t *testing.T, st *submitSpy, upstreamID string) {
			t.Helper()
			if err := st.CancelJobStatus(t.Context(), upstreamID); err != nil {
				t.Fatalf("CancelJobStatus: %v", err)
			}
		},
		"deleted": func(t *testing.T, st *submitSpy, upstreamID string) {
			t.Helper()
			if err := st.DeleteJob(t.Context(), upstreamID); err != nil {
				t.Fatalf("DeleteJob: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			st, sub, farmID, queueID, upstreamID := newBlockedSubmitFixture(t)
			st.beforeSubmission = func() { lose(t, st, upstreamID) }

			_, err := sub.Submit(t.Context(), minimalJSON("BlockedJob"), store.TemplateFormatJSON, openjd.SubmitOptions{
				FarmID:    farmID,
				QueueID:   queueID,
				DependsOn: []string{upstreamID},
			})
			if _, ok := errors.AsType[*openjd.SubmitValidationError](err); !ok {
				t.Fatalf("Submit = %v, want a *SubmitValidationError", err)
			}
			if !errors.Is(err, store.ErrDependencyUnsatisfiable) {
				t.Errorf("Submit = %v, want it to wrap ErrDependencyUnsatisfiable", err)
			}
			if _, gerr := st.GetJob(t.Context(), st.lastSubmission.Job.ID); !errors.Is(gerr, store.ErrNotFound) {
				t.Errorf("a job row survived the refused submission (GetJob = %v)", gerr)
			}
		})
	}
}

// TestSubmit_BlockedJobIsNeverObservableWithoutItsEdges pins that a blocked
// job and its dependency edges are never observed apart.
//
// If a job were created already-blocked before its edges were written, a
// sweepBlockedJobs tick could land in between, see a blocked job with zero
// edges, read that as "nothing left to wait on", and release it, leaving a job
// neither blocked nor scheduled that the sweep never revisits. Writing status
// and edges in one transaction prevents that.
//
// The window is not observable from a single-threaded test. This asserts that
// the two arrive together: a persisted blocked job has its edges.
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
