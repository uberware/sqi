// SPDX-License-Identifier: AGPL-3.0-or-later

// Package restore puts a backup in place and makes the farm safe to resume
// from it: every attempt the restored database still records as open is
// handed back (the fence), and the JetStream directory, whose messages belong
// to a different moment, is moved aside. See docs/operations.md, Restore.
package restore

import (
	"context"
	"fmt"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// farFuture is a cutoff after every attempt's creation.
var farFuture = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// Fence hands back every open attempt as a worker shutdown does: the task
// returns to ready, the attempt closes as failed (worker shutdown), its claims
// are released, and no retry or job failure is counted. It returns how many it
// reclaimed. It must run with the single-server lock held, so nothing can open
// an attempt meanwhile; it fails if any attempt is still open afterwards.
func Fence(ctx context.Context, st store.Store, now time.Time) (int, error) {
	open, err := st.ListOpenAttempts(ctx, farFuture)
	if err != nil {
		return 0, fmt.Errorf("restore: list open attempts: %w", err)
	}
	n := 0
	for _, a := range open {
		ok, err := st.ReclaimTaskAttempt(ctx, a.AttemptID, a.TaskID, now)
		if err != nil {
			return n, fmt.Errorf("restore: reclaim attempt %s of task %s: %w", a.AttemptID, a.TaskID, err)
		}
		if ok {
			n++
		}
	}
	left, err := st.ListOpenAttempts(ctx, farFuture)
	if err != nil {
		return n, fmt.Errorf("restore: re-check open attempts: %w", err)
	}
	if len(left) > 0 {
		return n, fmt.Errorf("restore: %d attempts still open after the fence (first %s); is a server running?",
			len(left), left[0].AttemptID)
	}
	return n, nil
}
