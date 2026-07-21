// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite_test

import (
	"context"
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
