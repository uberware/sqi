// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"fmt"

	"github.com/uberware/sqi/internal/store"
)

// fenceLocked refuses a write made under a leadership context
// ([store.WithLeaderEpoch]) whose epoch is no longer the lease's, with
// [store.ErrFenced]. A context without an epoch is never refused. Every
// mutating method calls it first, before it changes anything, so a refused
// write leaves the store as it was, as the SQL stores' rollback does. The
// leadership methods do not call it. The caller holds s.mu.
func (s *Store) fenceLocked(ctx context.Context) error {
	epoch, ok := store.LeaderEpoch(ctx)
	if !ok || epoch == s.leadership.epoch {
		return nil
	}
	return fmt.Errorf("store: fenced at epoch %d, the lease is at %d: %w", epoch, s.leadership.epoch, store.ErrFenced)
}
