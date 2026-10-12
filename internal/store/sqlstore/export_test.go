// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

import (
	"context"
	"database/sql"
)

// Exported for the tests in package sqlstore_test, which open SQLite stores
// through internal/store/sqlite (importing it from package sqlstore would be
// an import cycle).

// Aliases for the unexported types the tests name.
type (
	DBTx      = dbTx
	DBPool    = dbPool
	DBBool    = dbBool
	TxOutcome = txOutcome
	CasResult = casResult
)

const (
	TxCommit      = txCommit
	TxRollback    = txRollback
	MaxTxAttempts = maxTxAttempts

	CasApplied  = casApplied
	CasSame     = casSame
	CasRejected = casRejected

	SQLExpiredJobByID = sqlExpiredJobByID
	SQLFinalizeStep   = sqlFinalizeStep
	SQLListStuckSteps = sqlListStuckSteps

	SQLCancelJobTasks           = sqlCancelJobTasks
	SQLNextAttemptNumber        = sqlNextAttemptNumber
	SQLWorkerInFlightJobIDs     = sqlWorkerInFlightJobIDs
	SQLSelectTaskStatus         = sqlSelectTaskStatus
	SQLIsLatestAttempt          = sqlIsLatestAttempt
	SQLCloseTaskRunningAttempts = sqlCloseTaskRunningAttempts
	SQLInsertClaim              = sqlInsertClaim
	SQLDeleteJobAttempts        = sqlDeleteJobAttempts
	SQLRetryTasksPrefix         = sqlRetryTasksPrefix
	SQLDeleteUserKeepingAdmin   = sqlDeleteUserKeepingAdmin
	SQLFinalizeJob              = sqlFinalizeJob
	SQLReclaimTaskSet           = sqlReclaimTaskSet
)

var (
	MapErr              = mapErr
	ClassifyErr         = classifyErr
	TextToTime          = textToTime
	CasWriteTaskStatus  = casWriteTaskStatus
	CasTaskStatusTx     = casTaskStatusTx
	ExpiredJobsQuery    = expiredJobsQuery
	SelectExpiredJobsTx = selectExpiredJobsTx
	PurgeExpiredJobTx   = purgeExpiredJobTx
)

// WriteDB and ReadDB return the store's write and read pools.
func (s *Store) WriteDB() *sql.DB { return s.w.db }
func (s *Store) ReadDB() *sql.DB  { return s.r.db }

// Dialect returns the dialect the store runs its statements through.
func (s *Store) Dialect() Dialect { return s.d }

// WritePool and ReadPool return the dialect-wrapped write and read pools.
func (s *Store) WritePool() *DBPool { return s.w }
func (s *Store) ReadPool() *DBPool  { return s.r }

// WrapTx returns tx wrapped in the store's dialect.
func (s *Store) WrapTx(tx *sql.Tx) *DBTx { return newDBTx(tx, s.d) }

// InTx runs fn in a transaction, as the store's operations do.
func (s *Store) InTx(ctx context.Context, op string, fn func(tx *DBTx) (TxOutcome, error)) error {
	return s.inTx(ctx, op, fn)
}

// SearchClause is searchClause, for the tests that pin its output.
var SearchClause = searchClause

// StmtCount is the number of statements the store prepared.
func (s *Store) StmtCount() int { return len(s.stmts) }
