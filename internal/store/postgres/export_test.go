// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"database/sql"
	"time"

	"github.com/uberware/sqi/internal/store/sqlstore"
)

// LockAnchors exposes the dialect's anchor locking to the external tests,
// which need storetest (and storetest imports this package).
func LockAnchors(ctx context.Context, tx *sql.Tx, anchors ...sqlstore.Anchor) error {
	return pgDialect{}.LockAnchors(ctx, tx, anchors...)
}

// ClockMicros and ForShare expose the dialect's fragments to the external
// tests.
func ClockMicros() string { return pgDialect{}.ClockMicros() }

func ForShare() string { return pgDialect{}.ForShare() }

// SetHALockWaitForTest shortens how long an HA server waits for the database
// lock to be released, and returns the function that restores it.
func SetHALockWaitForTest(d time.Duration) (restore func()) {
	old := haLockWait
	haLockWait = d
	return func() { haLockWait = old }
}

// SetReleaseTimeoutForTest changes how long releasing the server lock may
// take, and returns the function that restores it.
func SetReleaseTimeoutForTest(d time.Duration) (restore func()) {
	old := releaseTimeout
	releaseTimeout = d
	return func() { releaseTimeout = old }
}

// WriteDBForTest, ReadDBForTest and LeaseDBForTest expose the store's pools so
// a test can look at the sessions they hold. LeaseDBForTest is nil without HA.
func (s *Store) WriteDBForTest() *sql.DB { return s.db }

func (s *Store) ReadDBForTest() *sql.DB { return s.rdb }

func (s *Store) LeaseDBForTest() *sql.DB { return s.leaseDB }
