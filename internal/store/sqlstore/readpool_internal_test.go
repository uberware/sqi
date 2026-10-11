// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

// White-box tests for statement routing between the write and read pools. The
// pools themselves are opened by the database package (see
// internal/store/sqlite's readpool_test.go); what is tested here is which pool
// a statement lands on, and that is unexported.

import (
	"database/sql"
	"testing"
)

// ── statement classification ─────────────────────────────────────────────────

func TestIsReadStatement(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  bool
	}{
		{"plain select", `SELECT 1`, true},
		{"lowercase select", `select 1`, true},
		{"mixed case select", `SeLeCt 1`, true},
		{"leading whitespace and newline", "\n\t  SELECT id FROM jobs", true},
		{"select with paren", `SELECT(1)`, true},
		{"line comment then select", "-- a comment\nSELECT 1", true},
		{"two line comments then select", "-- one\n-- two\nSELECT 1", true},
		{"block comment then select", "/* a comment */ SELECT 1", true},
		{"insert", `INSERT INTO jobs (id) VALUES (?)`, false},
		{"insert returning", `INSERT INTO jobs (id) VALUES (?) RETURNING id`, false},
		{"update returning", `UPDATE jobs SET x = ? WHERE id = ? RETURNING id`, false},
		{"delete returning", `DELETE FROM workers WHERE id = ? RETURNING id`, false},
		{"pragma", `PRAGMA wal_checkpoint(TRUNCATE)`, false},
		{"vacuum into", `VACUUM INTO ?`, false},
		{"cte fails safe to the write pool", `WITH x AS (SELECT 1) SELECT * FROM x`, false},
		{"word starting with select", `SELECTED FROM x`, false},
		{"empty", ``, false},
		{"only a comment", "-- nothing here\n", false},
		{"unterminated block comment", "/* never closed SELECT 1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isReadStatement(tt.query); got != tt.want {
				t.Errorf("isReadStatement(%q) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

// TestPoolFor_RoutesEveryPreparedStatement pins the routing of the actual SQL
// this package prepares. Writes that read like reads (INSERT/UPDATE/DELETE ...
// RETURNING, all issued through QueryRowContext) are the trap this guards: they
// must stay on the single-connection write pool.
//
// poolFor only compares pool identities, so two distinct, never-opened pools
// stand in for a file-backed store's write and read pools.
func TestPoolFor_RoutesEveryPreparedStatement(t *testing.T) {
	s := &Store{w: &dbPool{db: new(sql.DB)}, r: &dbPool{db: new(sql.DB)}}

	reads := map[string]string{
		"sqlGetJob":                       sqlGetJob,
		"sqlListBlockedJobs":              sqlListBlockedJobs,
		"sqlListReadyTasks":               sqlListReadyTasks,
		"sqlCountTasksByJob":              sqlCountTasksByJob,
		"sqlCountUnschedulableTasksByJob": sqlCountUnschedulableTasksByJob,
		"sqlCommittedCores":               sqlCommittedCores,
		"sqlActiveClaimCount":             sqlActiveClaimCount,
		"sqlListPoolUsage":                sqlListPoolUsage,
		"sqlGetSessionUserByTokenHash":    sqlGetSessionUserByTokenHash,
		"sqlGetAPIKeyUserByTokenHash":     sqlGetAPIKeyUserByTokenHash,
		"sqlListTaskLogs":                 sqlListTaskLogs,
		"sqlListAuditAll":                 sqlListAuditAll,
		"sqlSelectTaskStatus":             sqlSelectTaskStatus,
		"sqlFailureReasonSummary":         sqlFailureReasonSummary,
		"sqlListJobDependencyIDs":         sqlListJobDependencyIDs,
	}
	writes := map[string]string{
		"sqlInsertJob":                  sqlInsertJob,
		"sqlInsertTask":                 sqlInsertTask,
		"sqlCASTaskStatus":              sqlCASTaskStatus,
		"sqlDeleteJobRow":               sqlDeleteJobRow,
		"sqlDeleteOfflineWorkersBefore": sqlDeleteOfflineWorkersBefore, // DELETE ... RETURNING
		"sqlUpdateUser":                 sqlUpdateUser,                 // UPDATE ... RETURNING
		"sqlSetUserDisplayName":         sqlSetUserDisplayName,         // UPDATE ... RETURNING
		"sqlIncrementJobFailure":        sqlIncrementJobFailure,        // UPDATE ... RETURNING
		"sqlRecordTaskFailure":          sqlRecordTaskFailure,          // UPDATE ... RETURNING
		"sqlUpsertWorker":               sqlUpsertWorker,               // INSERT ... RETURNING
		"sqlInsertAPIKey":               sqlInsertAPIKey,               // INSERT ... RETURNING
		"sqlUpdateComputeLoc":           sqlUpdateComputeLoc,           // UPDATE ... RETURNING
		"sqlDemoteStalledJobs":          sqlDemoteStalledJobs,          // UPDATE ... RETURNING
		"sqlTransitionStepPendingTasks": sqlTransitionStepPendingTasks, // UPDATE ... RETURNING
		"sqlLeaseTaskGuarded":           sqlLeaseTaskGuarded,
		"sqlRetryTasksPrefix":           sqlRetryTasksPrefix,
	}

	for name, query := range reads {
		if s.poolFor(query) != s.r.db {
			t.Errorf("%s routed to the write pool, want the read pool", name)
		}
	}
	for name, query := range writes {
		if s.poolFor(query) != s.w.db {
			t.Errorf("%s routed to the READ pool; a write on a multi-connection pool risks write-write conflicts", name)
		}
	}
}
