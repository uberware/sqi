// SPDX-License-Identifier: AGPL-3.0-or-later

package ws

// Owner scoping for WebSocket delivery. A client registers with a Scope; every
// envelope carrying job ownership is tested against it before delivery, on both
// the live fan-out path and the ring-buffer replay path.

import (
	"strings"
	"sync"
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
type ownerCache struct {
	mu     sync.Mutex
	m      map[string]string
	lookup func(jobID string) (string, error)
}

const maxOwnerCache = 4096

func newOwnerCache(lookup func(jobID string) (string, error)) *ownerCache {
	return &ownerCache{m: make(map[string]string), lookup: lookup}
}

// get returns the owner of jobID, or "" when it has none or cannot be resolved.
func (c *ownerCache) get(jobID string) string {
	if c == nil || c.lookup == nil || jobID == "" {
		return ""
	}
	c.mu.Lock()
	owner, ok := c.m[jobID]
	c.mu.Unlock()
	if ok {
		return owner
	}

	owner, err := c.lookup(jobID)
	if err != nil {
		// Don't cache a failed resolution — a transient store error would
		// otherwise pin this job as invisible for the process's lifetime.
		return ""
	}

	c.mu.Lock()
	if len(c.m) >= maxOwnerCache {
		c.m = make(map[string]string, maxOwnerCache)
	}
	c.m[jobID] = owner
	c.mu.Unlock()
	return owner
}
