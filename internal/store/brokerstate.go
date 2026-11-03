// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"time"
)

// BrokerStateStore records which broker a database last ran against, so the
// server can notice at boot that the database has moved to a different one.
type BrokerStateStore interface {
	// GetBrokerIdentity returns the recorded identity, or "" and a nil error
	// when none has ever been recorded.
	GetBrokerIdentity(ctx context.Context) (string, error)
	// SetBrokerIdentity records identity as of at, replacing any earlier one.
	// The database holds at most one identity.
	SetBrokerIdentity(ctx context.Context, identity string, at time.Time) error
}
