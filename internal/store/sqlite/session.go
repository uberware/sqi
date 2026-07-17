// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"time"

	"github.com/uberware/sqi/internal/store"
)

const (
	sqlInsertSession = `
INSERT INTO sessions (id, token_hash, user_id, expires_at, created_at)
VALUES (?, ?, ?, ?, ?)
RETURNING id, token_hash, user_id, expires_at, created_at`

	sqlGetSessionByTokenHash = `SELECT id, token_hash, user_id, expires_at, created_at FROM sessions WHERE token_hash = ? AND expires_at > ?` //nolint:gosec // G101: SQL text, not a credential

	sqlDeleteSession         = `DELETE FROM sessions WHERE id = ?`
	sqlDeleteSessionsForUser = `DELETE FROM sessions WHERE user_id = ?`
	sqlDeleteExpiredSessions = `DELETE FROM sessions WHERE expires_at <= ?`
)

func scanSession(row scanner) (store.Session, error) {
	var s store.Session
	var expiresAt, createdAt string
	if err := row.Scan(&s.ID, &s.TokenHash, &s.UserID, &expiresAt, &createdAt); err != nil {
		return store.Session{}, err
	}
	s.ExpiresAt = mustTime(expiresAt)
	s.CreatedAt = mustTime(createdAt)
	return s, nil
}

// CreateSession implements [store.SessionStore].
//
// Unlike CreateUser, this does not stamp CreatedAt (or ExpiresAt) server-side:
// both are caller-supplied by design, since ExpiresAt is derived from the
// issuance instant and the two timestamps must agree with each other.
func (s *Store) CreateSession(ctx context.Context, sess store.Session) (store.Session, error) {
	row := s.stmtInsertSession.QueryRowContext(ctx, sess.ID, sess.TokenHash, sess.UserID,
		timeToText(sess.ExpiresAt), timeToText(sess.CreatedAt))
	out, err := scanSession(row)
	return out, mapErr(err)
}

// GetSessionByTokenHash implements [store.SessionStore].
func (s *Store) GetSessionByTokenHash(ctx context.Context, tokenHash string, now time.Time) (store.Session, error) {
	row := s.stmtGetSessionByTokenHash.QueryRowContext(ctx, tokenHash, timeToText(now))
	out, err := scanSession(row)
	return out, mapErr(err)
}

// DeleteSession implements [store.SessionStore].
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	res, err := s.stmtDeleteSession.ExecContext(ctx, id)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}

// DeleteSessionsForUser implements [store.SessionStore].
func (s *Store) DeleteSessionsForUser(ctx context.Context, userID string) error {
	_, err := s.stmtDeleteSessionsForUser.ExecContext(ctx, userID)
	return mapErr(err)
}

// DeleteExpiredSessions implements [store.SessionStore].
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) (int, error) {
	res, err := s.stmtDeleteExpiredSessions.ExecContext(ctx, timeToText(now))
	if err != nil {
		return 0, mapErr(err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}
