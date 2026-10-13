// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres_test

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/postgres"
	"github.com/uberware/sqi/internal/store/sqlstore"
	"github.com/uberware/sqi/internal/store/storetest"
)

// taskInsertCounter counts the task INSERT statements that reach the database,
// and the distinct texts among them.
type taskInsertCounter struct {
	sqlstore.Dialect

	n atomic.Int64

	mu    sync.Mutex
	texts map[string]bool
}

func (d *taskInsertCounter) Rebind(q string) string {
	if strings.HasPrefix(strings.TrimSpace(q), "INSERT INTO tasks") {
		d.n.Add(1)
		d.mu.Lock()
		if d.texts == nil {
			d.texts = map[string]bool{}
		}
		d.texts[q] = true
		d.mu.Unlock()
	}
	return d.Dialect.Rebind(q)
}

// distinct returns how many distinct task INSERT texts d has seen.
func (d *taskInsertCounter) distinct() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.texts)
}

// runsPerTx is how many times the store runs each transaction's body: twice
// under SQI_TEST_PG_FORCE_TX_RETRY=1, where storetest.PostgresWith rehearses
// every transaction once (rolled back) before the run that counts, once
// otherwise. The counter sees the statements of every run and Rebind is not
// told which transaction a statement belongs to, so the tests expect the
// per-run count times this. Each run of CreateJobSubmission, the rehearsal
// included, issues the same statements.
func runsPerTx() int64 {
	if os.Getenv("SQI_TEST_PG_FORCE_TX_RETRY") == "1" {
		return 2
	}
	return 1
}

// batchStore returns a store over a fresh database that counts task INSERTs,
// with the farm and queue "bf" and "bq".
func batchStore(t *testing.T) (*postgres.Store, *taskInsertCounter) {
	t.Helper()
	var count *taskInsertCounter
	opts := postgres.DefaultOptions()
	opts.WrapDialect = func(d sqlstore.Dialect) sqlstore.Dialect {
		count = &taskInsertCounter{Dialect: d}
		return count
	}
	st := storetest.PostgresWith(t, opts)
	if _, err := st.CreateFarm(t.Context(), store.Farm{ID: "bf", Name: "bf"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateQueue(t.Context(), store.Queue{ID: "bq", FarmID: "bf", Name: "bq"}); err != nil {
		t.Fatal(err)
	}
	return st, count
}

func batchSeed(n int) *storetest.JobSeed { return batchSeedNamed("b", n) }

// batchSeedNamed is batchSeed with every ID prefixed by name, so one store can
// take several.
func batchSeedNamed(name string, n int) *storetest.JobSeed {
	seed := storetest.NewJob(name+"j", "bf", "bq").Step(name+"s", store.StepStatusReady)
	for i := range n {
		seed.Task(fmt.Sprintf("%st-%05d", name, i), name+"s", store.TaskStatusReady)
	}
	return seed
}

// TestCreateJobSubmission_InsertsTasksInBatches: 1,201 tasks go in as six
// statements (500 and 500, then the remainder of 201 as 128, 64, 8 and 1
// rows), come back in the order they were given, each stamped strictly later
// than the one before, and all are stored.
func TestCreateJobSubmission_InsertsTasksInBatches(t *testing.T) {
	st, count := batchStore(t)
	const n = 1201

	out, err := st.CreateJobSubmission(t.Context(), batchSeed(n).Sub)
	if err != nil {
		t.Fatalf("CreateJobSubmission: %v", err)
	}
	if got, want := count.n.Load(), 6*runsPerTx(); got != want {
		t.Errorf("%d task INSERT statements for %d tasks over %d run(s), want %d (6 per run)", got, n, runsPerTx(), want)
	}
	if len(out.Tasks) != n {
		t.Fatalf("returned %d tasks, want %d", len(out.Tasks), n)
	}
	for i, task := range out.Tasks {
		if want := fmt.Sprintf("bt-%05d", i); task.ID != want {
			t.Fatalf("returned task %d is %s, want %s", i, task.ID, want)
		}
		if i > 0 && !task.CreatedAt.After(out.Tasks[i-1].CreatedAt) {
			t.Fatalf("task %d created at %s, not after task %d at %s",
				i, task.CreatedAt, i-1, out.Tasks[i-1].CreatedAt)
		}
	}
	counts, err := st.CountTasksByJob(t.Context(), "bj")
	if err != nil || counts[store.TaskStatusReady] != n {
		t.Errorf("stored tasks = %v, %v; want %d ready", counts, err, n)
	}
}

// TestCreateJobSubmission_BatchedFailureRollsBackEntirely: a duplicate task ID
// in the last statement leaves no job, step or task, the statements before it
// included.
func TestCreateJobSubmission_BatchedFailureRollsBackEntirely(t *testing.T) {
	st, count := batchStore(t)
	seed := batchSeed(1201)
	seed.Sub.Tasks[1200].ID = seed.Sub.Tasks[0].ID

	_, err := st.CreateJobSubmission(t.Context(), seed.Sub)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("CreateJobSubmission = %v, want store.ErrConflict", err)
	}
	if got, want := count.n.Load(), 6*runsPerTx(); got != want {
		t.Errorf("%d task INSERT statements over %d run(s), want %d: 6 per run, the duplicate in the last one", got, runsPerTx(), want)
	}
	if _, err := st.GetJob(t.Context(), "bj"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetJob = %v; the job row survived a failed submission", err)
	}
	if steps, err := st.ListSteps(t.Context(), "bj"); err != nil || len(steps) != 0 {
		t.Errorf("ListSteps = %d steps, %v; want none", len(steps), err)
	}
	if counts, err := st.CountTasksByJob(t.Context(), "bj"); err != nil || len(counts) != 0 {
		t.Errorf("CountTasksByJob = %v, %v; want no tasks", counts, err)
	}
}

// TestCreateJobSubmission_BatchedTaskInsertsFewShapes: submissions of a dozen
// odd sizes send task INSERTs of at most ten distinct texts (500 rows, then
// 256 down to 1). pgx prepares every distinct text as a named statement on its
// connection and keeps it, so cutting the remainder as it falls would leave a
// long-lived connection holding one statement per remainder size (twelve
// here, up to 499 over a server's life).
func TestCreateJobSubmission_BatchedTaskInsertsFewShapes(t *testing.T) {
	st, count := batchStore(t)
	for i, n := range []int{1, 3, 5, 13, 31, 63, 77, 255, 333, 499, 999, 1201} {
		if _, err := st.CreateJobSubmission(t.Context(), batchSeedNamed(fmt.Sprintf("s%02d", i), n).Sub); err != nil {
			t.Fatalf("CreateJobSubmission of %d tasks: %v", n, err)
		}
	}
	if got := count.distinct(); got > 10 {
		t.Errorf("%d distinct task INSERT texts, want at most 10", got)
	}
}
