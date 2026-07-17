// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/uberware/sqi/internal/store"
)

const (
	// gosec G101 flags these consts on the false-positive belief that an
	// identifier containing "APIKey" is a hardcoded credential; the value is
	// SQL text, not a secret. The nolint must be a trailing comment on the
	// declaration's own line (where gosec attributes the finding), so these
	// stay single-line rather than the multi-line style used elsewhere.
	sqlInsertAPIKey = `INSERT INTO api_keys (id, user_id, name, token_hash, prefix, expires_at, last_used_at, revoked_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id, user_id, name, token_hash, prefix, expires_at, last_used_at, revoked_at, created_at` //nolint:gosec // G101: SQL text, not a credential

	sqlGetAPIKeyByTokenHash = `SELECT id, user_id, name, token_hash, prefix, expires_at, last_used_at, revoked_at, created_at FROM api_keys WHERE token_hash = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)` //nolint:gosec // G101: SQL text, not a credential

	sqlListAPIKeysForUser = `SELECT id, user_id, name, token_hash, prefix, expires_at, last_used_at, revoked_at, created_at FROM api_keys WHERE user_id = ? AND revoked_at IS NULL ORDER BY created_at DESC` //nolint:gosec // G101: SQL text, not a credential

	sqlRevokeAPIKey        = `UPDATE api_keys SET revoked_at = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL` //nolint:gosec // G101: SQL text, not a credential
	sqlTouchAPIKeyLastUsed = `UPDATE api_keys SET last_used_at = ? WHERE id = ?`                                      //nolint:gosec // G101: SQL text, not a credential
)

func scanAPIKey(row scanner) (store.APIKey, error) {
	var k store.APIKey
	var expiresAt, lastUsedAt, revokedAt sql.NullString
	var createdAt string
	if err := row.Scan(&k.ID, &k.UserID, &k.Name, &k.TokenHash, &k.Prefix,
		&expiresAt, &lastUsedAt, &revokedAt, &createdAt); err != nil {
		return store.APIKey{}, err
	}
	k.ExpiresAt = nullTextToTime(expiresAt)
	k.LastUsedAt = nullTextToTime(lastUsedAt)
	k.RevokedAt = nullTextToTime(revokedAt)
	k.CreatedAt = mustTime(createdAt)
	return k, nil
}

// CreateAPIKey implements [store.APIKeyStore].
func (s *Store) CreateAPIKey(ctx context.Context, k store.APIKey) (store.APIKey, error) {
	row := s.stmtInsertAPIKey.QueryRowContext(ctx, k.ID, k.UserID, k.Name, k.TokenHash, k.Prefix,
		nullTimeToText(k.ExpiresAt), nullTimeToText(k.LastUsedAt), nullTimeToText(k.RevokedAt),
		timeToText(k.CreatedAt))
	out, err := scanAPIKey(row)
	return out, mapErr(err)
}

// GetAPIKeyByTokenHash implements [store.APIKeyStore].
func (s *Store) GetAPIKeyByTokenHash(ctx context.Context, tokenHash string, now time.Time) (store.APIKey, error) {
	row := s.stmtGetAPIKeyByTokenHash.QueryRowContext(ctx, tokenHash, timeToText(now))
	out, err := scanAPIKey(row)
	return out, mapErr(err)
}

// ListAPIKeysForUser implements [store.APIKeyStore].
func (s *Store) ListAPIKeysForUser(ctx context.Context, userID string) ([]store.APIKey, error) {
	rows, err := s.stmtListAPIKeysForUser.QueryContext(ctx, userID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []store.APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, mapErr(err)
		}
		out = append(out, k)
	}
	return out, mapErr(rows.Err())
}

// RevokeAPIKey implements [store.APIKeyStore].
func (s *Store) RevokeAPIKey(ctx context.Context, id, userID string, now time.Time) error {
	res, err := s.stmtRevokeAPIKey.ExecContext(ctx, timeToText(now), id, userID)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}

// TouchAPIKeyLastUsed implements [store.APIKeyStore].
func (s *Store) TouchAPIKeyLastUsed(ctx context.Context, id string, now time.Time) error {
	res, err := s.stmtTouchAPIKeyLastUsed.ExecContext(ctx, timeToText(now), id)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}
