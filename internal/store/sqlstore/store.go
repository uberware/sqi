// SPDX-License-Identifier: AGPL-3.0-or-later

// Package sqlstore is the SQL implementation of [store.Store] shared by every
// database the store runs on. It writes each statement once, with "?"
// placeholders, and runs it through a [Dialect] that rebinds the query,
// converts time and bool arguments, and classifies driver errors. A database
// package (internal/store/sqlite) opens the connection pools, supplies its
// dialect, and builds the store with [New].
//
// Reads and writes run on separate pools: [New] takes a write pool and a read
// pool, and every statement goes to one or the other by its text (see
// poolFor). Transactions always run on the write pool.
package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/uberware/sqi/internal/store"
)

// Compile-time interface compliance check.
var _ store.Store = (*Store)(nil)

// Store is the SQL implementation of [store.Store].
// Create one with [New]; close it with [Store.Close] when done.
type Store struct {
	// d is the SQL dialect every statement goes through.
	d Dialect
	// w is the write pool. Every transaction, and every statement that is not
	// a SELECT, uses it.
	w *dbPool
	// r is the read pool, so a SELECT does not queue behind a long write. Its
	// raw pool may alias w's (an in-memory SQLite database has only one pool).
	r     *dbPool
	stmts []*dbStmt // all prepared statements; closed by Close

	// retries counts transaction re-runs after an ErrClassRetry error.
	retries atomic.Int64
	// forceRetry makes inTx run every function once and roll it back before
	// the real run: a test mode that proves each function is safe to re-run.
	forceRetry bool
	// log receives retry diagnostics; slog.Default() when nil.
	log *slog.Logger

	// ── farms ────────────────────────────────────────────────────────────
	stmtInsertFarm *dbStmt
	stmtGetFarm    *dbStmt
	stmtListFarms  *dbStmt
	stmtUpdateFarm *dbStmt
	stmtDeleteFarm *dbStmt

	// ── queues ───────────────────────────────────────────────────────────
	stmtInsertQueue *dbStmt
	stmtGetQueue    *dbStmt
	stmtUpdateQueue *dbStmt
	stmtDeleteQueue *dbStmt

	// ── storage_locations ────────────────────────────────────────────────
	stmtInsertStorageLoc    *dbStmt
	stmtGetStorageLoc       *dbStmt
	stmtGetStorageLocByName *dbStmt
	stmtListStorageLocs     *dbStmt
	stmtUpdateStorageLoc    *dbStmt
	stmtDeleteStorageLoc    *dbStmt

	// ── compute_locations ────────────────────────────────────────────────────
	stmtInsertComputeLoc    *dbStmt
	stmtGetComputeLoc       *dbStmt
	stmtGetComputeLocByName *dbStmt
	stmtListComputeLocs     *dbStmt
	stmtUpdateComputeLoc    *dbStmt
	stmtDeleteComputeLoc    *dbStmt

	// ── products ─────────────────────────────────────────────────────────────
	stmtInsertProduct    *dbStmt
	stmtGetProductByName *dbStmt
	stmtListProducts     *dbStmt
	stmtUpdateProduct    *dbStmt
	stmtDeleteProduct    *dbStmt

	// ── usage_pools ──────────────────────────────────────────────────────
	stmtInsertPool    *dbStmt
	stmtGetPool       *dbStmt
	stmtListPools     *dbStmt
	stmtListPoolUsage *dbStmt
	stmtUpdatePool    *dbStmt
	stmtDeletePool    *dbStmt

	// ── usage_claims ─────────────────────────────────────────────────────
	stmtActiveClaimCount *dbStmt

	// ── workers ──────────────────────────────────────────────────────────
	stmtUpsertWorker             *dbStmt
	stmtGetWorker                *dbStmt
	stmtSetWorkerDisabled        *dbStmt
	stmtUpdateWorkerHeartbeat    *dbStmt
	stmtListStaleWorkers         *dbStmt
	stmtCountIdleWorkers         *dbStmt
	stmtCountIdleWorkersAllFarms *dbStmt
	stmtDeleteOfflineWorkers     *dbStmt

	// ── jobs ─────────────────────────────────────────────────────────────
	stmtGetJob    *dbStmt
	stmtUpdateJob *dbStmt

	// ── steps ────────────────────────────────────────────────────────────
	stmtGetStep   *dbStmt
	stmtListSteps *dbStmt

	// ── tasks ────────────────────────────────────────────────────────────
	stmtGetTask                      *dbStmt
	stmtSetTaskUnschedulableReason   *dbStmt
	stmtListReadyTasks               *dbStmt
	stmtCountActiveTasksInQueue      *dbStmt
	stmtCountActiveTasksInFarm       *dbStmt
	stmtCountReadyTasksByQueue       *dbStmt
	stmtCountTasksByJob              *dbStmt
	stmtCountUnschedulableTasksByJob *dbStmt

	// ── task_attempts ────────────────────────────────────────────────────
	stmtGetAttempt    *dbStmt
	stmtLatestAttempt *dbStmt
	stmtListAttempts  *dbStmt

	// ── task_logs ────────────────────────────────────────────────────────
	stmtInsertTaskLog *dbStmt
	stmtListTaskLogs  *dbStmt

	// ── audit_log ────────────────────────────────────────────────────────
	stmtInsertAudit *dbStmt

	// ── users ────────────────────────────────────────────────────────────
	stmtInsertUser          *dbStmt
	stmtGetUser             *dbStmt
	stmtGetUserByUsername   *dbStmt
	stmtGetUserByExternalID *dbStmt
	stmtListUsers           *dbStmt
	stmtUpdateUser          *dbStmt
	stmtSetUserPassword     *dbStmt
	stmtSetUserDisplayName  *dbStmt
	stmtCountUsers          *dbStmt

	// ── sessions ─────────────────────────────────────────────────────────
	stmtInsertSession             *dbStmt
	stmtGetSessionByTokenHash     *dbStmt
	stmtGetSessionUserByTokenHash *dbStmt
	stmtDeleteSession             *dbStmt
	stmtDeleteExpiredSessions     *dbStmt

	// ── api keys ─────────────────────────────────────────────────────────
	stmtInsertAPIKey             *dbStmt
	stmtGetAPIKeyUserByTokenHash *dbStmt
	stmtListAPIKeysForUser       *dbStmt
	stmtRevokeAPIKey             *dbStmt
	stmtTouchAPIKeyLastUsed      *dbStmt

	// ── worker credentials & join tokens ────────────────────────────────────
	stmtInsertWorkerCredential      *dbStmt
	stmtListActiveWorkerCredentials *dbStmt
	stmtRevokeWorkerCredential      *dbStmt
	stmtTouchWorkerCredential       *dbStmt
	stmtInsertWorkerJoinToken       *dbStmt
	stmtGetWorkerJoinTokenByHash    *dbStmt
	stmtMarkWorkerJoinTokenUsed     *dbStmt
}

// Option configures [New].
type Option func(*Store)

// WithForcedRetry makes every transaction run its function twice, the first
// run rolled back, as if the database had aborted it for a concurrent writer.
// Tests only: it proves each function keeps no state from a failed run.
func WithForcedRetry() Option { return func(s *Store) { s.forceRetry = true } }

// WithLogger sets the logger retry diagnostics go to.
func WithLogger(l *slog.Logger) Option { return func(s *Store) { s.log = l } }

// TxRetries reports how many times a transaction was run again after the
// database aborted it for a concurrent writer.
func (s *Store) TxRetries() int64 { return s.retries.Load() }

// New builds a Store over open write and read pools for dialect d and
// prepares its statements. read may be write. Close releases the pools; on
// error New closes them itself.
func New(ctx context.Context, write, read *sql.DB, d Dialect, opts ...Option) (*Store, error) {
	s := &Store{
		d: d,
		w: newDBPool(write, d),
		r: newDBPool(read, d),
	}
	for _, o := range opts {
		o(s)
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if err := s.prepareAll(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// Close closes all prepared statements and both connection pools.
// It is safe to call Close more than once.
func (s *Store) Close() error {
	var errs []error
	for _, stmt := range s.stmts {
		if err := stmt.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	// The read pool aliases the write pool for in-memory databases; closing it
	// twice would error.
	if s.r.db != nil && s.r.db != s.w.db {
		if err := s.r.db.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := s.w.db.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// poolFor returns the pool a statement belongs on: the read pool for a SELECT,
// the write pool for everything else.
func (s *Store) poolFor(query string) *sql.DB {
	if isReadStatement(query) {
		return s.r.db
	}
	return s.w.db
}

// isReadStatement reports whether query is a plain SELECT, ignoring leading
// whitespace and SQL comments.
//
// It fails safe: anything it cannot positively identify as a SELECT — a CTE
// (WITH ... may wrap an INSERT or UPDATE just as easily as a SELECT), a
// PRAGMA, an unterminated comment — is treated as a write. Sending a read to
// the write pool only serializes it, which is what every statement did before
// the read pool existed; sending a write to the read pool would put it on a
// multi-connection pool and risk write-write conflicts.
func isReadStatement(query string) bool {
	const sel = "select"
	s := trimSQLLeader(query)
	if len(s) < len(sel) || !strings.EqualFold(s[:len(sel)], sel) {
		return false
	}
	if len(s) == len(sel) {
		return true
	}
	// Reject identifiers that merely start with "select" (SELECTED, ...).
	switch s[len(sel)] {
	case ' ', '\t', '\n', '\r', '\f', '\v', '(':
		return true
	default:
		return false
	}
}

// trimSQLLeader strips leading whitespace and SQL comments from query, so the
// first token of the statement itself is at the front of the result. An
// unterminated block comment consumes the rest of the string, which leaves
// isReadStatement with nothing to match — the safe answer.
func trimSQLLeader(query string) string {
	for {
		query = strings.TrimLeft(query, " \t\n\r\f\v")
		switch {
		case strings.HasPrefix(query, "--"):
			nl := strings.IndexByte(query, '\n')
			if nl < 0 {
				return ""
			}
			query = query[nl+1:]
		case strings.HasPrefix(query, "/*"):
			end := strings.Index(query[2:], "*/")
			if end < 0 {
				return ""
			}
			query = query[2+end+2:]
		default:
			return query
		}
	}
}

// prepare compiles query, rebound for the store's dialect, into a prepared
// statement on the pool poolFor selects from the original text, tracks it for
// Close, and returns it. On failure it returns an error that includes the
// first 60 bytes of the query.
func (s *Store) prepare(ctx context.Context, query string) (*dbStmt, error) {
	stmt, err := s.poolFor(query).PrepareContext(ctx, s.d.Rebind(query))
	if err != nil {
		preview := query
		if len(preview) > 60 {
			preview = preview[:60] + "…"
		}
		return nil, fmt.Errorf("store: prepare %q: %w", preview, err)
	}
	ds := &dbStmt{st: stmt, d: s.d}
	s.stmts = append(s.stmts, ds)
	return ds, nil
}

// prepareAll compiles every SQL statement used by the store. Errors are
// returned immediately; the caller (New) releases resources via Close.
//
//nolint:cyclop,funlen // large but straightforward: one prepare call per query
func (s *Store) prepareAll(ctx context.Context) error {
	var err error

	// ── farms ─────────────────────────────────────────────────────────────
	if s.stmtInsertFarm, err = s.prepare(ctx, sqlInsertFarm); err != nil {
		return err
	}
	if s.stmtGetFarm, err = s.prepare(ctx, sqlGetFarm); err != nil {
		return err
	}
	if s.stmtListFarms, err = s.prepare(ctx, sqlListFarms); err != nil {
		return err
	}
	if s.stmtUpdateFarm, err = s.prepare(ctx, sqlUpdateFarm); err != nil {
		return err
	}
	if s.stmtDeleteFarm, err = s.prepare(ctx, sqlDeleteFarm); err != nil {
		return err
	}

	// ── queues ────────────────────────────────────────────────────────────
	if s.stmtInsertQueue, err = s.prepare(ctx, sqlInsertQueue); err != nil {
		return err
	}
	if s.stmtGetQueue, err = s.prepare(ctx, sqlGetQueue); err != nil {
		return err
	}
	if s.stmtUpdateQueue, err = s.prepare(ctx, sqlUpdateQueue); err != nil {
		return err
	}
	if s.stmtDeleteQueue, err = s.prepare(ctx, sqlDeleteQueue); err != nil {
		return err
	}

	// ── storage_locations ─────────────────────────────────────────────────
	if s.stmtInsertStorageLoc, err = s.prepare(ctx, sqlInsertStorageLoc); err != nil {
		return err
	}
	if s.stmtGetStorageLoc, err = s.prepare(ctx, sqlGetStorageLoc); err != nil {
		return err
	}
	if s.stmtGetStorageLocByName, err = s.prepare(ctx, sqlGetStorageLocByName); err != nil {
		return err
	}
	if s.stmtListStorageLocs, err = s.prepare(ctx, sqlListStorageLocs); err != nil {
		return err
	}
	if s.stmtUpdateStorageLoc, err = s.prepare(ctx, sqlUpdateStorageLoc); err != nil {
		return err
	}
	if s.stmtDeleteStorageLoc, err = s.prepare(ctx, sqlDeleteStorageLoc); err != nil {
		return err
	}

	// ── compute_locations ─────────────────────────────────────────────────────
	if s.stmtInsertComputeLoc, err = s.prepare(ctx, sqlInsertComputeLoc); err != nil {
		return err
	}
	if s.stmtGetComputeLoc, err = s.prepare(ctx, sqlGetComputeLoc); err != nil {
		return err
	}
	if s.stmtGetComputeLocByName, err = s.prepare(ctx, sqlGetComputeLocByName); err != nil {
		return err
	}
	if s.stmtListComputeLocs, err = s.prepare(ctx, sqlListComputeLocs); err != nil {
		return err
	}
	if s.stmtUpdateComputeLoc, err = s.prepare(ctx, sqlUpdateComputeLoc); err != nil {
		return err
	}
	if s.stmtDeleteComputeLoc, err = s.prepare(ctx, sqlDeleteComputeLoc); err != nil {
		return err
	}

	// ── products ──────────────────────────────────────────────────────────
	if s.stmtInsertProduct, err = s.prepare(ctx, sqlInsertProduct); err != nil {
		return err
	}
	if s.stmtGetProductByName, err = s.prepare(ctx, sqlGetProductByName+s.d.CIEqual("name")); err != nil {
		return err
	}
	if s.stmtListProducts, err = s.prepare(ctx, sqlListProducts); err != nil {
		return err
	}
	if s.stmtUpdateProduct, err = s.prepare(ctx, sqlUpdateProductHead+s.d.CIEqual("name")+sqlUpdateProductTail); err != nil {
		return err
	}
	if s.stmtDeleteProduct, err = s.prepare(ctx, sqlDeleteProduct+s.d.CIEqual("name")); err != nil {
		return err
	}

	// ── usage_pools ───────────────────────────────────────────────────────
	if s.stmtInsertPool, err = s.prepare(ctx, sqlInsertPool); err != nil {
		return err
	}
	if s.stmtGetPool, err = s.prepare(ctx, sqlGetPool); err != nil {
		return err
	}
	if s.stmtListPools, err = s.prepare(ctx, sqlListPools); err != nil {
		return err
	}
	if s.stmtListPoolUsage, err = s.prepare(ctx, sqlListPoolUsage); err != nil {
		return err
	}
	if s.stmtUpdatePool, err = s.prepare(ctx, sqlUpdatePool); err != nil {
		return err
	}
	if s.stmtDeletePool, err = s.prepare(ctx, sqlDeletePool); err != nil {
		return err
	}

	// ── usage_claims ──────────────────────────────────────────────────────
	if s.stmtActiveClaimCount, err = s.prepare(ctx, sqlActiveClaimCount); err != nil {
		return err
	}

	// ── workers ───────────────────────────────────────────────────────────
	if s.stmtUpsertWorker, err = s.prepare(ctx, sqlUpsertWorker); err != nil {
		return err
	}
	if s.stmtGetWorker, err = s.prepare(ctx, sqlGetWorker); err != nil {
		return err
	}
	if s.stmtSetWorkerDisabled, err = s.prepare(ctx, sqlSetWorkerDisabled); err != nil {
		return err
	}
	if s.stmtUpdateWorkerHeartbeat, err = s.prepare(ctx, sqlUpdateWorkerHeartbeat); err != nil {
		return err
	}
	if s.stmtListStaleWorkers, err = s.prepare(ctx, sqlListStaleWorkers); err != nil {
		return err
	}
	if s.stmtCountIdleWorkers, err = s.prepare(ctx, sqlCountIdleWorkers); err != nil {
		return err
	}
	if s.stmtCountIdleWorkersAllFarms, err = s.prepare(ctx, sqlCountIdleWorkersAllFarms); err != nil {
		return err
	}
	if s.stmtDeleteOfflineWorkers, err = s.prepare(ctx, sqlDeleteOfflineWorkersBefore); err != nil {
		return err
	}

	// ── jobs ──────────────────────────────────────────────────────────────
	if s.stmtGetJob, err = s.prepare(ctx, sqlGetJob); err != nil {
		return err
	}
	if s.stmtUpdateJob, err = s.prepare(ctx, sqlUpdateJob); err != nil {
		return err
	}

	// ── steps ─────────────────────────────────────────────────────────────
	if s.stmtGetStep, err = s.prepare(ctx, sqlGetStep); err != nil {
		return err
	}
	if s.stmtListSteps, err = s.prepare(ctx, sqlListSteps); err != nil {
		return err
	}

	// ── tasks ─────────────────────────────────────────────────────────────
	if s.stmtGetTask, err = s.prepare(ctx, sqlGetTask); err != nil {
		return err
	}
	if s.stmtSetTaskUnschedulableReason, err = s.prepare(ctx, sqlSetTaskUnschedulableReason); err != nil {
		return err
	}
	if s.stmtListReadyTasks, err = s.prepare(ctx, sqlListReadyTasks); err != nil {
		return err
	}
	if s.stmtCountActiveTasksInQueue, err = s.prepare(ctx, sqlCountActiveTasksInQueue); err != nil {
		return err
	}
	if s.stmtCountActiveTasksInFarm, err = s.prepare(ctx, sqlCountActiveTasksInFarm); err != nil {
		return err
	}
	if s.stmtCountReadyTasksByQueue, err = s.prepare(ctx, sqlCountReadyTasksByQueue); err != nil {
		return err
	}
	if s.stmtCountTasksByJob, err = s.prepare(ctx, sqlCountTasksByJob); err != nil {
		return err
	}
	if s.stmtCountUnschedulableTasksByJob, err = s.prepare(ctx, sqlCountUnschedulableTasksByJob); err != nil {
		return err
	}

	// ── task_attempts ─────────────────────────────────────────────────────
	if s.stmtGetAttempt, err = s.prepare(ctx, sqlGetAttempt); err != nil {
		return err
	}
	if s.stmtLatestAttempt, err = s.prepare(ctx, sqlLatestAttempt); err != nil {
		return err
	}
	if s.stmtListAttempts, err = s.prepare(ctx, sqlListAttempts); err != nil {
		return err
	}

	// ── task_logs ─────────────────────────────────────────────────────────
	if s.stmtInsertTaskLog, err = s.prepare(ctx, sqlInsertTaskLog); err != nil {
		return err
	}
	if s.stmtListTaskLogs, err = s.prepare(ctx, sqlListTaskLogs); err != nil {
		return err
	}

	// ── audit_log ─────────────────────────────────────────────────────────
	if s.stmtInsertAudit, err = s.prepare(ctx, sqlInsertAudit); err != nil {
		return err
	}

	// ── users ─────────────────────────────────────────────────────────────
	if s.stmtInsertUser, err = s.prepare(ctx, sqlInsertUser); err != nil {
		return err
	}
	if s.stmtGetUser, err = s.prepare(ctx, sqlGetUser); err != nil {
		return err
	}
	if s.stmtGetUserByUsername, err = s.prepare(ctx, sqlGetUserByUsername+s.d.CIEqual("username")); err != nil {
		return err
	}
	if s.stmtGetUserByExternalID, err = s.prepare(ctx, sqlGetUserByExternalID); err != nil {
		return err
	}
	if s.stmtListUsers, err = s.prepare(ctx, sqlListUsers); err != nil {
		return err
	}
	if s.stmtUpdateUser, err = s.prepare(ctx, sqlUpdateUser); err != nil {
		return err
	}
	if s.stmtSetUserPassword, err = s.prepare(ctx, sqlSetUserPassword); err != nil {
		return err
	}
	if s.stmtSetUserDisplayName, err = s.prepare(ctx, sqlSetUserDisplayName); err != nil {
		return err
	}
	if s.stmtCountUsers, err = s.prepare(ctx, sqlCountUsers); err != nil {
		return err
	}

	// ── sessions ──────────────────────────────────────────────────────────
	if s.stmtInsertSession, err = s.prepare(ctx, sqlInsertSession); err != nil {
		return err
	}
	if s.stmtGetSessionByTokenHash, err = s.prepare(ctx, sqlGetSessionByTokenHash); err != nil {
		return err
	}
	if s.stmtGetSessionUserByTokenHash, err = s.prepare(ctx, sqlGetSessionUserByTokenHash); err != nil {
		return err
	}
	if s.stmtDeleteSession, err = s.prepare(ctx, sqlDeleteSession); err != nil {
		return err
	}
	if s.stmtDeleteExpiredSessions, err = s.prepare(ctx, sqlDeleteExpiredSessions); err != nil {
		return err
	}

	// ── api keys ──────────────────────────────────────────────────────────
	if s.stmtInsertAPIKey, err = s.prepare(ctx, sqlInsertAPIKey); err != nil {
		return err
	}
	if s.stmtGetAPIKeyUserByTokenHash, err = s.prepare(ctx, sqlGetAPIKeyUserByTokenHash); err != nil {
		return err
	}
	if s.stmtListAPIKeysForUser, err = s.prepare(ctx, sqlListAPIKeysForUser); err != nil {
		return err
	}
	if s.stmtRevokeAPIKey, err = s.prepare(ctx, sqlRevokeAPIKey); err != nil {
		return err
	}
	if s.stmtTouchAPIKeyLastUsed, err = s.prepare(ctx, sqlTouchAPIKeyLastUsed); err != nil {
		return err
	}

	// ── worker credentials & join tokens ─────────────────────────────────────
	if s.stmtInsertWorkerCredential, err = s.prepare(ctx, sqlInsertWorkerCredential); err != nil {
		return err
	}
	if s.stmtListActiveWorkerCredentials, err = s.prepare(ctx, sqlListActiveWorkerCredentials); err != nil {
		return err
	}
	if s.stmtRevokeWorkerCredential, err = s.prepare(ctx, sqlRevokeWorkerCredential); err != nil {
		return err
	}
	if s.stmtTouchWorkerCredential, err = s.prepare(ctx, sqlTouchWorkerCredential); err != nil {
		return err
	}
	if s.stmtInsertWorkerJoinToken, err = s.prepare(ctx, sqlInsertWorkerJoinToken); err != nil {
		return err
	}
	if s.stmtGetWorkerJoinTokenByHash, err = s.prepare(ctx, sqlGetWorkerJoinTokenByHash); err != nil {
		return err
	}
	if s.stmtMarkWorkerJoinTokenUsed, err = s.prepare(ctx, sqlMarkWorkerJoinTokenUsed); err != nil {
		return err
	}
	// sqlConsumeWorkerJoinToken has no prepared statement of its own: it is
	// used only as raw SQL text inside RedeemWorkerJoinToken's transaction
	// (workercredential.go), matching CreateJobSubmission's idiom.

	return nil
}
