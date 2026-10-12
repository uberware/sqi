// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore_test

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlstore"
	"github.com/uberware/sqi/internal/store/storetest"
)

// batchTasksSeed is a job "bj" with one step and n tasks that differ in every
// column a task insert binds, so a value bound to the wrong column of a
// multi-row insert shows.
func batchTasksSeed(n int) store.JobSubmission {
	seed := storetest.NewJob("bj", "bj-farm", "bj-queue").Step("bs", store.StepStatusReady)
	for i := range n {
		task := store.Task{
			ID: fmt.Sprintf("bt-%02d", i), StepID: "bs", Status: store.TaskStatusReady,
			Name:       fmt.Sprintf("frame %d", i),
			Parameters: map[string]string{"Frame": strconv.Itoa(i), "Layer": "beauty"},
		}
		if i%3 == 0 {
			cores := i + 1
			task.RequiredCores = &cores
		}
		if i%4 == 1 {
			task.UnschedulableReason = fmt.Sprintf("no worker has %d cores", i)
		}
		if i%5 == 2 {
			retry := time.Date(2030, 1, 2, 3, 4, 5, i, time.UTC)
			task.RetryAfter = &retry
			task.FailedAttempts = i
		}
		seed.TaskRow(task)
	}
	return seed.Sub
}

// storeWithBatch opens a SQLite database seeded with bj's farm and queue and
// returns a store over it that inserts rows tasks per statement, or one per
// statement when rows is 0.
func storeWithBatch(t *testing.T, rows int) (*sqlstore.Store, *recordingDialect) {
	t.Helper()
	base := openTestStoreWB(t)
	seedScope(t, base, "bj")
	d := &recordingDialect{Dialect: base.Dialect()}
	opts := []sqlstore.Option{}
	if rows > 0 {
		opts = append(opts, sqlstore.WithTaskInsertBatch(rows))
	}
	return withDialect(t, base, d, opts...), d
}

// taskInserts counts the task INSERT statements d has seen.
func taskInserts(d *recordingDialect) int {
	n := 0
	for _, q := range d.queries {
		if strings.HasPrefix(strings.TrimSpace(q), "INSERT INTO tasks") {
			n++
		}
	}
	return n
}

// TestCreateJobSubmission_BatchedTasksMatchSingleRow pins that inserting tasks
// several to a statement stores and returns exactly what one per statement
// does: the same rows in the same order, stamped strictly later one after the
// other across the statement boundaries. SQLite carries the check because it
// shares the SQL layer; PostgreSQL is the store that batches in production.
func TestCreateJobSubmission_BatchedTasksMatchSingleRow(t *testing.T) {
	for _, n := range []int{0, 1, 3, 4, 7, 12} {
		t.Run(fmt.Sprintf("%d tasks", n), func(t *testing.T) {
			single, singleStmts := storeWithBatch(t, 0)
			batched, batchedStmts := storeWithBatch(t, 3)
			sub := batchTasksSeed(n)

			want, err := single.CreateJobSubmission(t.Context(), sub)
			if err != nil {
				t.Fatalf("one row per statement: %v", err)
			}
			got, err := batched.CreateJobSubmission(t.Context(), sub)
			if err != nil {
				t.Fatalf("batched: %v", err)
			}
			// Not vacuous: the batched store really sent n tasks three to a
			// statement, and the other one a statement each.
			// Three to a statement leaves a remainder of 1 or 2, each a power
			// of two and so one statement (taskBatchRows).
			if got, want := taskInserts(batchedStmts), (n+2)/3; got != want {
				t.Errorf("batched store sent %d task INSERTs, want %d", got, want)
			}
			if got := taskInserts(singleStmts); got != n {
				t.Errorf("single-row store sent %d task INSERTs, want %d", got, n)
			}
			if len(got.Tasks) != n {
				t.Fatalf("batched insert returned %d tasks, want %d", len(got.Tasks), n)
			}
			for i, task := range got.Tasks {
				if i > 0 && !task.CreatedAt.After(got.Tasks[i-1].CreatedAt) {
					t.Errorf("task %d created at %s, not after task %d at %s",
						i, task.CreatedAt, i-1, got.Tasks[i-1].CreatedAt)
				}
				got.Tasks[i].CreatedAt, got.Tasks[i].UpdatedAt = time.Time{}, time.Time{}
				want.Tasks[i].CreatedAt, want.Tasks[i].UpdatedAt = time.Time{}, time.Time{}
			}
			if !reflect.DeepEqual(got.Tasks, want.Tasks) {
				t.Errorf("batched tasks differ from one-per-statement tasks:\n got %+v\nwant %+v", got.Tasks, want.Tasks)
			}

			listed, err := batched.ListTasks(t.Context(), store.ListTasksOptions{JobID: "bj"})
			if err != nil || len(listed.Items) != n {
				t.Errorf("ListTasks = %d tasks, %v; want %d, nil", len(listed.Items), err, n)
			}
		})
	}
}

// TestCreateJobSubmission_BatchedTasksRollBackEntirely pins that a duplicate
// task ID in a later statement undoes the statements before it, the job and
// the steps, as the one-per-statement insert does.
func TestCreateJobSubmission_BatchedTasksRollBackEntirely(t *testing.T) {
	batched, _ := storeWithBatch(t, 3)
	sub := batchTasksSeed(8) // statements of 3, 3 and 2 tasks
	sub.Tasks[7].ID = sub.Tasks[0].ID

	_, err := batched.CreateJobSubmission(t.Context(), sub)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("CreateJobSubmission = %v, want store.ErrConflict", err)
	}
	if _, err := batched.GetJob(t.Context(), "bj"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetJob = %v; the job row survived a failed submission", err)
	}
	if steps, err := batched.ListSteps(t.Context(), "bj"); err != nil || len(steps) != 0 {
		t.Errorf("ListSteps = %d steps, %v; want none", len(steps), err)
	}
	if tasks, err := batched.ListTasks(t.Context(), store.ListTasksOptions{JobID: "bj"}); err != nil || len(tasks.Items) != 0 {
		t.Errorf("ListTasks = %d tasks, %v; want none", len(tasks.Items), err)
	}
}

// TestCreateJobSubmission_BatchedTaskInsertsFewShapes pins that submissions of
// every size from 1 to 15 tasks, eight to a statement, send task INSERTs of at
// most four distinct texts (8, 4, 2 and 1 rows), where cutting the remainder
// as it falls would send eight. Each distinct text is a statement a PostgreSQL
// connection prepares and keeps.
func TestCreateJobSubmission_BatchedTaskInsertsFewShapes(t *testing.T) {
	texts := map[string]bool{}
	for n := 1; n <= 15; n++ {
		st, d := storeWithBatch(t, 8)
		if _, err := st.CreateJobSubmission(t.Context(), batchTasksSeed(n)); err != nil {
			t.Fatalf("%d tasks: %v", n, err)
		}
		for _, q := range d.queries {
			if strings.HasPrefix(strings.TrimSpace(q), "INSERT INTO tasks") {
				texts[q] = true
			}
		}
	}
	if len(texts) > 4 {
		t.Errorf("%d distinct task INSERT texts across submissions of 1 to 15 tasks, want at most 4", len(texts))
	}
}

// TestCreateJobSubmission_DoomedSubmissionInsertsNoStepsOrTasks pins that a
// submission naming a failed upstream is refused before it inserts a step or a
// task: an unlocked read of the upstreams runs before the inserts, and only
// the locked re-check runs after them.
func TestCreateJobSubmission_DoomedSubmissionInsertsNoStepsOrTasks(t *testing.T) {
	st, d := storeWithBatch(t, 0)
	up := jobFor("bj", store.JobStatusFailed)
	up.ID = "bj-up"
	if _, err := st.CreateJobSubmission(t.Context(), store.JobSubmission{Job: up}); err != nil {
		t.Fatalf("seed upstream: %v", err)
	}
	d.queries = nil

	sub := batchTasksSeed(4)
	sub.Job.Status = store.JobStatusBlocked
	sub.DependsOn = []string{up.ID}
	_, err := st.CreateJobSubmission(t.Context(), sub)
	if !errors.Is(err, store.ErrDependencyUnsatisfiable) {
		t.Fatalf("CreateJobSubmission = %v, want ErrDependencyUnsatisfiable", err)
	}
	for _, q := range d.queries {
		if q := strings.TrimSpace(q); strings.HasPrefix(q, "INSERT INTO steps") || strings.HasPrefix(q, "INSERT INTO tasks") {
			t.Errorf("a doomed submission ran %.40q before failing", q)
		}
	}
}
