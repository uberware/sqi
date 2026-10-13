// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/store"
)

// TestSQLInsertTask_IsUnchanged pins the single-row insert byte for byte: it is
// built from the parts the batched insert shares, and SQLite runs it.
func TestSQLInsertTask_IsUnchanged(t *testing.T) {
	const want = `
INSERT INTO tasks (
	id, job_id, step_id, name, parameters, status,
	assigned_worker_id, assigned_at, created_at, updated_at, required_cores,
	unschedulable_reason, failed_attempts, retry_after, failure_reason)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING 
	id, job_id, step_id, name, parameters, status,
	assigned_worker_id, assigned_at, created_at, updated_at, required_cores,
	unschedulable_reason, failed_attempts, retry_after, failure_reason`
	if sqlInsertTask != want {
		t.Errorf("sqlInsertTask changed:\n got %q\nwant %q", sqlInsertTask, want)
	}
}

// TestTaskInsert_ColumnsPlaceholdersAndArgsAgree pins taskInsertColumns against
// the three things that must have that many entries: the row template, the
// column list and the values bound.
func TestTaskInsert_ColumnsPlaceholdersAndArgsAgree(t *testing.T) {
	if got := strings.Count(sqlTaskRowPlaceholders, "?"); got != taskInsertColumns {
		t.Errorf("row template has %d placeholders, taskInsertColumns = %d", got, taskInsertColumns)
	}
	cols := sqlInsertTasksHead[strings.Index(sqlInsertTasksHead, "(")+1 : strings.LastIndex(sqlInsertTasksHead, ")")]
	if got := len(strings.Split(cols, ",")); got != taskInsertColumns {
		t.Errorf("insert names %d columns, taskInsertColumns = %d", got, taskInsertColumns)
	}
	args, err := taskInsertArgs(store.Task{}, "now")
	if err != nil || len(args) != taskInsertColumns {
		t.Errorf("taskInsertArgs = %d values, %v; want %d", len(args), err, taskInsertColumns)
	}
}

func TestTaskRowsValues(t *testing.T) {
	for rows, want := range map[int]string{
		1: sqlTaskRowPlaceholders,
		3: sqlTaskRowPlaceholders + "," + sqlTaskRowPlaceholders + "," + sqlTaskRowPlaceholders,
	} {
		if got := taskRowsValues(rows); got != want {
			t.Errorf("taskRowsValues(%d) = %q, want %q", rows, got, want)
		}
	}
}

// TestWithTaskInsertBatch_IsCapped pins that a batch larger than a statement
// may bind parameters for is cut down.
func TestWithTaskInsertBatch_IsCapped(t *testing.T) {
	if maxTaskInsertBatch*taskInsertColumns > 32766 {
		t.Fatalf("a full batch binds %d values, over SQLite's 32766", maxTaskInsertBatch*taskInsertColumns)
	}
	var s Store
	WithTaskInsertBatch(1_000_000)(&s)
	if s.taskInsertBatch != maxTaskInsertBatch {
		t.Errorf("taskInsertBatch = %d, want %d", s.taskInsertBatch, maxTaskInsertBatch)
	}
}

func TestInInsertOrder(t *testing.T) {
	tasks := func(ids ...string) []store.Task {
		out := make([]store.Task, len(ids))
		for i, id := range ids {
			out[i] = store.Task{ID: id}
		}
		return out
	}
	ids := func(ts []store.Task) string {
		var b strings.Builder
		for _, task := range ts {
			b.WriteString(task.ID)
		}
		return b.String()
	}
	inserted := tasks("a", "b", "c")

	if got, err := inInsertOrder(tasks("a", "b", "c"), inserted); err != nil || ids(got) != "abc" {
		t.Errorf("in order: got %q, %v; want abc", ids(got), err)
	}
	if got, err := inInsertOrder(tasks("c", "a", "b"), inserted); err != nil || ids(got) != "abc" {
		t.Errorf("out of order: got %q, %v; want abc", ids(got), err)
	}
	if _, err := inInsertOrder(tasks("a", "b", "z"), inserted); err == nil {
		t.Error("a row for a task that was not inserted was accepted")
	}
}

// TestTaskBatchRows pins the statement sizes a submission's tasks are cut
// into: full batches while a batch is left, then the largest power of two
// that fits, so every task is inserted and no statement exceeds the batch.
func TestTaskBatchRows(t *testing.T) {
	for _, c := range []struct{ left, batch, want int }{
		{1201, 500, 500},
		{500, 500, 500},
		{499, 500, 256},
		{256, 500, 256},
		{255, 500, 128},
		{201, 500, 128},
		{1, 500, 1},
		{2, 3, 2},
		{1, 3, 1},
	} {
		if got := taskBatchRows(c.left, c.batch); got != c.want {
			t.Errorf("taskBatchRows(%d, %d) = %d, want %d", c.left, c.batch, got, c.want)
		}
	}
}

// TestTaskBatchRows_FewShapes pins the property the power-of-two remainder
// exists for: across every submission size up to four batches, the statements
// come in at most 1+log2(batch) row counts (10 at 500), so the distinct SQL
// texts a connection prepares stay bounded however the sizes vary.
func TestTaskBatchRows_FewShapes(t *testing.T) {
	const batch = 500
	shapes := map[int]bool{}
	for n := 1; n <= 4*batch; n++ {
		inserted := 0
		for left := n; left > 0; {
			rows := taskBatchRows(left, batch)
			if rows < 1 || rows > left || rows > batch {
				t.Fatalf("n=%d: taskBatchRows(%d, %d) = %d", n, left, batch, rows)
			}
			shapes[rows] = true
			inserted += rows
			left -= rows
		}
		if inserted != n {
			t.Fatalf("n=%d: inserted %d", n, inserted)
		}
	}
	if len(shapes) > 10 {
		t.Errorf("%d distinct statement sizes for submissions of 1 to %d tasks, want at most 10", len(shapes), 4*batch)
	}
}
