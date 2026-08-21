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

// TestAttemptOwnerCache_ConcurrentAccess drives put/get/evict concurrently
// under -race, then checks a real final invariant rather than just surviving
// without a panic or a race report: every id gets its own unique key (no two
// goroutines contend for the same entry), so the final state is fully
// determined by each goroutine's own i%3==0 evict decision. That lets the
// assertion also catch a put/get keying mistake — e.g. get reading back a
// neighboring entry's taskID — which a mere "did it panic" check cannot.
func TestAttemptOwnerCache_ConcurrentAccess(t *testing.T) {
	c := newAttemptOwnerCache()
	const n = 50
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := strconv.Itoa(i)
			c.put(id, "worker-1", "task-"+id)
			c.get(id)
			if i%3 == 0 {
				c.evict(id)
			}
		}(i)
	}
	wg.Wait()

	for i := range n {
		id := strconv.Itoa(i)
		got, ok := c.get(id)
		if i%3 == 0 {
			if ok {
				t.Errorf("id %s: expected evict to have removed the entry, got %+v", id, got)
			}
			continue
		}
		if !ok {
			t.Errorf("id %s: expected entry to remain present", id)
			continue
		}
		wantTaskID := "task-" + id
		if got.taskID != wantTaskID {
			t.Errorf("id %s: taskID = %q, want %q (own key, not a neighbor's)", id, got.taskID, wantTaskID)
		}
	}
}
