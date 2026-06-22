// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// RegisterWorker inserts or replaces the worker record for the given ID.
// If the worker ID already exists its record is updated in full.
func (s *Store) RegisterWorker(_ context.Context, worker store.Worker) (store.Worker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, isUpdate := s.workers[worker.ID]

	worker.Tags = copyMap(worker.Tags)
	if isUpdate {
		worker.RegisteredAt = existing.RegisteredAt
	}

	s.workers[worker.ID] = worker
	return worker, nil
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
// (everything except ID and RegisteredAt) and updates UpdatedAt.
func (s *Store) UpdateWorker(_ context.Context, worker store.Worker) (store.Worker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.workers[worker.ID]
	if !ok {
		return store.Worker{}, store.ErrNotFound
	}

	worker.RegisteredAt = existing.RegisteredAt
	worker.UpdatedAt = time.Now()
	worker.Tags = copyMap(worker.Tags)
	s.workers[worker.ID] = worker
	return worker, nil
}

// UpdateWorkerStatus sets the status of the worker and updates UpdatedAt.
func (s *Store) UpdateWorkerStatus(_ context.Context, id string, status store.WorkerStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	worker, ok := s.workers[id]
	if !ok {
		return store.ErrNotFound
	}

	worker.Status = status
	worker.UpdatedAt = time.Now()
	s.workers[id] = worker
	return nil
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
// and whose status is [store.WorkerStatusOnline].
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

// CountIdleWorkers returns the number of online workers in the given farm that
// have no task in [store.TaskStatusAssigned] or [store.TaskStatusRunning] state.
// When farmID is empty all farms are counted.
func (s *Store) CountIdleWorkers(_ context.Context, farmID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Build the set of worker IDs that have an active task.
	busy := make(map[string]struct{})
	for _, t := range s.tasks {
		if t.Status == store.TaskStatusAssigned || t.Status == store.TaskStatusRunning {
			busy[t.AssignedWorkerID] = struct{}{}
		}
	}

	var count int
	for _, w := range s.workers {
		if w.Status != store.WorkerStatusOnline {
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

// DeleteOfflineWorkersBefore hard-deletes every offline worker last seen before
// cutoff and returns the removed records. Non-offline workers (including
// disabled) and workers that have never sent a heartbeat are left untouched.
func (s *Store) DeleteOfflineWorkersBefore(_ context.Context, cutoff time.Time) ([]store.Worker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var removed []store.Worker
	for id, w := range s.workers {
		if w.Status != store.WorkerStatusOffline {
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
	if opts.Status != "" && w.Status != opts.Status {
		return false
	}
	if opts.Search != "" && !workerMatchesSearch(w, opts.Search) {
		return false
	}
	return true
}

// workerMatchesSearch reports whether w matches query as a case-insensitive
// substring of name, hostname, id, or compute_location.
func workerMatchesSearch(w store.Worker, query string) bool {
	q := strings.ToLower(query)
	return strings.Contains(strings.ToLower(w.Name), q) ||
		strings.Contains(strings.ToLower(w.Hostname), q) ||
		strings.Contains(strings.ToLower(w.ID), q) ||
		strings.Contains(strings.ToLower(w.ComputeLocation), q)
}

// cmpWorker returns a comparison value for two workers by the given sort field
// and direction. An empty field defaults to [store.WorkerSortByHostname]; an
// empty direction defaults to ascending.
func cmpWorker(a, b store.Worker, field store.WorkerSortField, dir store.SortDir) int {
	var n int
	switch field {
	case store.WorkerSortByStatus:
		n = cmp.Compare(string(a.Status), string(b.Status))
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
