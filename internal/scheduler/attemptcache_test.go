// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"strconv"
	"sync"
	"testing"
)

func TestAttemptOwnerCache_PutGetEvict(t *testing.T) {
	c := newAttemptOwnerCache()

	if _, ok := c.get("missing"); ok {
		t.Fatal("expected miss on empty cache")
	}

	c.put("a1", "worker-1", "task-1")
	got, ok := c.get("a1")
	if !ok {
		t.Fatal("expected hit after put")
	}
	if got.workerID != "worker-1" || got.taskID != "task-1" {
		t.Errorf("got %+v, want {worker-1 task-1}", got)
	}

	c.evict("a1")
	if _, ok := c.get("a1"); ok {
		t.Error("expected miss after evict")
	}

	// Evicting a key that was never present must not panic.
	c.evict("never-there")
}

func TestAttemptOwnerCache_BoundedUnderContinuousGrowth(t *testing.T) {
	c := newAttemptOwnerCache()

	// Simulate a server that never observes a terminal status: put far more
	// entries than the cap without ever calling evict.
	for i := range maxAttemptOwnerCacheEntries * 3 {
		id := strconv.Itoa(i)
		c.put(id, "worker-1", "task-"+id)
	}

	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	if n > maxAttemptOwnerCacheEntries {
		t.Errorf("cache grew to %d entries, want <= %d", n, maxAttemptOwnerCacheEntries)
	}
}

func TestAttemptOwnerCache_ConcurrentAccess(_ *testing.T) {
	c := newAttemptOwnerCache()
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := strconv.Itoa(i % 10)
			c.put(id, "worker-1", "task-1")
			c.get(id)
			if i%3 == 0 {
				c.evict(id)
			}
		}(i)
	}
	wg.Wait()
}
