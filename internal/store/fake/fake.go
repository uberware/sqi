// SPDX-License-Identifier: AGPL-3.0-only

package fake

import (
	"sync"

	"github.com/uberware/sqi/internal/store"
)

// Store is an in-memory implementation of [store.Store] for unit tests that
// must avoid touching the filesystem.
type Store struct {
	mu               sync.Mutex
	farms            map[string]store.Farm
	queues           map[string]store.Queue
	storageLocations map[string]store.StorageLocation
	licensePools     map[string]store.LicensePool
	licenseCheckouts map[string]store.LicenseCheckout
	workers          map[string]store.Worker
	jobs             map[string]store.Job
	steps            map[string]store.Step
	tasks            map[string]store.Task
	taskAttempts     map[string]store.TaskAttempt
	auditEntries     []store.AuditEntry
}

var _ store.Store = (*Store)(nil)

// New returns a ready-to-use in-memory store.
func New() *Store {
	return &Store{
		farms:            make(map[string]store.Farm),
		queues:           make(map[string]store.Queue),
		storageLocations: make(map[string]store.StorageLocation),
		licensePools:     make(map[string]store.LicensePool),
		licenseCheckouts: make(map[string]store.LicenseCheckout),
		workers:          make(map[string]store.Worker),
		jobs:             make(map[string]store.Job),
		steps:            make(map[string]store.Step),
		tasks:            make(map[string]store.Task),
		taskAttempts:     make(map[string]store.TaskAttempt),
		auditEntries:     make([]store.AuditEntry, 0),
	}
}

// Close implements io.Closer; it is a no-op for the in-memory store.
func (*Store) Close() error {
	return nil
}

// Reset reinitializes all maps to empty. Convenience helper for tests that call
// t.Cleanup(s.Reset) rather than constructing a new fake per sub-test.
func (s *Store) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.farms = make(map[string]store.Farm)
	s.queues = make(map[string]store.Queue)
	s.storageLocations = make(map[string]store.StorageLocation)
	s.licensePools = make(map[string]store.LicensePool)
	s.licenseCheckouts = make(map[string]store.LicenseCheckout)
	s.workers = make(map[string]store.Worker)
	s.jobs = make(map[string]store.Job)
	s.steps = make(map[string]store.Step)
	s.tasks = make(map[string]store.Task)
	s.taskAttempts = make(map[string]store.TaskAttempt)
	s.auditEntries = make([]store.AuditEntry, 0)
}
