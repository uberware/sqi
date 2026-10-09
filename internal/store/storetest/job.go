// SPDX-License-Identifier: AGPL-3.0-or-later

package storetest

import (
	"slices"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// JobSeed is one job's whole graph — the job, its steps and its tasks — which
// [JobSeed.Submit] writes with a single CreateJobSubmission. A job starts
// pending at priority 50 with no steps and no tasks, every row named after its
// ID; the chained methods add to it, and a row is written in whatever status it
// is given, exactly as CreateJobSubmission writes it.
//
// A task asked for assigned or running is not written that way: submit it with
// [JobSeed.SubmitLeasing], which leases it, so it carries the attempt and the
// worker a real lease makes.
type JobSeed struct {
	// Sub is the submission built so far, for a test that needs a field the
	// methods do not set.
	Sub store.JobSubmission
}

// NewJob starts a pending job at priority 50, named after its ID.
func NewJob(id, farmID, queueID string) *JobSeed {
	return &JobSeed{Sub: store.JobSubmission{Job: store.Job{
		ID: id, FarmID: farmID, QueueID: queueID, Name: id,
		Status: store.JobStatusPending, Priority: 50, TemplateFormat: store.TemplateFormatYAML,
	}}}
}

// As sets the job's status, stamping the timestamps a job in that status
// carries: started_at once it has run, completed_at once it is terminal.
func (j *JobSeed) As(status store.JobStatus) *JobSeed {
	now := time.Now().UTC()
	j.Sub.Job.Status = status
	switch status {
	case store.JobStatusRunning:
		j.Sub.Job.StartedAt = &now
	case store.JobStatusCompleted, store.JobStatusFailed, store.JobStatusCanceled:
		j.Sub.Job.CompletedAt = &now
	}
	return j
}

// Step adds a step in the given status, named after its ID and ordered after
// the steps before it, or sets the status of the step the job already holds
// under that ID.
func (j *JobSeed) Step(id string, status store.StepStatus) *JobSeed {
	for i := range j.Sub.Steps {
		if j.Sub.Steps[i].ID == id {
			j.Sub.Steps[i].Status = status
			return j
		}
	}
	return j.StepRow(store.Step{ID: id, StepOrder: len(j.Sub.Steps), Status: status})
}

// StepRow adds the given step, filling in its job, its name (its ID) when it
// has none, and empty dependencies.
func (j *JobSeed) StepRow(step store.Step) *JobSeed {
	step.JobID = j.Sub.Job.ID
	if step.Name == "" {
		step.Name = step.ID
	}
	if step.DependsOn == nil {
		step.DependsOn = []string{}
	}
	j.Sub.Steps = append(j.Sub.Steps, step)
	return j
}

// Task adds a task of stepID in the given status. A task and its step are one
// submission, so a pending step is added when the job holds none by that ID;
// call Step first to give the step another status. Step IDs are unique across
// jobs, as in the store, so two jobs in one test name different steps.
func (j *JobSeed) Task(id, stepID string, status store.TaskStatus) *JobSeed {
	return j.TaskRow(store.Task{ID: id, StepID: stepID, Status: status})
}

// TaskRow adds the given task, filling in its job, its name (its ID) when it
// has none and empty parameters, and adding its step as Task does.
func (j *JobSeed) TaskRow(task store.Task) *JobSeed {
	task.JobID = j.Sub.Job.ID
	if task.Name == "" {
		task.Name = task.ID
	}
	if task.Parameters == nil {
		task.Parameters = map[string]string{}
	}
	if !slices.ContainsFunc(j.Sub.Steps, func(s store.Step) bool { return s.ID == task.StepID }) {
		j.Step(task.StepID, store.StepStatusPending)
	}
	j.Sub.Tasks = append(j.Sub.Tasks, task)
	return j
}

// Submit writes the whole graph through [Submit] and returns what the store
// wrote.
func (j *JobSeed) Submit(t testing.TB, st store.Store) store.JobSubmission {
	t.Helper()
	return Submit(t, st, j.Sub)
}

// SubmitLeasing writes the whole graph through [SubmitLeasing], leasing each
// assigned or running task to workerID.
func (j *JobSeed) SubmitLeasing(
	t testing.TB, st store.Store, workerID string,
) (out store.JobSubmission, attempts map[string]store.TaskAttempt) {
	t.Helper()
	return SubmitLeasing(t, st, j.Sub, LeaseTo(workerID))
}
