// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

import (
	"context"
	"database/sql"
	"time"

	"github.com/uberware/sqi/internal/store"
)

const (
	workerCredentialCols = `id, worker_id, public_key, name, enrolled_at, last_seen_at, revoked_at, capacity_group_id` //nolint:gosec // G101: column names, not a credential

	workerJoinTokenCols = `id, token_hash, prefix, name, expires_at, used_at, created_by, created_at, capacity_group_id, revoked_at` //nolint:gosec // G101: column names, not a credential

	sqlInsertWorkerCredential = `INSERT INTO worker_credentials (` + workerCredentialCols + `) VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING ` + workerCredentialCols

	// gosec G101 flags these consts on the false-positive belief that an
	// identifier containing "Credential" is a hardcoded credential; the
	// value is SQL text, not a secret. The nolint must be a trailing comment
	// on the declaration's own line (where gosec attributes the finding), so
	// these stay single-line rather than the multi-line style used
	// elsewhere.

	sqlListActiveWorkerCredentials = `SELECT ` + workerCredentialCols + ` FROM worker_credentials WHERE revoked_at IS NULL ORDER BY enrolled_at`

	sqlGetActiveWorkerCredentialByPublicKey = `SELECT ` + workerCredentialCols + ` FROM worker_credentials WHERE public_key = ? AND revoked_at IS NULL`

	sqlRevokeWorkerCredential = `UPDATE worker_credentials SET revoked_at = ? WHERE worker_id = ? AND revoked_at IS NULL` //nolint:gosec // G101: SQL text, not a credential

	sqlTouchWorkerCredential = `UPDATE worker_credentials SET last_seen_at = ? WHERE worker_id = ? AND revoked_at IS NULL` //nolint:gosec // G101: SQL text, not a credential

	sqlInsertWorkerJoinToken = `INSERT INTO worker_join_tokens (` + workerJoinTokenCols + `) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING ` + workerJoinTokenCols

	sqlGetWorkerJoinTokenByHash = `SELECT ` + workerJoinTokenCols + ` FROM worker_join_tokens WHERE token_hash = ?`

	sqlListCapacityGroupTokens = `SELECT ` + workerJoinTokenCols + ` FROM worker_join_tokens WHERE capacity_group_id = ? ORDER BY created_at DESC, id`

	// sqlRevokeWorkerJoinToken keeps the first revocation time, so revoking a
	// revoked token matches its row and changes nothing.
	sqlRevokeWorkerJoinToken = `UPDATE worker_join_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`

	sqlMarkWorkerJoinTokenUsed = `UPDATE worker_join_tokens SET used_at = ? WHERE id = ?`

	// sqlConsumeWorkerJoinToken claims a single-use token in ONE statement:
	// the used_at IS NULL and expires_at > ? predicates are the validity
	// check (a revoked token is refused like a used one), and the SET is the
	// claim. Two concurrent enrollments presenting
	// the same token therefore cannot both succeed — the second matches no
	// row and gets store.ErrNotFound, indistinguishable from an unknown
	// token, which is what the unauthenticated enroll endpoint needs. Used
	// only inside RedeemWorkerJoinToken's transaction (raw text via tx, not
	// a prepared statement — see that method's doc comment), never on its
	// own: a token claimed here has to be followed by successfully creating
	// the credential it authorizes, in the same transaction, or the claim
	// itself must roll back.
	sqlConsumeWorkerJoinToken = `UPDATE worker_join_tokens SET used_at = ? WHERE token_hash = ? AND used_at IS NULL AND revoked_at IS NULL AND expires_at > ? RETURNING ` + workerJoinTokenCols
)

func scanWorkerCredential(row scanner) (store.WorkerCredential, error) {
	var c store.WorkerCredential
	var lastSeenAt, revokedAt, capacityGroupID sql.NullString
	var enrolledAt string
	if err := row.Scan(&c.ID, &c.WorkerID, &c.PublicKey, &c.Name,
		&enrolledAt, &lastSeenAt, &revokedAt, &capacityGroupID); err != nil {
		return store.WorkerCredential{}, err
	}
	c.EnrolledAt = mustTime(enrolledAt)
	c.LastSeenAt = nullTextToTime(lastSeenAt)
	c.RevokedAt = nullTextToTime(revokedAt)
	c.CapacityGroupID = capacityGroupID.String
	return c, nil
}

func scanWorkerJoinToken(row scanner) (store.WorkerJoinToken, error) {
	var t store.WorkerJoinToken
	var usedAt, capacityGroupID, revokedAt sql.NullString
	var expiresAt, createdAt string
	if err := row.Scan(&t.ID, &t.TokenHash, &t.Prefix, &t.Name,
		&expiresAt, &usedAt, &t.CreatedBy, &createdAt, &capacityGroupID, &revokedAt); err != nil {
		return store.WorkerJoinToken{}, err
	}
	t.ExpiresAt = mustTime(expiresAt)
	t.UsedAt = nullTextToTime(usedAt)
	t.CreatedAt = mustTime(createdAt)
	t.CapacityGroupID = capacityGroupID.String
	t.RevokedAt = nullTextToTime(revokedAt)
	return t, nil
}

// CreateWorkerCredential implements [store.WorkerCredentialStore].
func (s *Store) CreateWorkerCredential(ctx context.Context, c store.WorkerCredential) (store.WorkerCredential, error) {
	out, err := writeRow(ctx, s, "create worker credential", scanWorkerCredential, s.stmtInsertWorkerCredential,
		c.ID, c.WorkerID, c.PublicKey, c.Name,
		TimeText(c.EnrolledAt), nullTimeToText(c.LastSeenAt), nullTimeToText(c.RevokedAt), nullString(c.CapacityGroupID))
	return out, mapErr(err)
}

// ListActiveWorkerCredentials implements [store.WorkerCredentialStore].
func (s *Store) ListActiveWorkerCredentials(ctx context.Context) ([]store.WorkerCredential, error) {
	rows, err := s.stmtListActiveWorkerCredentials.QueryContext(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []store.WorkerCredential
	for rows.Next() {
		c, err := scanWorkerCredential(rows)
		if err != nil {
			return nil, mapErr(err)
		}
		out = append(out, c)
	}
	return out, mapErr(rows.Err())
}

// GetActiveWorkerCredentialByPublicKey implements [store.WorkerCredentialStore].
func (s *Store) GetActiveWorkerCredentialByPublicKey(ctx context.Context, publicKey string) (store.WorkerCredential, error) {
	row := s.stmtGetActiveWorkerCredentialByPublicKey.QueryRowContext(ctx, publicKey)
	out, err := scanWorkerCredential(row)
	return out, mapErr(err)
}

// RevokeWorkerCredential implements [store.WorkerCredentialStore].
func (s *Store) RevokeWorkerCredential(ctx context.Context, workerID string, at time.Time) error {
	res, err := s.execStmt(ctx, "revoke worker credential", s.stmtRevokeWorkerCredential, TimeText(at), workerID)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}

// TouchWorkerCredential implements [store.WorkerCredentialStore].
func (s *Store) TouchWorkerCredential(ctx context.Context, workerID string, at time.Time) error {
	res, err := s.execStmt(ctx, "touch worker credential", s.stmtTouchWorkerCredential, TimeText(at), workerID)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}

// CreateWorkerJoinToken implements [store.WorkerCredentialStore].
func (s *Store) CreateWorkerJoinToken(ctx context.Context, t store.WorkerJoinToken) (store.WorkerJoinToken, error) {
	out, err := writeRow(ctx, s, "create worker join token", scanWorkerJoinToken, s.stmtInsertWorkerJoinToken,
		t.ID, t.TokenHash, t.Prefix, t.Name,
		TimeText(t.ExpiresAt), nullTimeToText(t.UsedAt), t.CreatedBy, TimeText(t.CreatedAt),
		nullString(t.CapacityGroupID), nullTimeToText(t.RevokedAt))
	return out, mapErr(err)
}

// GetWorkerJoinTokenByHash implements [store.WorkerCredentialStore].
func (s *Store) GetWorkerJoinTokenByHash(ctx context.Context, hash string) (store.WorkerJoinToken, error) {
	row := s.stmtGetWorkerJoinTokenByHash.QueryRowContext(ctx, hash)
	out, err := scanWorkerJoinToken(row)
	return out, mapErr(err)
}

// MarkWorkerJoinTokenUsed implements [store.WorkerCredentialStore].
func (s *Store) MarkWorkerJoinTokenUsed(ctx context.Context, id string, at time.Time) error {
	res, err := s.execStmt(ctx, "mark worker join token used", s.stmtMarkWorkerJoinTokenUsed, TimeText(at), id)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}

// ListCapacityGroupTokens implements [store.WorkerCredentialStore].
func (s *Store) ListCapacityGroupTokens(ctx context.Context, groupID string) ([]store.WorkerJoinToken, error) {
	rows, err := s.stmtListCapacityGroupTokens.QueryContext(ctx, groupID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []store.WorkerJoinToken
	for rows.Next() {
		t, err := scanWorkerJoinToken(rows)
		if err != nil {
			return nil, mapErr(err)
		}
		out = append(out, t)
	}
	return out, mapErr(rows.Err())
}

// RevokeWorkerJoinToken implements [store.WorkerCredentialStore].
func (s *Store) RevokeWorkerJoinToken(ctx context.Context, id string, at time.Time) error {
	res, err := s.execStmt(ctx, "revoke worker join token", s.stmtRevokeWorkerJoinToken, TimeText(at), id)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}

// RedeemWorkerJoinToken implements [store.WorkerCredentialStore].
//
// Follows the same idiom as CreateJobSubmission (job.go): the whole
// operation runs inside one transaction (inTx), using raw SQL via tx rather
// than the prepared statements the standalone per-row methods use.
//
// The token claim reuses sqlConsumeWorkerJoinToken's text — a single
// UPDATE ... RETURNING that carries both the validity check and the claim in
// one statement, so an unknown token, an expired one, and one another
// request claimed a moment earlier are all the same "matched no row"
// outcome. If that claim fails, the transaction is rolled back and
// [store.ErrNotFound] is returned with the token untouched.
//
// The credential insert happens inside the SAME transaction, so a conflict
// there — a worker ID already bound to an active credential, or a public
// key already enrolled anywhere — rolls back the token claim too: the token
// is NOT consumed, and remains redeemable by a later, non-conflicting
// request.
func (s *Store) RedeemWorkerJoinToken(ctx context.Context, hash string, now time.Time, cred store.WorkerCredential) (store.WorkerCredential, error) {
	return inTxValue(ctx, s, "redeem worker join token", func(tx *dbTx) (store.WorkerCredential, txOutcome, error) {
		nowText := TimeText(now)
		tokRow := tx.QueryRowContext(ctx, sqlConsumeWorkerJoinToken, nowText, hash, nowText)
		if _, err := scanWorkerJoinToken(tokRow); err != nil {
			return store.WorkerCredential{}, txRollback, mapErr(err)
		}

		credRow := tx.QueryRowContext(ctx, sqlInsertWorkerCredential, cred.ID, cred.WorkerID, cred.PublicKey, cred.Name,
			TimeText(cred.EnrolledAt), nullTimeToText(cred.LastSeenAt), nullTimeToText(cred.RevokedAt),
			nullString(cred.CapacityGroupID))
		out, err := scanWorkerCredential(credRow)
		if err != nil {
			return store.WorkerCredential{}, txRollback, mapErr(err)
		}
		return out, txCommit, nil
	})
}
