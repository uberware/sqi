// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite_test

import (
	"context"
	"errors"
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

// TestCreateQueue_RunAsGroupWithoutRunAsUser_Rejected proves the real SQLite
// store refuses to persist a queue whose run_as_group is set with no
// run_as_user — that combination selects no OS identity at all (the
// scheduler only gates isolation on RunAsUser), so it would otherwise be
// stored and silently ignored.
func TestCreateQueue_RunAsGroupWithoutRunAsUser_Rejected(t *testing.T) {
	db := t.TempDir() + "/test.db"
	s := newTestStore(t, db)
	ctx := context.Background()
	farm := createTestFarm(t, s)

	group := "render"
	_, err := s.CreateQueue(ctx, store.Queue{ID: "q1", FarmID: farm.ID, Name: "q1", RunAsGroup: &group})
	if !errors.Is(err, store.ErrRunAsGroupWithoutUser) {
		t.Fatalf("err = %v, want store.ErrRunAsGroupWithoutUser", err)
	}
	if _, getErr := s.GetQueue(ctx, "q1"); !errors.Is(getErr, store.ErrNotFound) {
		t.Errorf("rejected create must not have persisted a row: GetQueue err = %v", getErr)
	}
}

// TestUpdateQueue_ClearingRunAsUserWhileGroupPreserved_Rejected proves the
// invalid-combo check runs against the RESOLVED post-write values (after the
// UPDATE statement's own CASE WHEN preserve substitution), not the raw
// request: a PUT clearing run_as_user while omitting run_as_group (preserved
// from an existing non-empty value) must be rejected, and the transaction
// must roll back rather than leave a partially-applied row.
func TestUpdateQueue_ClearingRunAsUserWhileGroupPreserved_Rejected(t *testing.T) {
	db := t.TempDir() + "/test.db"
	s := newTestStore(t, db)
	ctx := context.Background()
	farm := createTestFarm(t, s)

	user, group := "render-svc", "render"
	if _, err := s.CreateQueue(ctx, store.Queue{
		ID: "q1", FarmID: farm.ID, Name: "q1", RunAsUser: &user, RunAsGroup: &group,
	}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}

	_, err := s.UpdateQueue(ctx, store.Queue{
		ID: "q1", FarmID: farm.ID, Name: "q1",
		RunAsUser:          nil,
		PreserveRunAsGroup: true,
	})
	if !errors.Is(err, store.ErrRunAsGroupWithoutUser) {
		t.Fatalf("err = %v, want store.ErrRunAsGroupWithoutUser", err)
	}

	got, getErr := s.GetQueue(ctx, "q1")
	if getErr != nil {
		t.Fatalf("GetQueue: %v", getErr)
	}
	if got.RunAsUser == nil || *got.RunAsUser != "render-svc" {
		t.Errorf("run_as_user = %v, want render-svc (rejected update must roll back)", got.RunAsUser)
	}
	if got.RunAsGroup == nil || *got.RunAsGroup != "render" {
		t.Errorf("run_as_group = %v, want render (rejected update must roll back)", got.RunAsGroup)
	}
}

// TestUpdateQueue_SettingRunAsGroupWhileUserPreservedEmpty_Rejected is the
// mirror case: setting run_as_group on a queue whose run_as_user was never
// set (and is preserved as nil) must be rejected too.
func TestUpdateQueue_SettingRunAsGroupWhileUserPreservedEmpty_Rejected(t *testing.T) {
	db := t.TempDir() + "/test.db"
	s := newTestStore(t, db)
	ctx := context.Background()
	farm := createTestFarm(t, s)

	if _, err := s.CreateQueue(ctx, store.Queue{ID: "q1", FarmID: farm.ID, Name: "q1"}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}

	group := "render"
	_, err := s.UpdateQueue(ctx, store.Queue{
		ID: "q1", FarmID: farm.ID, Name: "q1",
		RunAsGroup:        &group,
		PreserveRunAsUser: true,
	})
	if !errors.Is(err, store.ErrRunAsGroupWithoutUser) {
		t.Fatalf("err = %v, want store.ErrRunAsGroupWithoutUser", err)
	}

	got, getErr := s.GetQueue(ctx, "q1")
	if getErr != nil {
		t.Fatalf("GetQueue: %v", getErr)
	}
	if got.RunAsGroup != nil {
		t.Errorf("run_as_group = %v, want nil (rejected update must roll back)", *got.RunAsGroup)
	}
}

// TestUpdateQueue_ConcurrentPreserveAndSet_NoLostUpdate pins that an
// isolation-omitting update cannot erase a concurrent isolation-setting one.
// Preserving run_as_user by reading the queue (GetQueue) and writing the full
// row back (UpdateQueue) as two separate store calls leaves a gap: a
// concurrent isolation-setting PUT (an admin's) can commit between that read
// and that write, and the omitting request then writes back the stale nil it
// read, erasing the admin's write with no error and no audit trace.
// PreserveRunAsUser/PreserveRunAsGroup instead let an "omitting" update leave
// the column untouched inside the same atomic UPDATE statement (a
// self-referential CASE WHEN) that writes everything else, so there is no
// window in which a stale value can be captured.
//
// The test runs many concurrent iterations, each pairing an isolation-omitting
// update (PreserveRunAsUser/Group: true, as in an operator's ordinary PUT)
// against an isolation-setting update (an admin's PUT setting run_as_user for
// the first time) on a queue that starts unisolated, and confirms the set
// value is never lost regardless of which goroutine's statement the SQLite
// driver executes first. It runs against the real store (not the fake), whose
// single mutex serializes at a granularity that can never exhibit a real
// read/write interleaving, and under -race, which also confirms the store
// methods themselves are concurrency-safe. If PreserveRunAsUser's CASE WHEN
// branches were reversed (preserving would become clearing), this test would
// catch it whenever the preserving goroutine's statement lands after the
// setting goroutine's commit.
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
