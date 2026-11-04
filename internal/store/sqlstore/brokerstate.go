// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const (
	sqlGetBrokerIdentity = `SELECT identity FROM broker_state WHERE id = 1`

	sqlSetBrokerIdentity = `INSERT INTO broker_state (id, identity, recorded_at) VALUES (1, ?, ?)
ON CONFLICT (id) DO UPDATE SET identity = excluded.identity, recorded_at = excluded.recorded_at`
)

// GetBrokerIdentity implements [store.BrokerStateStore].
func (s *Store) GetBrokerIdentity(ctx context.Context) (string, error) {
	var identity string
	err := s.stmtGetBrokerIdentity.QueryRowContext(ctx).Scan(&identity)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return identity, mapErr(err)
}

// SetBrokerIdentity implements [store.BrokerStateStore].
func (s *Store) SetBrokerIdentity(ctx context.Context, identity string, at time.Time) error {
	_, err := s.execStmt(ctx, "set broker identity", s.stmtSetBrokerIdentity, identity, TimeText(at))
	return mapErr(err)
}
