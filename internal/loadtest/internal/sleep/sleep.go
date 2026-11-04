// SPDX-License-Identifier: AGPL-3.0-or-later

// Package sleep holds the load harness's context-aware sleeps.
package sleep

import (
	"context"
	"time"
)

// For sleeps for d or until ctx ends, and reports whether ctx is still live.
// A d of zero or less returns at once.
func For(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return ctx.Err() == nil
	}
}

// Until sleeps until t or until ctx ends, and reports whether ctx is still
// live. A t already past returns at once.
func Until(ctx context.Context, t time.Time) bool {
	return For(ctx, time.Until(t))
}
