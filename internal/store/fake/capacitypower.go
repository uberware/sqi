// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// ── CapacityPowerStore ────────────────────────────────────────────────────────
//
// The store lock stands in for the SQL stores' worker-row anchors: each
// transition decides and writes under it, as each SQL transition does under the
// rows it locks.
//
// An empty group ID names no group, as it matches no row in SQL, so the
// group-scoped transitions return before they look at a worker: a worker in
// no group has an empty CapacityGroupID too.

// setPowerLocked moves worker w to state at now with reason and stores it.
// Caller holds s.mu.
func (s *Store) setPowerLocked(w store.Worker, state store.PowerState, now time.Time, reason string) store.Worker {
	at := now.UTC()
	w.PowerState, w.PowerStateAt, w.PowerReason = state, &at, reason
	w.UpdatedAt = at
	s.workers[w.ID] = w
	return w
}

// MarkWaking implements [store.CapacityPowerStore].
func (s *Store) MarkWaking(ctx context.Context, groupID string, workerIDs []string, now time.Time) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return nil, err
	}
	if groupID == "" {
		return nil, nil
	}

	var moved []string
	for _, id := range slices.Compact(slices.Sorted(slices.Values(workerIDs))) {
		w, ok := s.workers[id]
		if !ok || w.CapacityGroupID != groupID || w.Status != store.WorkerStatusOffline || w.Disabled {
			continue
		}
		switch w.PowerState {
		case store.PowerAwake, store.PowerAsleep, store.PowerWakeFailed:
			s.setPowerLocked(w, store.PowerWaking, now, "")
			moved = append(moved, id)
		}
	}
	return moved, nil
}

// overdueMembersLocked returns the members of groupID in state since before
// cutoff, by ID. Caller holds s.mu.
func (s *Store) overdueMembersLocked(groupID string, state store.PowerState, cutoff time.Time) []store.Worker {
	var out []store.Worker
	for _, w := range s.workers {
		if w.CapacityGroupID == groupID && w.PowerState == state && w.PowerStateAt != nil && w.PowerStateAt.Before(cutoff) {
			out = append(out, w)
		}
	}
	slices.SortFunc(out, func(a, b store.Worker) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// FailWakes implements [store.CapacityPowerStore].
func (s *Store) FailWakes(ctx context.Context, groupID string, cutoff, now time.Time, reason string) ([]store.Worker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return nil, err
	}
	if groupID == "" {
		return nil, nil
	}

	var failed []store.Worker
	for _, w := range s.overdueMembersLocked(groupID, store.PowerWaking, cutoff) {
		failed = append(failed, cloneWorker(s.setPowerLocked(w, store.PowerWakeFailed, now, reason)))
	}
	return failed, nil
}

// BeginRetirement implements [store.CapacityPowerStore].
func (s *Store) BeginRetirement(ctx context.Context, groupID, workerID string, now time.Time) (store.Worker, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return store.Worker{}, false, err
	}

	w, ok := s.workers[workerID]
	if !ok || groupID == "" || w.CapacityGroupID != groupID || w.Status != store.WorkerStatusOnline || w.Disabled ||
		w.PowerState != store.PowerAwake || s.hasWorkInFlightLocked(workerID) {
		return store.Worker{}, false, nil
	}
	return cloneWorker(s.setPowerLocked(w, store.PowerRetiring, now, "")), true, nil
}

// FinishRetirement implements [store.CapacityPowerStore]: a named member is put
// to sleep and offline, and an ephemeral one is then deleted with its
// credential revoked, as DeleteEphemeralWorker deletes it.
func (s *Store) FinishRetirement(
	ctx context.Context, workerID string, now time.Time,
) (w store.Worker, deleted, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return store.Worker{}, false, false, err
	}

	current, known := s.workers[workerID]
	if !known || current.PowerState != store.PowerRetiring {
		return store.Worker{}, false, false, nil
	}
	if s.hasWorkInFlightLocked(workerID) {
		return store.Worker{}, false, false, fmt.Errorf("store: finish retirement of worker %s, which holds a task: %w",
			workerID, store.ErrConflict)
	}
	prior := current
	current.Status = store.WorkerStatusOffline
	asleep := s.setPowerLocked(current, store.PowerAsleep, now, "")
	if !asleep.Ephemeral {
		return cloneWorker(asleep), false, true, nil
	}
	gone, deleted := s.deleteEphemeralWorkerLocked(workerID, now, func(store.Worker) bool { return true })
	if !deleted {
		// The SQL stores roll the whole transition back.
		s.workers[workerID] = prior
		return store.Worker{}, false, false, fmt.Errorf("store: retired ephemeral worker %s was not deleted: %w",
			workerID, store.ErrConflict)
	}
	return gone, true, true, nil
}

// AbortRetirement implements [store.CapacityPowerStore].
func (s *Store) AbortRetirement(ctx context.Context, workerID string, now time.Time, reason string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return false, err
	}

	w, ok := s.workers[workerID]
	if !ok || w.PowerState != store.PowerRetiring {
		return false, nil
	}
	s.setPowerLocked(w, store.PowerAwake, now, reason)
	return true, nil
}

// ExpireRetirements implements [store.CapacityPowerStore].
func (s *Store) ExpireRetirements(ctx context.Context, groupID string, cutoff, now time.Time, reason string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return nil, err
	}
	if groupID == "" {
		return nil, nil
	}

	var expired []string
	for _, w := range s.overdueMembersLocked(groupID, store.PowerRetiring, cutoff) {
		s.setPowerLocked(w, store.PowerAwake, now, reason)
		expired = append(expired, w.ID)
	}
	return expired, nil
}

// storedGroupState copies st as the SQL stores keep it: times in UTC, and no
// pointer shared with the caller.
func storedGroupState(st store.CapacityGroupState) store.CapacityGroupState {
	utc := func(t *time.Time) *time.Time {
		if t == nil {
			return nil
		}
		v := t.UTC()
		return &v
	}
	st.RequestedAt, st.DegradedAt, st.BackoffUntil = utc(st.RequestedAt), utc(st.DegradedAt), utc(st.BackoffUntil)
	st.UpdatedAt = st.UpdatedAt.UTC()
	return st
}

// ListCapacityGroupStates implements [store.CapacityPowerStore].
func (s *Store) ListCapacityGroupStates(_ context.Context) (map[string]store.CapacityGroupState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make(map[string]store.CapacityGroupState, len(s.capacityStates))
	for id, st := range s.capacityStates {
		out[id] = storedGroupState(st)
	}
	return out, nil
}

// PutCapacityGroupState implements [store.CapacityPowerStore].
func (s *Store) PutCapacityGroupState(ctx context.Context, st store.CapacityGroupState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return err
	}

	if _, ok := s.capacityGroups[st.GroupID]; !ok {
		return fmt.Errorf("store: put state of capacity group %s: %w", st.GroupID, store.ErrNotFound)
	}
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = time.Now()
	}
	s.capacityStates[st.GroupID] = storedGroupState(st)
	return nil
}
