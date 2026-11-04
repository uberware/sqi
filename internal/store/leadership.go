// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"time"
)

// Lease is the leadership lease as the database holds it. Every instant is on
// the database's clock, never a server's.
type Lease struct {
	// Holder is the instance id leading, or "" when the lease is released.
	Holder string
	// Epoch is the epoch Holder leads at. It rises on every takeover, so a
	// write stamped with an older epoch is known to come from a deposed leader.
	Epoch int64
	// ExpiresUS is when the lease lapses, in microseconds since the Unix epoch.
	ExpiresUS int64
	// NowUS is the database clock when the lease was read, on the same scale,
	// so a reader can tell whether ExpiresUS has passed without its own clock.
	NowUS int64
}

// ServerMember is one server instance that has run against this database.
type ServerMember struct {
	InstanceID        string
	Name              string
	Host              string
	PID               int
	Version           string
	GrantsFingerprint string
	StartedAt         time.Time
	LastSeenUS        int64  // set by the store from the database clock
	BrokerOK          bool   // input to Upsert: stamp BrokerOKUS now
	BrokerOKUS        *int64 // output of List: the last stamp, nil if never
}

// LeadershipStore is the leader lease and the membership of the servers
// sharing one database.
//
// Its methods are never fenced ([WithLeaderEpoch]), even when called with a
// leadership context, stale or current: they are how a server comes to hold
// an epoch and keeps it, so they must work whatever epoch it holds. Where the store
// has a connection of its own for them they use it, so a renewal is never
// queued behind other writes.
type LeadershipStore interface {
	// AcquireLeadership takes the lease for holder for ttl and returns the
	// new epoch, when it is unheld or has lapsed. A lease still held and
	// unexpired returns ok false and no error. Every takeover, of a lapsed
	// lease or a released one, raises the epoch by one.
	AcquireLeadership(ctx context.Context, holder string, ttl time.Duration) (epoch int64, ok bool, err error)
	// RenewLeadership extends holder's lease at epoch to ttl from now. ok is
	// false when holder no longer holds the lease at epoch, which is a loss of
	// authority rather than an error.
	RenewLeadership(ctx context.Context, holder string, epoch int64, ttl time.Duration) (ok bool, err error)
	// ReleaseLeadership gives the lease up when holder holds it at epoch. It
	// does nothing, and returns nil, for anyone else. The epoch is unchanged:
	// the next acquire raises it.
	ReleaseLeadership(ctx context.Context, holder string, epoch int64) error
	// GetLeadership returns the lease and the database clock.
	GetLeadership(ctx context.Context) (Lease, error)

	// UpsertServerMember records m as seen now, inserting the member or
	// refreshing its row. LastSeenUS is stamped from the database clock; the
	// broker stamp is refreshed only when m.BrokerOK is set, so a member that
	// has lost its broker keeps the time it last saw it.
	UpsertServerMember(ctx context.Context, m ServerMember) error
	// ListServerMembers returns every member, ordered by name then instance
	// id, and the database clock the members' stamps are compared against.
	ListServerMembers(ctx context.Context) (members []ServerMember, nowUS int64, err error)
	// DeleteServerMembersUnseenFor removes members not seen for d and returns
	// how many it removed.
	DeleteServerMembersUnseenFor(ctx context.Context, d time.Duration) (int, error)
}
