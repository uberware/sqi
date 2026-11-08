// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/store/sqlstore"
)

// TestTimingQueries_UseStepIndexes pins the cost of the remaining-time read on
// SQLite: tasks are reached through their steps, the per-step counts and
// measured sums come from the tasks(step_id, status, run_ms) index alone, and
// the running tasks are an equality search on it. Neither statement may scan
// every task or every step, or read a task row for the counts; a large job's
// read then costs one index entry per task, with no attempt lookups.
func TestTimingQueries_UseStepIndexes(t *testing.T) {
	db := openTestStoreWB(t).ReadDB()
	for _, byQueues := range []bool{false, true} {
		steps, running, args := sqlstore.TimingQueries(byQueues, []string{"id-1", "id-2"})
		for _, tc := range []struct {
			name, query, want string
		}{
			{"steps", steps, "SEARCH t USING COVERING INDEX tasks_step_status (step_id=?)"},
			{"running", running, "SEARCH t USING INDEX tasks_step_status (step_id=? AND status=?)"},
		} {
			name := tc.name
			if byQueues {
				name += " by queue"
			}
			t.Run(name, func(t *testing.T) {
				plan := queryPlan(t, db, tc.query, args...)
				shown := strings.Join(plan, "\n  ")
				if !slices.Contains(plan, tc.want) {
					t.Errorf("plan has no %q\nplan:\n  %s", tc.want, shown)
				}
				for _, row := range plan {
					if strings.HasPrefix(row, "SCAN t") || strings.HasPrefix(row, "SCAN s") {
						t.Errorf("plan row %q scans every task or step\nplan:\n  %s", row, shown)
					}
				}
			})
		}
	}
}
