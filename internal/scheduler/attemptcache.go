// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import "sync"

// maxAttemptOwnerCacheEntries bounds [attemptOwnerCache] independently of
// terminal-status eviction. Normal operation never approaches it — every
// attempt is evicted when it reaches a terminal status (see
// handleTaskTerminal and handleTaskFailed) — so this only guards against an
// attempt whose terminal status this process never observes (e.g. one still
// in flight across a server restart), which would otherwise accumulate for
// the life of the process.
const maxAttemptOwnerCacheEntries = 8192

// attemptOwner is the (workerID, taskID) pair recorded for a task attempt at
// creation time. Both fields are immutable for the life of the attempt: no
// store implementation ever updates worker_id or task_id on an existing
// task_attempts row, so caching them is safe for as long as the entry exists.
type attemptOwner struct {
	workerID string
	taskID   string
}

// attemptOwnerCache is a bounded, concurrency-safe cache of task-attempt
// ownership, consulted by handleLogChunk before it falls back to
// store.GetTaskAttempt. Log chunks arrive roughly every 500ms per running
// task, so caching the two fields handleLogChunk actually checks turns
// hundreds of repeated reads of the same immutable row into one.
type attemptOwnerCache struct {
	mu      sync.Mutex
	entries map[string]attemptOwner
}

// newAttemptOwnerCache returns an empty attemptOwnerCache.
func newAttemptOwnerCache() *attemptOwnerCache {
	return &attemptOwnerCache{entries: make(map[string]attemptOwner)}
}

// get returns the cached owner for attemptID, if present.
func (c *attemptOwnerCache) get(attemptID string) (attemptOwner, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	o, ok := c.entries[attemptID]
	return o, ok
}

// put records attemptID's owner. If the cache is already at capacity, one
// arbitrary existing entry is dropped first — Go map iteration order is
// randomized, so this is not LRU, but it only ever runs when
// maxAttemptOwnerCacheEntries has already been reached, which normal
// terminal-status eviction is designed to prevent.
func (c *attemptOwnerCache) put(attemptID, workerID, taskID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[attemptID]; !exists && len(c.entries) >= maxAttemptOwnerCacheEntries {
		for k := range c.entries {
			delete(c.entries, k)
			break
		}
	}
	c.entries[attemptID] = attemptOwner{workerID: workerID, taskID: taskID}
}

// evict removes attemptID's entry, if present. Called once an attempt
// reaches a terminal status, since it can never receive another log chunk
// after that.
func (c *attemptOwnerCache) evict(attemptID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, attemptID)
}
