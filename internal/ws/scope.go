// SPDX-License-Identifier: AGPL-3.0-or-later

package ws

// Owner scoping for WebSocket delivery. A client registers with a Scope; every
// envelope carrying job ownership is tested against it before delivery, on both
// the live fan-out path and the ring-buffer replay path.

import (
	"strings"
	"sync"
	"time"
)

// Scope is a WebSocket client's job visibility. All=true means the client sees
// every job (it holds jobs.read.all, or auth is disabled); otherwise it sees
// only jobs whose owner matches Owner.
type Scope struct {
	Owner string
	All   bool
}

// allows reports whether a client with this scope may receive env.
//
// Envelopes that carry no job ownership (worker status, diagnostics, task logs
// — the latter gated once at subscribe time) always pass. Owner-scoped
// envelopes pass only on a case-insensitive owner match.
//
// An owner-scoped envelope whose owner could not be resolved has owner==""
// and is dropped for scoped clients, because "" matches no real username. That
// fail-closed default also hides pre-auth jobs (which have no owner) from
// scoped clients, matching the REST behavior.
func (s Scope) allows(env Envelope) bool {
	if s.All || !env.ownerScoped {
		return true
	}
	return env.owner != "" && strings.EqualFold(env.owner, s.Owner)
}

// ownerCache memoizes jobID → owner. Job ownership is immutable once a job is
// created, so entries never go stale and no TTL is needed. The map is cleared
// wholesale when it exceeds maxOwnerCache rather than evicting per-entry: the
// cost of a rare full reload is far below the bookkeeping of a true LRU, and
// the backing lookup is a single indexed read.
//
// The lookup distinguishes "this job definitively has no owner" (nil error,
// empty string — a job submitted before auth was enabled) from "the store
// could not answer" (non-nil error). Only the former is cached. That
// distinction is load-bearing: task events are the highest-frequency event in
// the system, so caching only non-empty owners would re-query the store on
// *every* task transition of *every* pre-auth job, forever, on the scheduler's
// goroutine. Caching the error case instead would be worse — a transient
// failure would pin the job as invisible for the process's lifetime.
//
// A failed lookup is instead suppressed for failureCooldown before being
// retried. Retrying on every event would let a degraded store stall the
// caller: get runs synchronously on the scheduler goroutine that dispatches
// task events, and each attempt can block for the resolver's timeout. Without
// a cooldown, N events during an outage cost N timeouts; with it they cost
// one per job per cooldown window, and resolution still recovers on its own
// once the store does.
type ownerCache struct {
	mu       sync.Mutex
	m        map[string]string
	failedAt map[string]time.Time
	lookup   func(jobID string) (string, error)
	now      func() time.Time
}

const (
	maxOwnerCache = 4096

	// failureCooldown is how long a failed resolution is suppressed. Short
	// enough that a brief store hiccup barely delays owner resolution, long
	// enough that a sustained outage cannot turn every task event into a
	// blocking store call.
	failureCooldown = 5 * time.Second
)

func newOwnerCache(lookup func(jobID string) (string, error)) *ownerCache {
	return &ownerCache{
		m:        make(map[string]string),
		failedAt: make(map[string]time.Time),
		lookup:   lookup,
		now:      time.Now,
	}
}

// get returns the owner of jobID, or "" when it has none, cannot be resolved,
// or resolution failed recently enough to still be in cooldown.
func (c *ownerCache) get(jobID string) string {
	if c == nil || c.lookup == nil || jobID == "" {
		return ""
	}
	c.mu.Lock()
	owner, ok := c.m[jobID]
	if !ok {
		if at, failed := c.failedAt[jobID]; failed && c.now().Sub(at) < failureCooldown {
			c.mu.Unlock()
			return "" // still in cooldown; don't touch the store
		}
	}
	c.mu.Unlock()
	if ok {
		return owner
	}

	owner, err := c.lookup(jobID)
	if err != nil {
		// Record the failure rather than the result: a transient store error
		// must not pin this job as invisible for the process's lifetime, but
		// it also must not be retried on every subsequent event.
		c.mu.Lock()
		if len(c.failedAt) >= maxOwnerCache {
			c.failedAt = make(map[string]time.Time, maxOwnerCache)
		}
		c.failedAt[jobID] = c.now()
		c.mu.Unlock()
		return ""
	}

	c.mu.Lock()
	if len(c.m) >= maxOwnerCache {
		c.m = make(map[string]string, maxOwnerCache)
	}
	c.m[jobID] = owner
	delete(c.failedAt, jobID)
	c.mu.Unlock()
	return owner
}
