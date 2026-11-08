// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
)

// The QueueJobTimings benchmark fixture: one queue of benchTimingJobs
// unfinished jobs of benchTimingTasks tasks each, half of every job succeeded
// through real attempts and benchTimingRunning running.
const (
	benchTimingJobs    = 200
	benchTimingTasks   = 100
	benchTimingRunning = 2
)

// BenchmarkQueueJobTimings measures one QueueJobTimings call on SQLite over
// the fixture above.
func BenchmarkQueueJobTimings(b *testing.B) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, b.TempDir()+"/test.db", sqlite.DefaultOptions())
	if err != nil {
		b.Fatalf("sqlite.Open: %v", err)
	}
	b.Cleanup(func() { _ = st.Close() })

	statuses := make([]store.TaskStatus, benchTimingTasks)
	for i := range statuses {
		switch {
		case i < benchTimingTasks/2:
			statuses[i] = store.TaskStatusRunning // completed below
		case i < benchTimingTasks/2+benchTimingRunning:
			statuses[i] = store.TaskStatusRunning
		default:
			statuses[i] = store.TaskStatusReady
		}
	}
	spec := stepSpec{name: "a", status: store.StepStatusReady, tasks: statuses}
	first := seedGraph(b, st, graphOpts{jobStatus: store.JobStatusRunning}, spec)
	graphs := []jobGraph{first}
	for len(graphs) < benchTimingJobs {
		graphs = append(graphs, seedGraph(b, st, graphOpts{jobStatus: store.JobStatusRunning, share: &first}, spec))
	}
	end := time.Now().UTC()
	for _, g := range graphs {
		for _, task := range g.Tasks["a"][:benchTimingTasks/2] {
			a := g.Attempts[task.ID]
			res, err := st.CompleteTaskAttempt(ctx, store.AttemptCompletion{
				AttemptID: a.ID, TaskID: task.ID, TaskStatus: store.TaskStatusSucceeded,
				AttemptStatus: store.AttemptStatusSucceeded, EndedAt: end,
			})
			if err != nil || !res.Applied {
				b.Fatalf("CompleteTaskAttempt = (%+v, %v)", res, err)
			}
		}
	}

	queues := []string{first.Queue.ID}
	b.ResetTimer()
	for range b.N {
		got, err := st.QueueJobTimings(ctx, queues)
		if err != nil || len(got) != benchTimingJobs {
			b.Fatalf("QueueJobTimings = (%d jobs, %v), want %d", len(got), err, benchTimingJobs)
		}
	}
}

// The JobTimings benchmark fixture: one unfinished job of benchLargeJobTasks
// tasks, half of them succeeded through real attempts and benchTimingRunning
// running. It is the case the per-job read is slowest on: the cost follows
// the job's succeeded tasks, and GET /jobs/{id} reads it on every call.
const benchLargeJobTasks = 100_000

// BenchmarkJobTimingsLargeJob measures one JobTimings call on SQLite for the
// one large job above.
func BenchmarkJobTimingsLargeJob(b *testing.B) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, b.TempDir()+"/test.db", sqlite.DefaultOptions())
	if err != nil {
		b.Fatalf("sqlite.Open: %v", err)
	}
	b.Cleanup(func() { _ = st.Close() })

	succeeded := benchLargeJobTasks / 2
	statuses := make([]store.TaskStatus, benchLargeJobTasks)
	for i := range statuses {
		switch {
		case i < succeeded+benchTimingRunning:
			statuses[i] = store.TaskStatusRunning // the first `succeeded` complete below
		default:
			statuses[i] = store.TaskStatusReady
		}
	}
	g := seedGraph(b, st, graphOpts{jobStatus: store.JobStatusRunning},
		stepSpec{name: "a", status: store.StepStatusReady, tasks: statuses})
	end := time.Now().UTC()
	for _, task := range g.Tasks["a"][:succeeded] {
		a := g.Attempts[task.ID]
		res, err := st.CompleteTaskAttempt(ctx, store.AttemptCompletion{
			AttemptID: a.ID, TaskID: task.ID, TaskStatus: store.TaskStatusSucceeded,
			AttemptStatus: store.AttemptStatusSucceeded, EndedAt: end,
		})
		if err != nil || !res.Applied {
			b.Fatalf("CompleteTaskAttempt = (%+v, %v)", res, err)
		}
	}

	ids := []string{g.Job.ID}
	for b.Loop() {
		got, err := st.JobTimings(ctx, ids)
		if err != nil || len(got) != 1 {
			b.Fatalf("JobTimings = (%d jobs, %v), want 1", len(got), err)
		}
	}
}
