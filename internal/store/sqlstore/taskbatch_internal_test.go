// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/store"
)

// TestSQLInsertTask_IsUnchanged pins the single-row insert byte for byte: it is
// built from the parts the batched insert shares, and SQLite still runs it.
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
