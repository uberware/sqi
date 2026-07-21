// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
)

// newTestStore opens a SQLite store at path with auto-migration enabled and
// registers cleanup to close it when the test ends.
func newTestStore(t *testing.T, path string) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(context.Background(), path, sqlite.DefaultOptions())
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := s.Close(); closeErr != nil {
			t.Errorf("store.Close: %v", closeErr)
		}
	})
	return s
}

// createTestFarm inserts a minimal farm for queue tests to reference.
func createTestFarm(t *testing.T, s *sqlite.Store) store.Farm {
	t.Helper()
	f, err := s.CreateFarm(context.Background(), store.Farm{
		ID:   "farm-1",
		Name: "Test Farm",
	})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	return f
}

func TestQueueRunAsUserRoundTrip(t *testing.T) {
	db := t.TempDir() + "/test.db"
	s := newTestStore(t, db)
	ctx := context.Background()

	farm := createTestFarm(t, s)
	user := "render-svc"
	group := "render"

	created, err := s.CreateQueue(ctx, store.Queue{
		ID:         "queue-1",
		FarmID:     farm.ID,
		Name:       "isolated",
		RunAsUser:  &user,
		RunAsGroup: &group,
	})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	if created.RunAsUser == nil || *created.RunAsUser != "render-svc" {
		t.Errorf("RunAsUser = %v, want render-svc", created.RunAsUser)
	}

	got, err := s.GetQueue(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetQueue: %v", err)
	}
	if got.RunAsUser == nil || *got.RunAsUser != "render-svc" {
		t.Errorf("after reload RunAsUser = %v, want render-svc", got.RunAsUser)
	}
	if got.RunAsGroup == nil || *got.RunAsGroup != "render" {
		t.Errorf("after reload RunAsGroup = %v, want render", got.RunAsGroup)
	}
}

func TestQueueRunAsUserDefaultsNil(t *testing.T) {
	db := t.TempDir() + "/test.db"
	s := newTestStore(t, db)
	ctx := context.Background()

	farm := createTestFarm(t, s)
	created, err := s.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: farm.ID, Name: "plain"})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	if created.RunAsUser != nil {
		t.Errorf("RunAsUser = %v, want nil (no isolation)", *created.RunAsUser)
	}
}

// TestUpdateQueue_ConcurrentPreserveAndSet_NoLostUpdate is the concurrency
// regression test for the lost-update bug fixed alongside PreserveRunAsUser /
// PreserveRunAsGroup: the API layer used to preserve run_as_user across an
// isolation-omitting PUT by reading the existing queue (GetQueue) and then
// writing the full row back (UpdateQueue) as two separate store calls. A
// concurrent isolation-setting PUT (an admin's) could commit in the gap
// between that read and that write, and the omitting request would then
// write back the stale nil it had read, silently erasing the admin's write —
// with no error and no audit trace.
//
// The fix removes the read entirely. PreserveRunAsUser/PreserveRunAsGroup let
// an "omitting" update leave the column untouched inside the very same atomic
// UPDATE statement (a self-referential CASE WHEN) that writes everything
// else, so there is no window in which a stale value can be captured.
//
// A byte-for-byte reproduction of the original race is no longer possible:
// the buggy GetQueue-then-UpdateQueue orchestration lived entirely in
// internal/api/queues.go's resolveQueueIsolationFields, which this fix
// deletes — store.UpdateQueue itself was never the buggy half, so there is no
// "old" store call left to race against a "new" one. What this test asserts
// instead is the invariant the fix establishes, directly against the new
// single-statement path: run many genuinely concurrent iterations, each
// pairing an isolation-omitting update (PreserveRunAsUser/Group: true,
// mirroring an operator's ordinary PUT) against an isolation-setting update
// (mirroring an admin's PUT setting run_as_user for the first time) on a
// queue that starts unisolated, and confirm the set value is never silently
// lost regardless of which goroutine's statement the SQLite driver executes
// first. Run against the real store (not the fake) — the fake's single mutex
// serializes at a granularity that can never exhibit a real read/write
// interleaving — and under -race, which also confirms the store methods
// themselves are concurrency-safe. If PreserveRunAsUser's CASE WHEN branches
// were ever reversed (a plausible regression: preserving would silently
// become clearing), this test would catch it whenever the preserving
// goroutine's statement lands after the setting goroutine's commit.
func TestUpdateQueue_ConcurrentPreserveAndSet_NoLostUpdate(t *testing.T) {
	db := t.TempDir() + "/test.db"
	s := newTestStore(t, db)
	ctx := context.Background()
	farm := createTestFarm(t, s)

	const iterations = 50
	for i := range iterations {
		id := fmt.Sprintf("queue-%d", i)
		name := fmt.Sprintf("q-%d", i)
		if _, err := s.CreateQueue(ctx, store.Queue{ID: id, FarmID: farm.ID, Name: name}); err != nil {
			t.Fatalf("iteration %d: CreateQueue: %v", i, err)
		}

		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)

		// "Operator" update: omits run_as_user/run_as_group entirely (an
		// ordinary priority edit) — must preserve whatever the concurrent
		// admin write sets, never clobber it with a stale read.
		go func() {
			defer wg.Done()
			<-start
			if _, err := s.UpdateQueue(ctx, store.Queue{
				ID:                 id,
				FarmID:             farm.ID,
				Name:               name,
				Priority:           99,
				PreserveRunAsUser:  true,
				PreserveRunAsGroup: true,
			}); err != nil {
				t.Errorf("iteration %d: operator UpdateQueue: %v", i, err)
			}
		}()

		// "Admin" update: sets run_as_user for the first time.
		user := "render-svc"
		go func() {
			defer wg.Done()
			<-start
			if _, err := s.UpdateQueue(ctx, store.Queue{
				ID:        id,
				FarmID:    farm.ID,
				Name:      name,
				RunAsUser: &user,
			}); err != nil {
				t.Errorf("iteration %d: admin UpdateQueue: %v", i, err)
			}
		}()

		close(start)
		wg.Wait()

		got, err := s.GetQueue(ctx, id)
		if err != nil {
			t.Fatalf("iteration %d: GetQueue: %v", i, err)
		}
		if got.RunAsUser == nil || *got.RunAsUser != "render-svc" {
			t.Fatalf("iteration %d: run_as_user = %v, want render-svc (lost update)", i, got.RunAsUser)
		}
	}
}
