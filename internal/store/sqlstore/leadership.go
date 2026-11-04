// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// The leadership statements name the database clock as {now}; [Store.leaseSQL]
// substitutes the dialect's expression, so a lease is always aged by the
// database and never by a server. Both pools of a database see one clock, and
// SQLite's reads it once per statement.
const (
	// sqlAcquireLease takes an unheld or lapsed lease and returns the epoch it
	// now leads at. No row back means someone else holds it.
	sqlAcquireLease = `UPDATE server_lease SET holder = ?, epoch = epoch + 1, expires_us = {now} + ?
WHERE id = 1 AND (holder IS NULL OR expires_us < {now})
RETURNING epoch`

	// sqlSetFenceEpoch publishes the new epoch. On PostgreSQL this UPDATE waits
	// for any fenced commit already past its check, which is the ordering point
	// between the old leader's last write and the new leader's first.
	sqlSetFenceEpoch = `UPDATE server_epoch SET epoch = ? WHERE id = 1`

	sqlRenewLease = `UPDATE server_lease SET expires_us = {now} + ?
WHERE id = 1 AND holder = ? AND epoch = ?`

	sqlReleaseLease = `UPDATE server_lease SET holder = NULL WHERE id = 1 AND holder = ? AND epoch = ?`

	sqlGetLease = `SELECT holder, epoch, expires_us, {now} FROM server_lease WHERE id = 1`

	sqlNowMicros = `SELECT {now}`

	// sqlUpsertServerMember stamps last_seen_us from the database clock, and
	// broker_ok_us only for a member that saw its broker (? = 1): COALESCE
	// keeps the earlier stamp for one that did not.
	sqlUpsertServerMember = `INSERT INTO server_members
  (instance_id, name, host, pid, version, grants_fingerprint, started_at, last_seen_us, broker_ok_us)
VALUES (?, ?, ?, ?, ?, ?, ?, {now}, CASE WHEN ? = 1 THEN {now} ELSE NULL END)
ON CONFLICT (instance_id) DO UPDATE SET
  name = excluded.name, host = excluded.host, pid = excluded.pid, version = excluded.version,
  grants_fingerprint = excluded.grants_fingerprint, started_at = excluded.started_at,
  last_seen_us = excluded.last_seen_us,
  broker_ok_us = COALESCE(excluded.broker_ok_us, server_members.broker_ok_us)`

	sqlListServerMembers = `SELECT instance_id, name, host, pid, version, grants_fingerprint, started_at,
  last_seen_us, broker_ok_us
FROM server_members ORDER BY name, instance_id`

	sqlDeleteServerMembersUnseen = `DELETE FROM server_members WHERE last_seen_us < {now} - ?`
)

// leaseSQL returns query with the database clock expression in place of {now}.
func (s *Store) leaseSQL(query string) string {
	return strings.ReplaceAll(query, "{now}", s.d.ClockMicros())
}

// leasePool returns the pool the leadership methods run on: the one
// [WithLeasePool] set, so a renewal never queues behind other writes, or the
// write pool when none was.
func (s *Store) leasePool() *dbPool {
	if s.lease != nil {
		return s.lease
	}
	return s.w
}

// leaseTTLMicros converts ttl to the integer microseconds the lease columns
// hold, refusing one that rounds to nothing: a lease that expires as it is
// granted could never be told from a lost one.
func leaseTTLMicros(ttl time.Duration) (int64, error) {
	us := ttl.Microseconds()
	if us <= 0 {
		return 0, fmt.Errorf("store: lease ttl %v must be at least 1µs", ttl)
	}
	return us, nil
}

// AcquireLeadership implements [store.LeadershipStore].
func (s *Store) AcquireLeadership(ctx context.Context, holder string, ttl time.Duration) (epoch int64, ok bool, err error) {
	ttlUS, err := leaseTTLMicros(ttl)
	if err != nil {
		return 0, false, err
	}
	err = s.inTxOn(ctx, s.leasePool(), "acquire leadership", func(tx *dbTx) (txOutcome, error) {
		epoch, ok = 0, false
		err := tx.QueryRowContext(ctx, s.leaseSQL(sqlAcquireLease), holder, ttlUS).Scan(&epoch)
		if errors.Is(err, sql.ErrNoRows) {
			return txRollback, nil // held, and nothing written
		}
		if err != nil {
			return txRollback, mapErr(err)
		}
		res, err := tx.ExecContext(ctx, sqlSetFenceEpoch, epoch)
		if err != nil {
			return txRollback, mapErr(err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return txRollback, mapErr(err)
		}
		if n != 1 {
			return txRollback, fmt.Errorf("store: server_epoch has no row to update (updated %d)", n)
		}
		ok = true
		return txCommit, nil
	})
	if err != nil {
		return 0, false, err
	}
	return epoch, ok, nil
}

// RenewLeadership implements [store.LeadershipStore].
func (s *Store) RenewLeadership(ctx context.Context, holder string, epoch int64, ttl time.Duration) (bool, error) {
	ttlUS, err := leaseTTLMicros(ttl)
	if err != nil {
		return false, err
	}
	res, err := s.leasePool().ExecContext(ctx, s.leaseSQL(sqlRenewLease), ttlUS, holder, epoch)
	if err != nil {
		return false, mapErr(err)
	}
	n, err := res.RowsAffected()
	return n == 1, mapErr(err)
}

// ReleaseLeadership implements [store.LeadershipStore].
func (s *Store) ReleaseLeadership(ctx context.Context, holder string, epoch int64) error {
	_, err := s.leasePool().ExecContext(ctx, s.leaseSQL(sqlReleaseLease), holder, epoch)
	return mapErr(err)
}

// GetLeadership implements [store.LeadershipStore].
func (s *Store) GetLeadership(ctx context.Context) (store.Lease, error) {
	var l store.Lease
	var holder sql.NullString
	err := s.leasePool().QueryRowContext(ctx, s.leaseSQL(sqlGetLease)).Scan(&holder, &l.Epoch, &l.ExpiresUS, &l.NowUS)
	if err != nil {
		return store.Lease{}, mapErr(err)
	}
	l.Holder = holder.String
	return l, nil
}

// UpsertServerMember implements [store.LeadershipStore].
func (s *Store) UpsertServerMember(ctx context.Context, m store.ServerMember) error {
	brokerOK := int64(0)
	if m.BrokerOK {
		brokerOK = 1
	}
	_, err := s.leasePool().ExecContext(ctx, s.leaseSQL(sqlUpsertServerMember),
		m.InstanceID, m.Name, m.Host, int64(m.PID), m.Version, m.GrantsFingerprint, TimeText(m.StartedAt), brokerOK)
	return mapErr(err)
}

// ListServerMembers implements [store.LeadershipStore].
func (s *Store) ListServerMembers(ctx context.Context) ([]store.ServerMember, int64, error) {
	p := s.leasePool()
	rows, err := p.QueryContext(ctx, sqlListServerMembers)
	if err != nil {
		return nil, 0, mapErr(err)
	}
	defer rows.Close()
	var out []store.ServerMember
	for rows.Next() {
		var m store.ServerMember
		var pid int64
		var started string
		var brokerOK sql.NullInt64
		if err := rows.Scan(&m.InstanceID, &m.Name, &m.Host, &pid, &m.Version, &m.GrantsFingerprint,
			&started, &m.LastSeenUS, &brokerOK); err != nil {
			return nil, 0, mapErr(err)
		}
		m.PID = int(pid)
		if m.StartedAt, err = textToTime(started); err != nil {
			return nil, 0, err
		}
		if brokerOK.Valid {
			m.BrokerOKUS = &brokerOK.Int64
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, mapErr(err)
	}
	// Read after the members, so no stamp is ahead of the clock it is
	// compared with.
	var now int64
	if err := p.QueryRowContext(ctx, s.leaseSQL(sqlNowMicros)).Scan(&now); err != nil {
		return nil, 0, mapErr(err)
	}
	return out, now, nil
}

// DeleteServerMembersUnseenFor implements [store.LeadershipStore].
func (s *Store) DeleteServerMembersUnseenFor(ctx context.Context, d time.Duration) (int, error) {
	res, err := s.leasePool().ExecContext(ctx, s.leaseSQL(sqlDeleteServerMembersUnseen), d.Microseconds())
	if err != nil {
		return 0, mapErr(err)
	}
	n, err := res.RowsAffected()
	return int(n), mapErr(err)
}
