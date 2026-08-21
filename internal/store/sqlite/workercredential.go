// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/uberware/sqi/internal/store"
)

const (
	sqlInsertWorkerCredential = `INSERT INTO worker_credentials (id, worker_id, public_key, name, enrolled_at, last_seen_at, revoked_at) VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id, worker_id, public_key, name, enrolled_at, last_seen_at, revoked_at`

	// gosec G101 flags these consts on the false-positive belief that an
	// identifier containing "Credential" is a hardcoded credential; the
	// value is SQL text, not a secret. The nolint must be a trailing comment
	// on the declaration's own line (where gosec attributes the finding), so
	// these stay single-line rather than the multi-line style used
	// elsewhere.
	sqlGetActiveWorkerCredentialByWorkerID = `SELECT id, worker_id, public_key, name, enrolled_at, last_seen_at, revoked_at FROM worker_credentials WHERE worker_id = ? AND revoked_at IS NULL` //nolint:gosec // G101: SQL text, not a credential

	sqlListActiveWorkerCredentials = `SELECT id, worker_id, public_key, name, enrolled_at, last_seen_at, revoked_at FROM worker_credentials WHERE revoked_at IS NULL ORDER BY enrolled_at` //nolint:gosec // G101: SQL text, not a credential

	sqlRevokeWorkerCredential = `UPDATE worker_credentials SET revoked_at = ? WHERE worker_id = ? AND revoked_at IS NULL` //nolint:gosec // G101: SQL text, not a credential

	sqlTouchWorkerCredential = `UPDATE worker_credentials SET last_seen_at = ? WHERE worker_id = ? AND revoked_at IS NULL` //nolint:gosec // G101: SQL text, not a credential

	sqlInsertWorkerJoinToken = `INSERT INTO worker_join_tokens (id, token_hash, prefix, name, expires_at, used_at, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id, token_hash, prefix, name, expires_at, used_at, created_by, created_at`

	sqlGetWorkerJoinTokenByHash = `SELECT id, token_hash, prefix, name, expires_at, used_at, created_by, created_at FROM worker_join_tokens WHERE token_hash = ?` //nolint:gosec // G101: SQL text, not a credential

	sqlMarkWorkerJoinTokenUsed = `UPDATE worker_join_tokens SET used_at = ? WHERE id = ?`

	// sqlConsumeWorkerJoinToken claims a single-use token in ONE statement:
	// the used_at IS NULL and expires_at > ? predicates are the validity
	// check, and the SET is the claim. Two concurrent enrollments presenting
	// the same token therefore cannot both succeed — the second matches no
	// row and gets store.ErrNotFound, indistinguishable from an unknown
	// token, which is what the unauthenticated enroll endpoint needs. Used
	// only inside RedeemWorkerJoinToken's transaction (raw text via tx, not
	// a prepared statement — see that method's doc comment), never on its
	// own: a token claimed here has to be followed by successfully creating
	// the credential it authorizes, in the same transaction, or the claim
	// itself must roll back.
	sqlConsumeWorkerJoinToken = `UPDATE worker_join_tokens SET used_at = ? WHERE token_hash = ? AND used_at IS NULL AND expires_at > ? RETURNING id, token_hash, prefix, name, expires_at, used_at, created_by, created_at`
)

func scanWorkerCredential(row scanner) (store.WorkerCredential, error) {
	var c store.WorkerCredential
	var lastSeenAt, revokedAt sql.NullString
	var enrolledAt string
	if err := row.Scan(&c.ID, &c.WorkerID, &c.PublicKey, &c.Name,
		&enrolledAt, &lastSeenAt, &revokedAt); err != nil {
		return store.WorkerCredential{}, err
	}
	c.EnrolledAt = mustTime(enrolledAt)
	c.LastSeenAt = nullTextToTime(lastSeenAt)
	c.RevokedAt = nullTextToTime(revokedAt)
	return c, nil
}

func scanWorkerJoinToken(row scanner) (store.WorkerJoinToken, error) {
	var t store.WorkerJoinToken
	var usedAt sql.NullString
	var expiresAt, createdAt string
	if err := row.Scan(&t.ID, &t.TokenHash, &t.Prefix, &t.Name,
		&expiresAt, &usedAt, &t.CreatedBy, &createdAt); err != nil {
		return store.WorkerJoinToken{}, err
	}
	t.ExpiresAt = mustTime(expiresAt)
	t.UsedAt = nullTextToTime(usedAt)
	t.CreatedAt = mustTime(createdAt)
	return t, nil
}

// CreateWorkerCredential implements [store.WorkerCredentialStore].
func (s *Store) CreateWorkerCredential(ctx context.Context, c store.WorkerCredential) (store.WorkerCredential, error) {
	row := s.stmtInsertWorkerCredential.QueryRowContext(ctx, c.ID, c.WorkerID, c.PublicKey, c.Name,
		timeToText(c.EnrolledAt), nullTimeToText(c.LastSeenAt), nullTimeToText(c.RevokedAt))
	out, err := scanWorkerCredential(row)
	return out, mapErr(err)
}

// GetActiveWorkerCredentialByWorkerID implements [store.WorkerCredentialStore].
func (s *Store) GetActiveWorkerCredentialByWorkerID(ctx context.Context, workerID string) (store.WorkerCredential, error) {
	row := s.stmtGetActiveWorkerCredentialByWorkerID.QueryRowContext(ctx, workerID)
	out, err := scanWorkerCredential(row)
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

// RevokeWorkerCredential implements [store.WorkerCredentialStore].
func (s *Store) RevokeWorkerCredential(ctx context.Context, workerID string, at time.Time) error {
	res, err := s.stmtRevokeWorkerCredential.ExecContext(ctx, timeToText(at), workerID)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}

// TouchWorkerCredential implements [store.WorkerCredentialStore].
func (s *Store) TouchWorkerCredential(ctx context.Context, workerID string, at time.Time) error {
	res, err := s.stmtTouchWorkerCredential.ExecContext(ctx, timeToText(at), workerID)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}

// CreateWorkerJoinToken implements [store.WorkerCredentialStore].
func (s *Store) CreateWorkerJoinToken(ctx context.Context, t store.WorkerJoinToken) (store.WorkerJoinToken, error) {
	row := s.stmtInsertWorkerJoinToken.QueryRowContext(ctx, t.ID, t.TokenHash, t.Prefix, t.Name,
		timeToText(t.ExpiresAt), nullTimeToText(t.UsedAt), t.CreatedBy, timeToText(t.CreatedAt))
	out, err := scanWorkerJoinToken(row)
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
	res, err := s.stmtMarkWorkerJoinTokenUsed.ExecContext(ctx, timeToText(at), id)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}

// RedeemWorkerJoinToken implements [store.WorkerCredentialStore].
//
// Follows the same idiom as CreateJobSubmission (job.go): the whole
// operation runs inside one transaction, using raw SQL via tx rather than
// the prepared statements the standalone per-row methods use, and a
// deferred Rollback that is a no-op after a successful Commit.
//
// The token claim reuses sqlConsumeWorkerJoinToken's text — a single
// UPDATE ... RETURNING that carries both the validity check and the claim in
// one statement, so an unknown token, an expired one, and one another
// request claimed a moment earlier are all the same "matched no row"
// outcome. If that claim fails, the transaction is rolled back (via the
// deferred Rollback) and [store.ErrNotFound] is returned with the token
// untouched.
//
// The credential insert happens inside the SAME transaction, so a conflict
// there — a worker ID already bound to an active credential, or a public
// key already enrolled anywhere — rolls back the token claim too: the token
// is NOT consumed, and remains redeemable by a later, non-conflicting
// request.
func (s *Store) RedeemWorkerJoinToken(ctx context.Context, hash string, now time.Time, cred store.WorkerCredential) (store.WorkerCredential, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.WorkerCredential{}, mapErr(err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback after commit is a no-op

	nowText := timeToText(now)
	tokRow := tx.QueryRowContext(ctx, sqlConsumeWorkerJoinToken, nowText, hash, nowText)
	if _, err := scanWorkerJoinToken(tokRow); err != nil {
		return store.WorkerCredential{}, mapErr(err)
	}

	credRow := tx.QueryRowContext(ctx, sqlInsertWorkerCredential, cred.ID, cred.WorkerID, cred.PublicKey, cred.Name,
		timeToText(cred.EnrolledAt), nullTimeToText(cred.LastSeenAt), nullTimeToText(cred.RevokedAt))
	out, err := scanWorkerCredential(credRow)
	if err != nil {
		return store.WorkerCredential{}, mapErr(err)
	}

	if err := tx.Commit(); err != nil {
		return store.WorkerCredential{}, mapErr(err)
	}
	return out, nil
}
