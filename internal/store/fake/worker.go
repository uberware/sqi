// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// RegisterWorker implements [store.WorkerStore]: it inserts or replaces the
// worker record for the given ID, except that an empty InstanceID keeps the
// stored one and Disabled is never taken from the registration: a new worker
// is enabled and an existing one keeps its flag (H4a2 §5.3), as SQLite's
// upsert leaves the column alone. A non-empty stored InstanceID that differs
// from a non-empty incoming one is a restarted worker process, whose assigned
// and running tasks are reclaimed as the offline transitions reclaim them, with
// [store.FailureReasonWorkerRestarted]; the store lock stands in for SQLite's
// worker-row and job-row anchors.
func (s *Store) RegisterWorker(_ context.Context, worker store.Worker) (store.Worker, []store.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, isUpdate := s.workers[worker.ID]
	worker.Tags = copyMap(worker.Tags)
	worker.Disabled = existing.Disabled // false for a new worker
	var reclaimed []store.Task
	if isUpdate {
		worker.RegisteredAt = existing.RegisteredAt
		if worker.InstanceID == "" {
			worker.InstanceID = existing.InstanceID
		} else if existing.InstanceID != "" && existing.InstanceID != worker.InstanceID {
			reclaimed = s.reclaimWorkerTasksLocked(worker.ID, store.FailureReasonWorkerRestarted, time.Now().UTC())
		}
	}
	s.workers[worker.ID] = worker
	return worker, reclaimed, nil
}

// GetWorker returns the worker with the given ID, or [store.ErrNotFound].
func (s *Store) GetWorker(_ context.Context, id string) (store.Worker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	worker, ok := s.workers[id]
	if !ok {
		return store.Worker{}, store.ErrNotFound
	}

	worker.Tags = copyMap(worker.Tags)
	return worker, nil
}

// ListWorkers returns a paginated, filtered, and sorted page of workers
// matching opts.
func (s *Store) ListWorkers(_ context.Context, opts store.ListWorkersOptions) (store.Page[store.Worker], error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := opts.Pagination.Validate(); err != nil {
		return store.Page[store.Worker]{}, err
	}

	workers := make([]store.Worker, 0, len(s.workers))
	for _, w := range s.workers {
		if filterWorker(w, opts) {
			w.Tags = copyMap(w.Tags)
			workers = append(workers, w)
		}
	}

	slices.SortStableFunc(workers, func(a, b store.Worker) int {
		return cmpWorker(a, b, opts.SortBy, opts.SortDir)
	})

	return applyPage(workers, opts.Pagination), nil
}

// UpdateWorker replaces the mutable capability fields of an existing worker
// (everything except ID, RegisteredAt, InstanceID and Disabled) and updates
// UpdatedAt. InstanceID and Disabled are kept as SQLite's UPDATE keeps them: an
// edit never changes which worker process the row belongs to, nor an
// operator's disable.
func (s *Store) UpdateWorker(_ context.Context, worker store.Worker) (store.Worker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.workers[worker.ID]
	if !ok {
		return store.Worker{}, store.ErrNotFound
	}

	worker.RegisteredAt = existing.RegisteredAt
	worker.InstanceID = existing.InstanceID
	worker.Disabled = existing.Disabled
	worker.UpdatedAt = time.Now()
	worker.Tags = copyMap(worker.Tags)
	s.workers[worker.ID] = worker
	return worker, nil
}

// SetWorkerDisabled implements [store.WorkerStore].
func (s *Store) SetWorkerDisabled(_ context.Context, id string, disabled bool) (store.Worker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	worker, ok := s.workers[id]
	if !ok {
		return store.Worker{}, store.ErrNotFound
	}

	worker.Disabled = disabled
	worker.UpdatedAt = time.Now().UTC()
	s.workers[id] = worker
	worker.Tags = copyMap(worker.Tags)
	return worker, nil
}

// UpdateWorkerHeartbeat records the most recent heartbeat time for the given worker.
func (s *Store) UpdateWorkerHeartbeat(_ context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	worker, ok := s.workers[id]
	if !ok {
		return store.ErrNotFound
	}

	worker.LastHeartbeatAt = &at
	worker.UpdatedAt = time.Now()
	s.workers[id] = worker
	return nil
}

// ListStaleWorkers returns workers whose last heartbeat is older than before
// and whose status is [store.WorkerStatusOnline], disabled or not (H4a2 §5.2).
//
// Unlike SQLite, where a NULL heartbeat never compares older, it also lists an
// online worker that has never sent a heartbeat; TestListStaleWorkers pins
// that. The list is only a candidate source: [Store.OfflineStaleWorker] applies
// SQLite's rule, so such a worker is never taken offline on either backend.
func (s *Store) ListStaleWorkers(_ context.Context, before time.Time) ([]store.Worker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var workers []store.Worker
	for _, w := range s.workers {
		if w.Status != store.WorkerStatusOnline {
			continue
		}
		if w.LastHeartbeatAt == nil || w.LastHeartbeatAt.Before(before) {
			w.Tags = copyMap(w.Tags)
			workers = append(workers, w)
		}
	}

	return workers, nil
}

// CountIdleWorkers returns the number of online, enabled workers in the given
// farm that have no task in [store.TaskStatusAssigned] or
// [store.TaskStatusRunning] state. When farmID is empty all farms are counted.
func (s *Store) CountIdleWorkers(_ context.Context, farmID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Build the set of worker IDs that have an active task.
	busy := make(map[string]struct{})
	for _, t := range s.tasks {
		if inFlightTask(t.Status) {
			busy[t.AssignedWorkerID] = struct{}{}
		}
	}

	var count int
	for _, w := range s.workers {
		if w.Status != store.WorkerStatusOnline || w.Disabled {
			continue
		}
		if farmID != "" && w.FarmID != farmID {
			continue
		}
		if _, isBusy := busy[w.ID]; !isBusy {
			count++
		}
	}
	return count, nil
}

// DeleteWorker hard-deletes the worker with the given ID.
func (s *Store) DeleteWorker(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.workers[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.workers, id)
	return nil
}

// DeleteWorkerIfRemovable implements [store.WorkerStore]. The rule is
// [store.Worker.Removable], which SQLite restates in its DELETE, plus the
// in-flight condition a Worker value cannot see (H4a2 §5.4).
func (s *Store) DeleteWorkerIfRemovable(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	w, ok := s.workers[id]
	if !ok {
		return store.ErrNotFound
	}
	if !w.Removable() || s.hasWorkInFlightLocked(id) {
		return store.ErrConflict
	}
	delete(s.workers, id)
	return nil
}

// hasWorkInFlightLocked reports whether any task is assigned to or running on
// workerID. Caller holds s.mu.
func (s *Store) hasWorkInFlightLocked(workerID string) bool {
	onWorker := inFlightOn(workerID)
	for _, t := range s.tasks {
		if onWorker(t) {
			return true
		}
	}
	return false
}

// DeleteOfflineWorkersBefore hard-deletes every enabled offline worker last
// seen before cutoff and returns the removed records. Online workers, disabled
// ones and workers that have never sent a heartbeat are left untouched.
func (s *Store) DeleteOfflineWorkersBefore(_ context.Context, cutoff time.Time) ([]store.Worker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var removed []store.Worker
	for id, w := range s.workers {
		if w.Status != store.WorkerStatusOffline || w.Disabled {
			continue
		}
		if w.LastHeartbeatAt == nil || !w.LastHeartbeatAt.Before(cutoff) {
			continue
		}
		w.Tags = copyMap(w.Tags)
		removed = append(removed, w)
		delete(s.workers, id)
	}
	return removed, nil
}

// filterWorker reports whether w matches all non-zero filter fields in opts.
func filterWorker(w store.Worker, opts store.ListWorkersOptions) bool {
	if opts.FarmID != "" && w.FarmID != opts.FarmID {
		if !opts.IncludeUnaffiliated || w.FarmID != "" {
			return false
		}
	}
	if opts.QueueID != "" && w.QueueID != opts.QueueID {
		return false
	}
	if opts.ComputeLocation != "" && w.ComputeLocation != opts.ComputeLocation {
		return false
	}
	if opts.Status != "" && w.EffectiveStatus() != opts.Status {
		return false
	}
	if opts.Search != "" && !workerMatchesSearch(w, opts.Search) {
		return false
	}
	return true
}

// workerMatchesSearch reports whether every whitespace-separated term in query
// matches (case-insensitive substring) one of w's name, hostname, id, or
// compute_location fields. Uses the shared matcher so it stays in lockstep with
// the SQLite LIKE search.
func workerMatchesSearch(w store.Worker, query string) bool {
	return store.MatchesSearch(query, w.Name, w.Hostname, w.ID, w.ComputeLocation)
}

// cmpWorker returns a comparison value for two workers by the given sort field
// and direction. An empty field defaults to [store.WorkerSortByHostname]; an
// empty direction defaults to ascending.
func cmpWorker(a, b store.Worker, field store.WorkerSortField, dir store.SortDir) int {
	var n int
	switch field {
	case store.WorkerSortByStatus:
		n = cmp.Compare(string(a.EffectiveStatus()), string(b.EffectiveStatus()))
	case store.WorkerSortByRegisteredAt:
		n = a.RegisteredAt.Compare(b.RegisteredAt)
	case store.WorkerSortByLastHeartbeatAt:
		var aT, bT time.Time
		if a.LastHeartbeatAt != nil {
			aT = *a.LastHeartbeatAt
		}
		if b.LastHeartbeatAt != nil {
			bT = *b.LastHeartbeatAt
		}
		n = aT.Compare(bT)
	default: // WorkerSortByHostname
		n = cmp.Compare(a.Hostname, b.Hostname)
	}
	if dir == store.SortDesc {
		return -n
	}
	return n
}
