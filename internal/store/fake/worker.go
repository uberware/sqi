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
// stored one and Disabled, CapacityGroupID and Ephemeral are never taken from
// the registration: a new worker is enabled and in no group, and an existing
// one keeps them, as the SQL upsert leaves those columns alone. Interfaces and
// CloudInstanceID are taken from it unless empty, when the stored ones are
// kept. The power state is decided as the SQL upsert decides it: a waking,
// asleep or wake-failed worker becomes awake (its time is now and its reason
// is cleared), a retiring one stays retiring, and a new worker is awake. A nil
// RegistrationBrokerAt keeps the stored one. A registration stamped earlier
// than the stored one ([store.StaleRegistration]) is refused with
// [store.ErrStaleRegistration], returning the stored worker. A non-empty stored
// InstanceID that differs from a non-empty incoming one is a restarted worker
// process, whose assigned and running tasks are reclaimed as the offline
// transitions reclaim them, with [store.FailureReasonWorkerRestarted]; the
// store lock stands in for the SQL stores' worker-row and job-row anchors.
func (s *Store) RegisterWorker(ctx context.Context, worker store.Worker) (store.Worker, store.Registration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return store.Worker{}, store.Registration{}, err
	}

	existing, isUpdate := s.workers[worker.ID]
	if isUpdate && store.StaleRegistration(existing, worker) {
		return cloneWorker(existing), store.Registration{}, store.ErrStaleRegistration
	}
	var reg store.Registration
	if isUpdate {
		var at *time.Time
		if existing.PowerStateAt != nil {
			t := *existing.PowerStateAt
			at = &t
		}
		reg.WokeFrom, reg.WokeFromAt = store.RegistrationWake(existing.PowerState, at)
	}
	worker = cloneWorker(worker)
	worker.Disabled = existing.Disabled                   // false for a new worker
	worker.CapacityGroupID = existing.CapacityGroupID     // empty for a new worker
	worker.Ephemeral = existing.Ephemeral                 // false for a new worker
	worker.HeartbeatBrokerAt = existing.HeartbeatBrokerAt // nil for a new worker
	worker.PowerState, worker.PowerStateAt, worker.PowerReason = registeredPowerState(existing, isUpdate)
	if isUpdate {
		worker.RegisteredAt = existing.RegisteredAt
		if worker.RegistrationBrokerAt == nil {
			worker.RegistrationBrokerAt = existing.RegistrationBrokerAt
		}
		if len(worker.Interfaces) == 0 {
			worker.Interfaces = cloneWorker(existing).Interfaces
		}
		if worker.CloudInstanceID == "" {
			worker.CloudInstanceID = existing.CloudInstanceID
		}
		if worker.InstanceID == "" {
			worker.InstanceID = existing.InstanceID
		} else if existing.InstanceID != "" && existing.InstanceID != worker.InstanceID {
			reg.Reclaimed = s.reclaimWorkerTasksLocked(worker.ID, store.FailureReasonWorkerRestarted, time.Now().UTC())
		}
	}
	s.workers[worker.ID] = worker
	return worker, reg, nil
}

// registeredPowerState is the power state a registration leaves a worker in,
// given the row it replaces: awake for a new worker, awake as of now for a
// waking, asleep or wake-failed one, awake for an empty state (with its time
// and reason kept), and unchanged otherwise.
func registeredPowerState(existing store.Worker, isUpdate bool) (store.PowerState, *time.Time, string) {
	if !isUpdate {
		return store.PowerAwake, nil, ""
	}
	switch existing.PowerState {
	case store.PowerWaking, store.PowerAsleep, store.PowerWakeFailed:
		at := time.Now().UTC()
		return store.PowerAwake, &at, ""
	case "":
		return store.PowerAwake, existing.PowerStateAt, existing.PowerReason
	default:
		return existing.PowerState, existing.PowerStateAt, existing.PowerReason
	}
}

// GetWorker returns the worker with the given ID, or [store.ErrNotFound].
func (s *Store) GetWorker(_ context.Context, id string) (store.Worker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	worker, ok := s.workers[id]
	if !ok {
		return store.Worker{}, store.ErrNotFound
	}

	worker = cloneWorker(worker)
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
			w = cloneWorker(w)
			workers = append(workers, w)
		}
	}

	slices.SortStableFunc(workers, func(a, b store.Worker) int {
		return cmpWorker(a, b, opts.SortBy, opts.SortDir)
	})

	return applyPage(workers, opts.Pagination), nil
}

// SetWorkerDisabled implements [store.WorkerStore].
func (s *Store) SetWorkerDisabled(ctx context.Context, id string, disabled bool) (store.Worker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return store.Worker{}, err
	}

	worker, ok := s.workers[id]
	if !ok {
		return store.Worker{}, store.ErrNotFound
	}

	worker.Disabled = disabled
	worker.UpdatedAt = time.Now().UTC()
	s.workers[id] = worker
	worker = cloneWorker(worker)
	return worker, nil
}

// SetWorkerCapacityGroup implements [store.WorkerStore].
func (s *Store) SetWorkerCapacityGroup(ctx context.Context, workerID, groupID string, ephemeral bool) (store.Worker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return store.Worker{}, err
	}

	worker, ok := s.workers[workerID]
	if !ok {
		return store.Worker{}, store.ErrNotFound
	}
	if groupID != "" {
		if _, ok := s.capacityGroups[groupID]; !ok {
			return store.Worker{}, store.ErrNotFound
		}
		if worker.CapacityGroupID != "" && worker.CapacityGroupID != groupID {
			return store.Worker{}, store.ErrConflict
		}
	}

	worker.CapacityGroupID = groupID
	worker.Ephemeral = ephemeral && groupID != ""
	if groupID == "" {
		// Power state belongs to the group: a worker outside one is awake.
		if worker.PowerState != store.PowerAwake {
			at := time.Now().UTC()
			worker.PowerStateAt = &at
		}
		worker.PowerState, worker.PowerReason = store.PowerAwake, ""
	}
	worker.UpdatedAt = time.Now().UTC()
	s.workers[workerID] = worker
	return cloneWorker(worker), nil
}

// ListCapacityGroupMembers implements [store.WorkerStore]: the group's workers
// by hostname, then ID.
func (s *Store) ListCapacityGroupMembers(_ context.Context, groupID string) ([]store.Worker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var members []store.Worker
	for _, w := range s.workers {
		if w.CapacityGroupID == groupID {
			members = append(members, cloneWorker(w))
		}
	}
	slices.SortFunc(members, func(a, b store.Worker) int {
		if c := cmp.Compare(a.Hostname, b.Hostname); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return members, nil
}

// DeleteEphemeralWorker implements [store.WorkerStore]: the worker goes only
// while it is ephemeral, offline and holds no assigned or running task, and its
// active credential is revoked at now with it.
func (s *Store) DeleteEphemeralWorker(ctx context.Context, id string, now time.Time) (store.Worker, bool, error) {
	return s.deleteEphemeralWorker(ctx, id, now, func(store.Worker) bool { return true })
}

// DeleteVanishedEphemeralWorker implements [store.WorkerStore]: as
// DeleteEphemeralWorker, for a worker that is also not disabled and was last
// seen before cutoff.
func (s *Store) DeleteVanishedEphemeralWorker(ctx context.Context, id string, cutoff, now time.Time) (store.Worker, bool, error) {
	return s.deleteEphemeralWorker(ctx, id, now, func(w store.Worker) bool {
		return !w.Disabled && w.LastHeartbeatAt != nil && w.LastHeartbeatAt.Before(cutoff)
	})
}

// deleteEphemeralWorker deletes worker id when it is ephemeral, offline, holds
// no work in flight and satisfies also, and revokes its active credential at
// now with it.
func (s *Store) deleteEphemeralWorker(
	ctx context.Context, id string, now time.Time, also func(store.Worker) bool,
) (store.Worker, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return store.Worker{}, false, err
	}
	w, deleted := s.deleteEphemeralWorkerLocked(id, now, also)
	return w, deleted, nil
}

// deleteEphemeralWorkerLocked is deleteEphemeralWorker under s.mu, which
// FinishRetirement shares. Caller holds s.mu.
func (s *Store) deleteEphemeralWorkerLocked(id string, now time.Time, also func(store.Worker) bool) (store.Worker, bool) {
	w, ok := s.workers[id]
	if !ok || !w.Ephemeral || w.Status != store.WorkerStatusOffline || !also(w) || s.hasWorkInFlightLocked(id) {
		return store.Worker{}, false
	}
	delete(s.workers, id)
	for credID, c := range s.workerCredentials {
		if c.WorkerID == id && c.RevokedAt == nil {
			at := now
			c.RevokedAt = &at
			s.workerCredentials[credID] = c
		}
	}
	return cloneWorker(w), true
}

// UpdateWorkerHeartbeat records the most recent heartbeat time for the given
// worker. A heartbeat the broker stamped before the recorded one is ignored; an
// unstamped one is recorded unconditionally and keeps the recorded stamp.
func (s *Store) UpdateWorkerHeartbeat(ctx context.Context, id string, at time.Time, brokerAt *time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return err
	}

	worker, ok := s.workers[id]
	if !ok {
		return store.ErrNotFound
	}

	if brokerAt != nil {
		if worker.HeartbeatBrokerAt != nil && brokerAt.Before(*worker.HeartbeatBrokerAt) {
			return nil
		}
		stamp := *brokerAt
		worker.HeartbeatBrokerAt = &stamp
	}
	worker.LastHeartbeatAt = &at
	worker.UpdatedAt = time.Now()
	s.workers[id] = worker
	return nil
}

// heartbeatBefore reports whether w's last heartbeat is strictly older than
// cutoff. A worker that has never sent a heartbeat is never older, matching
// the SQL stores, where `last_heartbeat_at < ?` is not true for NULL. The fake's
// staleness and retention checks all use it.
func heartbeatBefore(w store.Worker, cutoff time.Time) bool {
	return w.LastHeartbeatAt != nil && w.LastHeartbeatAt.Before(cutoff)
}

// ListStaleWorkers returns workers whose last heartbeat is older than before
// (see [heartbeatBefore]) and whose status is [store.WorkerStatusOnline],
// disabled or not.
func (s *Store) ListStaleWorkers(_ context.Context, before time.Time) ([]store.Worker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var workers []store.Worker
	for _, w := range s.workers {
		if w.Status != store.WorkerStatusOnline {
			continue
		}
		if heartbeatBefore(w, before) {
			w = cloneWorker(w)
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

// DeleteWorkerIfRemovable implements [store.WorkerStore]. The rule is
// [store.Worker.Removable], which the SQL stores restate in their DELETE, plus
// the in-flight condition a Worker value cannot see.
func (s *Store) DeleteWorkerIfRemovable(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return err
	}

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
// ones, workers that have never sent a heartbeat and every member of a capacity
// group are left untouched.
func (s *Store) DeleteOfflineWorkersBefore(ctx context.Context, cutoff time.Time) ([]store.Worker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return nil, err
	}

	var removed []store.Worker
	for id, w := range s.workers {
		if w.Status != store.WorkerStatusOffline || w.Disabled {
			continue
		}
		if w.CapacityGroupID != "" {
			continue
		}
		if !heartbeatBefore(w, cutoff) {
			continue
		}
		w = cloneWorker(w)
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
	if opts.CapacityGroupID != "" && w.CapacityGroupID != opts.CapacityGroupID {
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
// compute_location fields. Uses the shared matcher so it stays in step with
// the SQL LIKE search.
func workerMatchesSearch(w store.Worker, query string) bool {
	return store.MatchesSearch(query, w.Name, w.Hostname, w.ID, w.ComputeLocation)
}

// cmpWorker returns a comparison value for two workers by the given sort field
// and direction. An empty field defaults to [store.WorkerSortByHostname]; an
// empty direction defaults to ascending. Rows that tie on the field compare by
// ID ascending, as the SQL stores order them.
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
	return withIDTieBreak(n, dir, a.ID, b.ID)
}

// cloneWorker returns w with its map and slice fields copied, so a worker the
// fake returns or stores shares no backing storage with its caller.
func cloneWorker(w store.Worker) store.Worker {
	ifaces := w.Interfaces
	w.Tags = copyMap(w.Tags)
	w.Extensions = copySlice(w.Extensions)
	w.Interfaces = nil
	if len(ifaces) > 0 {
		w.Interfaces = make([]store.NetInterface, len(ifaces))
		for i, n := range ifaces {
			n.Addrs = copySlice(n.Addrs)
			w.Interfaces[i] = n
		}
	}
	return w
}
