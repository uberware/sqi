// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"errors"
)

// ErrFenced is returned by a write made under a leadership context
// ([WithLeaderEpoch]) whose epoch is no longer the database's: another server
// has taken over since, and the write was rolled back. The server that gets it
// has lost its authority.
var ErrFenced = errors.New("store: this server is no longer the leader")

// leaderEpochKey is the context key of the epoch [WithLeaderEpoch] attaches.
type leaderEpochKey struct{}

// WithLeaderEpoch returns ctx carrying the epoch its server leads at. A store
// write made under it commits only while epoch is still the database's
// fencing epoch, checked as the last statement before the commit, and
// otherwise fails with [ErrFenced]. Reads, and the [LeadershipStore] methods,
// ignore it.
func WithLeaderEpoch(ctx context.Context, epoch int64) context.Context {
	return context.WithValue(ctx, leaderEpochKey{}, epoch)
}

// LeaderEpoch returns the epoch ctx carries, and whether it carries one.
func LeaderEpoch(ctx context.Context) (epoch int64, ok bool) {
	epoch, ok = ctx.Value(leaderEpochKey{}).(int64)
	return epoch, ok
}
