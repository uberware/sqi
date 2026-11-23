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
//
// A request decides to park after reading the store, and a wake can land
// between that read and the park. notify only reaches waiters already
// registered, so the registry also counts the wakes on each key: a request
// takes a [waitTicket] before its read, and [waiterRegistry.waitSince] returns
// at once if a wake has landed since.
type waiterRegistry struct {
	mu      sync.Mutex
	waiters map[string]map[chan struct{}]struct{}
	wakes   map[string]uint64 // notify calls per key
	wakeAll uint64            // notifyAll calls
}

// waitTicket is the wake count a request saw before reading the store.
type waitTicket struct{ key, all uint64 }

func newWaiterRegistry() *waiterRegistry {
	return &waiterRegistry{
		waiters: make(map[string]map[chan struct{}]struct{}),
		wakes:   make(map[string]uint64),
	}
}

// ticket returns the wake count for queueID now. Take it before the store read
// that decides whether to park.
func (r *waiterRegistry) ticket(queueID string) waitTicket {
	r.mu.Lock()
	defer r.mu.Unlock()
	return waitTicket{key: r.wakes[queueID], all: r.wakeAll}
}

// wait is waitSince with a ticket taken now.
func (r *waiterRegistry) wait(ctx context.Context, queueID string, timeout time.Duration) bool {
	return r.waitSince(ctx, queueID, r.ticket(queueID), timeout)
}

// waitSince blocks until notify(queueID) fires, ctx is done, or timeout
// elapses. It returns at once, as woken, when a wake for queueID has landed
// since t was taken. Returns true only when woken.
func (r *waiterRegistry) waitSince(ctx context.Context, queueID string, t waitTicket, timeout time.Duration) bool {
	ch := make(chan struct{}, 1)
	r.mu.Lock()
	if r.wakes[queueID] != t.key || r.wakeAll != t.all {
		r.mu.Unlock()
		return true
	}
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
	r.wakes[queueID]++
	r.mu.Unlock()
	for ch := range set {
		close(ch)
	}
}

// notifyAll wakes every parked waiter on every queue. Used for infrequent bulk
// events (e.g. offline-worker reclaim) where the affected queues are not known;
// a broadcast is acceptable because a missed wake is only a leaseHoldTimeout
// latency issue.
func (r *waiterRegistry) notifyAll() {
	r.mu.Lock()
	sets := r.waiters
	r.waiters = make(map[string]map[chan struct{}]struct{})
	r.wakeAll++
	r.mu.Unlock()
	for _, set := range sets {
		for ch := range set {
			close(ch)
		}
	}
}
