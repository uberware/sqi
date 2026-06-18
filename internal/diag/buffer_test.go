// SPDX-License-Identifier: AGPL-3.0-or-later

package diag_test

import (
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/diag"
)

func rec(comp, level, msg string, attrs map[string]string) diag.Record {
	return diag.Record{Ts: time.Now().UTC(), Component: comp, Level: level, Msg: msg, Attrs: attrs}
}

func TestBuffer_EvictsOldestPerComponent(t *testing.T) {
	b := diag.NewBuffer(2, nil)
	b.Append(rec("server", "INFO", "a", nil))
	b.Append(rec("server", "INFO", "b", nil))
	b.Append(rec("server", "INFO", "c", nil))

	got := b.Query(diag.Filter{Component: "server", Limit: 10})
	if len(got) != 2 || got[0].Msg != "b" || got[1].Msg != "c" {
		t.Fatalf("eviction wrong: %+v", got)
	}
}

func TestBuffer_KeysPerComponent(t *testing.T) {
	b := diag.NewBuffer(10, nil)
	b.Append(rec("server", "INFO", "s1", nil))
	b.Append(rec("worker:w1", "INFO", "w1msg", nil))

	if got := b.Query(diag.Filter{Component: "worker:w1", Limit: 10}); len(got) != 1 || got[0].Msg != "w1msg" {
		t.Fatalf("component keying wrong: %+v", got)
	}
	if got := b.Query(diag.Filter{Limit: 10}); len(got) != 2 {
		t.Fatalf("unfiltered query should return all components: %+v", got)
	}
}

func TestBuffer_FiltersByLevelTaskIDAndSince(t *testing.T) {
	b := diag.NewBuffer(10, nil)
	old := diag.Record{Ts: time.Now().Add(-time.Hour), Component: "server", Level: "INFO", Msg: "old"}
	b.Append(old)
	b.Append(rec("server", "ERROR", "err", map[string]string{"task_id": "t1"}))
	b.Append(rec("server", "DEBUG", "dbg", map[string]string{"task_id": "t2"}))

	if got := b.Query(diag.Filter{MinLevel: "WARN", Limit: 10}); len(got) != 1 || got[0].Msg != "err" {
		t.Fatalf("level filter wrong: %+v", got)
	}
	if got := b.Query(diag.Filter{TaskID: "t1", Limit: 10}); len(got) != 1 || got[0].Msg != "err" {
		t.Fatalf("task_id filter wrong: %+v", got)
	}
	if got := b.Query(diag.Filter{Since: time.Now().Add(-time.Minute), Limit: 10}); len(got) != 2 {
		t.Fatalf("since filter wrong: %+v", got)
	}
}

func TestBuffer_LimitReturnsNewest(t *testing.T) {
	b := diag.NewBuffer(10, nil)
	for _, m := range []string{"1", "2", "3"} {
		b.Append(rec("server", "INFO", m, nil))
	}
	got := b.Query(diag.Filter{Component: "server", Limit: 2})
	if len(got) != 2 || got[0].Msg != "2" || got[1].Msg != "3" {
		t.Fatalf("limit should return newest in chronological order: %+v", got)
	}
}

func TestBuffer_AppendInvokesNotify(t *testing.T) {
	var mu sync.Mutex
	var seen []diag.Record
	b := diag.NewBuffer(10, func(r diag.Record) {
		mu.Lock()
		seen = append(seen, r)
		mu.Unlock()
	})
	b.Append(rec("server", "INFO", "x", nil))
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0].Msg != "x" {
		t.Fatalf("notify not invoked: %+v", seen)
	}
}

func TestBuffer_SetNotify(t *testing.T) {
	b := diag.NewBuffer(10, nil)
	var mu sync.Mutex
	var n int
	b.SetNotify(func(diag.Record) {
		mu.Lock()
		n++
		mu.Unlock()
	})
	b.Append(rec("server", "INFO", "x", nil))
	mu.Lock()
	defer mu.Unlock()
	if n != 1 {
		t.Fatalf("SetNotify callback not invoked: n=%d", n)
	}
}

func TestBuffer_ConcurrentAppend(t *testing.T) {
	b := diag.NewBuffer(100, nil)
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			b.Append(rec("server", "INFO", "x", nil))
		})
	}
	wg.Wait()
	if got := b.Query(diag.Filter{Component: "server", Limit: 1000}); len(got) != 50 {
		t.Fatalf("concurrent append lost records: got %d", len(got))
	}
}

func TestBuffer_EvictsLeastRecentComponentBeyondGlobalCap(t *testing.T) {
	b := diag.NewBuffer(10, nil)
	b.SetMaxComponents(2)

	now := time.Now()
	b.Append(diag.Record{Ts: now.Add(-3 * time.Minute), Component: "worker:a", Level: "INFO", Msg: "a"})
	b.Append(diag.Record{Ts: now.Add(-2 * time.Minute), Component: "worker:b", Level: "INFO", Msg: "b"})
	b.Append(diag.Record{Ts: now.Add(-1 * time.Minute), Component: "worker:c", Level: "INFO", Msg: "c"})

	// With cap 2, the oldest-activity component ("worker:a") must be evicted.
	if got := b.Query(diag.Filter{Component: "worker:a", Limit: 10}); len(got) != 0 {
		t.Fatalf("worker:a should have been evicted, got %d", len(got))
	}
	if got := b.Query(diag.Filter{Component: "worker:c", Limit: 10}); len(got) != 1 {
		t.Fatalf("worker:c should be retained, got %d", len(got))
	}
}

func TestBuffer_NeverEvictsServerComponent(t *testing.T) {
	b := diag.NewBuffer(10, nil)
	b.SetMaxComponents(1)
	now := time.Now()
	// server logged first (oldest activity) — must still survive eviction.
	b.Append(diag.Record{Ts: now.Add(-5 * time.Minute), Component: "server", Level: "INFO", Msg: "srv"})
	b.Append(diag.Record{Ts: now.Add(-1 * time.Minute), Component: "worker:x", Level: "INFO", Msg: "x"})

	if got := b.Query(diag.Filter{Component: "server", Limit: 10}); len(got) != 1 {
		t.Fatalf("server must never be evicted, got %d", len(got))
	}
}
