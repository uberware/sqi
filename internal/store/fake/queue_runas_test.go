// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"errors"
	"testing"

	"github.com/uberware/sqi/internal/store"
)

// TestCreateQueue_RunAsGroupWithoutRunAsUser_Rejected proves the fake store
// enforces the same invariant the API handler and the SQLite store enforce:
// a queue.RunAsGroup with no queue.RunAsUser selects no OS identity at all
// (the scheduler only gates isolation on RunAsUser — see
// internal/scheduler/assign.go), so it must be refused rather than silently
// stored and ignored.
func TestCreateQueue_RunAsGroupWithoutRunAsUser_Rejected(t *testing.T) {
	s := New()
	defer func() { _ = s.Close() }()

	if _, err := s.CreateFarm(context.Background(), store.Farm{ID: "farm-1", Name: "f"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}

	group := "render"
	_, err := s.CreateQueue(context.Background(), store.Queue{
		ID: "q1", FarmID: "farm-1", Name: "q1", RunAsGroup: &group,
	})
	if !errors.Is(err, store.ErrRunAsGroupWithoutUser) {
		t.Fatalf("err = %v, want store.ErrRunAsGroupWithoutUser", err)
	}
	if _, getErr := s.GetQueue(context.Background(), "q1"); !errors.Is(getErr, store.ErrNotFound) {
		t.Errorf("rejected create must not have persisted a row: GetQueue err = %v", getErr)
	}
}

// TestUpdateQueue_ClearingRunAsUserWhileGroupPreserved_Rejected covers the
// case the request body alone cannot reveal: a PUT that clears run_as_user
// (explicit null) while omitting run_as_group entirely (preserved from the
// existing non-empty value) must be rejected exactly like setting both
// explicitly, because the RESOLVED state after the update would be
// group-without-user.
func TestUpdateQueue_ClearingRunAsUserWhileGroupPreserved_Rejected(t *testing.T) {
	s := New()
	defer func() { _ = s.Close() }()

	if _, err := s.CreateFarm(context.Background(), store.Farm{ID: "farm-1", Name: "f"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	user, group := "render-svc", "render"
	if _, err := s.CreateQueue(context.Background(), store.Queue{
		ID: "q1", FarmID: "farm-1", Name: "q1", RunAsUser: &user, RunAsGroup: &group,
	}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}

	_, err := s.UpdateQueue(context.Background(), store.Queue{
		ID: "q1", FarmID: "farm-1", Name: "q1",
		RunAsUser:          nil, // explicit clear
		PreserveRunAsGroup: true,
	})
	if !errors.Is(err, store.ErrRunAsGroupWithoutUser) {
		t.Fatalf("err = %v, want store.ErrRunAsGroupWithoutUser", err)
	}

	// The rejected update must not have partially applied.
	got, getErr := s.GetQueue(context.Background(), "q1")
	if getErr != nil {
		t.Fatalf("GetQueue: %v", getErr)
	}
	if got.RunAsUser == nil || *got.RunAsUser != "render-svc" {
		t.Errorf("run_as_user = %v, want render-svc (rejected update must not partially apply)", got.RunAsUser)
	}
	if got.RunAsGroup == nil || *got.RunAsGroup != "render" {
		t.Errorf("run_as_group = %v, want render (rejected update must not partially apply)", got.RunAsGroup)
	}
}

// TestUpdateQueue_SettingRunAsGroupWhileUserPreservedEmpty_Rejected covers
// the mirror case: setting run_as_group on a queue whose run_as_user was
// never set (and is therefore preserved as nil) must be rejected.
func TestUpdateQueue_SettingRunAsGroupWhileUserPreservedEmpty_Rejected(t *testing.T) {
	s := New()
	defer func() { _ = s.Close() }()

	if _, err := s.CreateFarm(context.Background(), store.Farm{ID: "farm-1", Name: "f"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if _, err := s.CreateQueue(context.Background(), store.Queue{ID: "q1", FarmID: "farm-1", Name: "q1"}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}

	group := "render"
	_, err := s.UpdateQueue(context.Background(), store.Queue{
		ID: "q1", FarmID: "farm-1", Name: "q1",
		RunAsGroup:        &group,
		PreserveRunAsUser: true, // stored run_as_user is nil
	})
	if !errors.Is(err, store.ErrRunAsGroupWithoutUser) {
		t.Fatalf("err = %v, want store.ErrRunAsGroupWithoutUser", err)
	}

	got, getErr := s.GetQueue(context.Background(), "q1")
	if getErr != nil {
		t.Fatalf("GetQueue: %v", getErr)
	}
	if got.RunAsGroup != nil {
		t.Errorf("run_as_group = %v, want nil (rejected update must not partially apply)", *got.RunAsGroup)
	}
}

// TestUpdateQueue_SettingRunAsGroupWhileUserPreservedSet_Allowed is the
// positive case: setting run_as_group while run_as_user is preserved from an
// ALREADY-set value is a legitimate update and must succeed.
func TestUpdateQueue_SettingRunAsGroupWhileUserPreservedSet_Allowed(t *testing.T) {
	s := New()
	defer func() { _ = s.Close() }()

	if _, err := s.CreateFarm(context.Background(), store.Farm{ID: "farm-1", Name: "f"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	user := "render-svc"
	if _, err := s.CreateQueue(context.Background(), store.Queue{
		ID: "q1", FarmID: "farm-1", Name: "q1", RunAsUser: &user,
	}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}

	group := "render"
	if _, err := s.UpdateQueue(context.Background(), store.Queue{
		ID: "q1", FarmID: "farm-1", Name: "q1",
		RunAsGroup:        &group,
		PreserveRunAsUser: true,
	}); err != nil {
		t.Fatalf("UpdateQueue: %v", err)
	}

	got, getErr := s.GetQueue(context.Background(), "q1")
	if getErr != nil {
		t.Fatalf("GetQueue: %v", getErr)
	}
	if got.RunAsUser == nil || *got.RunAsUser != "render-svc" {
		t.Errorf("run_as_user = %v, want render-svc", got.RunAsUser)
	}
	if got.RunAsGroup == nil || *got.RunAsGroup != "render" {
		t.Errorf("run_as_group = %v, want render", got.RunAsGroup)
	}
}
