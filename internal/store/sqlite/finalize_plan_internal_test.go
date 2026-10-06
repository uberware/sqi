// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"database/sql"
	"slices"
	"strings"
	"testing"
)

// queryPlan returns the detail column of query's EXPLAIN QUERY PLAN, one entry
// per plan row, in plan order.
func queryPlan(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	return out
}

// TestStepFinalizationQueries_UseStepStatusIndex pins migration 00032: the
// "is any task of this step still in flight" subquery of sqlFinalizeStep (run on
// the single write connection for every terminal report) and of
// sqlListStuckSteps (startup) is answered from the tasks(step_id, status) index
// alone, and sqlFinalizeStep's failed and canceled checks are equality searches
// on it. Without the index the in-flight check reads the step's task rows one by
// one through tasks(step_id), up to the first one in flight. No task access in
// either statement may read a table row or scan the whole index.
func TestStepFinalizationQueries_UseStepStatusIndex(t *testing.T) {
	cases := []struct {
		name  string
		query string
		args  []any
		want  []string // plan rows the tasks subqueries must produce
	}{
		{
			name: "sqlFinalizeStep", query: sqlFinalizeStep, args: []any{"2026-01-01T00:00:00Z", "step"},
			want: []string{
				"SEARCH t USING COVERING INDEX tasks_step_status (step_id=?)",              // still in flight?
				"SEARCH t USING COVERING INDEX tasks_step_status (step_id=? AND status=?)", // any failed / canceled?
			},
		},
		{
			name: "sqlListStuckSteps", query: sqlListStuckSteps,
			want: []string{
				"SEARCH t USING COVERING INDEX tasks_step_status (step_id=?)", // still in flight?
			},
		},
	}
	s := openTestStoreWB(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := queryPlan(t, s.db, tc.query, tc.args...)
			shown := strings.Join(plan, "\n  ")
			for _, want := range tc.want {
				if !slices.Contains(plan, want) {
					t.Errorf("plan has no %q\nplan:\n  %s", want, shown)
				}
			}
			for _, row := range plan {
				if strings.HasPrefix(row, "SCAN t") ||
					(strings.HasPrefix(row, "SEARCH t ") && !strings.Contains(row, "COVERING INDEX")) {
					t.Errorf("plan row %q reads task rows or scans every task\nplan:\n  %s", row, shown)
				}
			}
		})
	}
}
