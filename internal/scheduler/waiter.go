// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"sync"
	"time"
)

// waiterRegistry parks long-poll lease requests per queue and wakes them when
// new work may be available. A waiter registers a buffered channel; notify
// closes and clears the current set for a queue so every parked request wakes
// exactly once. Safe for concurrent use.
type waiterRegistry struct {
	mu      sync.Mutex
	waiters map[string]map[chan struct{}]struct{}
}

func newWaiterRegistry() *waiterRegistry {
	return &waiterRegistry{waiters: make(map[string]map[chan struct{}]struct{})}
}

// wait blocks until notify(queueID) fires, ctx is done, or timeout elapses.
// Returns true only when woken by notify.
func (r *waiterRegistry) wait(ctx context.Context, queueID string, timeout time.Duration) bool {
	ch := make(chan struct{}, 1)
	r.mu.Lock()
	set := r.waiters[queueID]
	if set == nil {
		set = make(map[chan struct{}]struct{})
		r.waiters[queueID] = set
	}
	set[ch] = struct{}{}
	r.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ch:
		return true
	case <-ctx.Done():
		r.remove(queueID, ch)
		return false
	case <-timer.C:
		r.remove(queueID, ch)
		return false
	}
}

func (r *waiterRegistry) remove(queueID string, ch chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if set := r.waiters[queueID]; set != nil {
		delete(set, ch)
	}
}

// notify wakes every parked waiter for queueID.
func (r *waiterRegistry) notify(queueID string) {
	r.mu.Lock()
	set := r.waiters[queueID]
	r.waiters[queueID] = nil
	r.mu.Unlock()
	for ch := range set {
		close(ch)
	}
}
