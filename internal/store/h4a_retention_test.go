// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
)

// TestCreateJobSubmission_UnsatisfiableUpstream pins F13 at the store layer: a
// submission whose upstream is missing, failed or canceled is refused inside
// its own transaction, and nothing it would have written survives.
func TestCreateJobSubmission_UnsatisfiableUpstream(t *testing.T) {
	for _, up := range []store.JobStatus{store.JobStatusFailed, store.JobStatusCanceled} {
		for name, st := range newStores(t) {
			t.Run(string(up)+"/"+name, func(t *testing.T) {
				upstream := seedGraph(t, st, graphOpts{jobStatus: up})
				jobID := uuid.NewString()
				_, err := st.CreateJobSubmission(t.Context(), store.JobSubmission{
					Job: store.Job{
						ID: jobID, FarmID: upstream.Farm.ID, QueueID: upstream.Queue.ID,
						Name: "dep", Status: store.JobStatusBlocked, TemplateFormat: store.TemplateFormatJSON,
					},
					DependsOn: []string{upstream.Job.ID},
				})
				if !errors.Is(err, store.ErrDependencyUnsatisfiable) {
					t.Fatalf("CreateJobSubmission = %v, want ErrDependencyUnsatisfiable", err)
				}
				assertSubmissionLeftNothing(t, st, jobID, upstream.Job.ID)
			})
		}
	}
	for name, st := range newStores(t) {
		t.Run("missing/"+name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{})
			jobID := uuid.NewString()
			_, err := st.CreateJobSubmission(t.Context(), store.JobSubmission{
				Job: store.Job{
					ID: jobID, FarmID: g.Farm.ID, QueueID: g.Queue.ID,
					Name: "dep", Status: store.JobStatusBlocked, TemplateFormat: store.TemplateFormatJSON,
				},
				DependsOn: []string{"gone"},
			})
			if !errors.Is(err, store.ErrDependencyUnsatisfiable) {
				t.Fatalf("CreateJobSubmission = %v, want ErrDependencyUnsatisfiable", err)
			}
			assertSubmissionLeftNothing(t, st, jobID, "gone")
		})
	}
}

// TestCreateJobSubmission_SatisfiableUpstream is the converse of the test
// above: an upstream that is still live or already completed is accepted, so
// the F13 check cannot refuse a genuine submission.
func TestCreateJobSubmission_SatisfiableUpstream(t *testing.T) {
	for _, up := range []store.JobStatus{store.JobStatusPending, store.JobStatusRunning, store.JobStatusCompleted} {
		for name, st := range newStores(t) {
			t.Run(string(up)+"/"+name, func(t *testing.T) {
				upstream := seedGraph(t, st, graphOpts{jobStatus: up})
				jobID := uuid.NewString()
				if _, err := st.CreateJobSubmission(t.Context(), store.JobSubmission{
					Job: store.Job{
						ID: jobID, FarmID: upstream.Farm.ID, QueueID: upstream.Queue.ID,
						Name: "dep", Status: store.JobStatusBlocked, TemplateFormat: store.TemplateFormatJSON,
					},
					DependsOn: []string{upstream.Job.ID},
				}); err != nil {
					t.Fatalf("CreateJobSubmission with a %s upstream: %v", up, err)
				}
				ups, err := st.ListJobDependencyIDs(t.Context(), jobID)
				if err != nil {
					t.Fatalf("ListJobDependencyIDs: %v", err)
				}
				if !slices.Equal(ups, []string{upstream.Job.ID}) {
					t.Fatalf("edges = %v, want [%s]", ups, upstream.Job.ID)
				}
			})
		}
	}
}

// assertSubmissionLeftNothing fails when a refused submission left its job row
// or its dependency edge behind.
func assertSubmissionLeftNothing(t *testing.T, st store.Store, jobID, upstreamID string) {
	t.Helper()
	if _, err := st.GetJob(t.Context(), jobID); !errorsIsNotFound(err) {
		t.Fatalf("GetJob after a refused submission = %v, want ErrNotFound", err)
	}
	dependents, err := st.ListDependents(t.Context(), upstreamID)
	if err != nil {
		t.Fatalf("ListDependents: %v", err)
	}
	if slices.Contains(dependents, jobID) {
		t.Fatalf("a refused submission left its edge on %s", upstreamID)
	}
}

// TestDeleteTerminalJobsBefore_SkipsNonTerminal guards the G6 refactor: a job
// that is not terminal is never purged nor reported.
//
// It does NOT reach the re-check branch. On SQLite the store's single write
// connection serializes the whole sweep, so a job cannot turn live between
// the eligibility SELECT and its delete, and the fake holds one lock across
// both. The re-check exists for the PostgreSQL store (H4c), where a concurrent
// RetryTasks can commit in that window; here the live job is excluded by the
// SELECT, so this test pins only the observable contract (only the terminal
// job is deleted and reported, the live job and its rows survive).
func TestDeleteTerminalJobsBefore_SkipsNonTerminal(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			done := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusCompleted})
			live := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusPending})
			got, err := st.DeleteTerminalJobsBefore(t.Context(), time.Now().UTC().Add(time.Hour), true)
			if err != nil {
				t.Fatalf("DeleteTerminalJobsBefore: %v", err)
			}
			if len(got) != 1 || got[0].ID != done.Job.ID {
				t.Fatalf("deleted = %+v, want only the completed job", got)
			}
			mustJob(t, st, live.Job.ID)
			if _, err := st.GetJob(t.Context(), done.Job.ID); !errorsIsNotFound(err) {
				t.Fatalf("GetJob(completed) = %v, want ErrNotFound", err)
			}
		})
	}
}

// TestRetryTasks_ReturnsExactlyTheRevivedRows pins I2 for RetryTasks: the
// returned set is the set of rows the call changed, no more and no fewer.
// Succeeded and still-live tasks are neither revived nor reported, and an ID
// subset narrows both.
func TestRetryTasks_ReturnsExactlyTheRevivedRows(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusFailed},
				stepSpec{name: "a", status: store.StepStatusFailed, tasks: []store.TaskStatus{
					store.TaskStatusFailed, store.TaskStatusCanceled, store.TaskStatusSucceeded, store.TaskStatusReady,
				}})
			ts := g.Tasks["a"]
			failed, canceled, succeeded, ready := ts[0], ts[1], ts[2], ts[3]

			// A subset narrows the set to the one failed task named.
			got, err := st.RetryTasks(t.Context(), g.Job.ID, []string{failed.ID, succeeded.ID}, time.Now().UTC())
			if err != nil {
				t.Fatalf("RetryTasks subset: %v", err)
			}
			if ids := taskIDs(got); !slices.Equal(ids, []string{failed.ID}) {
				t.Fatalf("subset revived %v, want only the failed task %s", ids, failed.ID)
			}
			if mustTask(t, st, canceled.ID).Status != store.TaskStatusCanceled {
				t.Fatal("a task outside the subset must keep its status")
			}

			// The unfiltered call revives what is still failed or canceled.
			got, err = st.RetryTasks(t.Context(), g.Job.ID, nil, time.Now().UTC())
			if err != nil {
				t.Fatalf("RetryTasks all: %v", err)
			}
			if ids := taskIDs(got); !slices.Equal(ids, []string{canceled.ID}) {
				t.Fatalf("second retry revived %v, want only the canceled task %s", ids, canceled.ID)
			}
			if mustTask(t, st, succeeded.ID).Status != store.TaskStatusSucceeded {
				t.Fatal("a succeeded task must never be revived")
			}
			if mustTask(t, st, ready.ID).Status != store.TaskStatusReady {
				t.Fatal("a live task must never be touched")
			}
			if mustJob(t, st, g.Job.ID).Status != store.JobStatusPending {
				t.Fatal("a retry must reset the terminal job to pending")
			}
		})
	}
}

// taskIDs returns the IDs of ts, sorted.
func taskIDs(ts []store.Task) []string {
	ids := make([]string, 0, len(ts))
	for _, t := range ts {
		ids = append(ids, t.ID)
	}
	slices.Sort(ids)
	return ids
}
