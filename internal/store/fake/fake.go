// SPDX-License-Identifier: AGPL-3.0-or-later

// Package fake provides an in-memory implementation of [store.Store] for unit
// tests that must avoid touching the filesystem.
package fake

import (
	"sync"

	"github.com/uberware/sqi/internal/store"
)

// Store is an in-memory implementation of [store.Store] for unit tests that
// must avoid touching the filesystem.
type Store struct {
	mu                sync.Mutex
	farms             map[string]store.Farm
	queues            map[string]store.Queue
	storageLocations  map[string]store.StorageLocation
	computeLocations  map[string]store.ComputeLocation
	products          map[string]store.Product
	usagePools        map[string]store.UsagePool
	usageClaims       map[string]store.UsageClaim
	workers           map[string]store.Worker
	jobs              map[string]store.Job
	jobDependencies   map[string][]string // jobID -> upstream IDs (insertion order)
	steps             map[string]store.Step
	tasks             map[string]store.Task
	taskAttempts      map[string]store.TaskAttempt
	taskLogs          []store.TaskLog
	auditEntries      []store.AuditEntry
	users             map[string]store.User
	sessions          map[string]store.Session
	apiKeys           map[string]store.APIKey
	workerCredentials map[string]store.WorkerCredential
	workerJoinTokens  map[string]store.WorkerJoinToken
}

var _ store.Store = (*Store)(nil)

// New returns a ready-to-use in-memory store.
func New() *Store {
	return &Store{
		farms:             make(map[string]store.Farm),
		queues:            make(map[string]store.Queue),
		storageLocations:  make(map[string]store.StorageLocation),
		computeLocations:  make(map[string]store.ComputeLocation),
		products:          make(map[string]store.Product),
		usagePools:        make(map[string]store.UsagePool),
		usageClaims:       make(map[string]store.UsageClaim),
		workers:           make(map[string]store.Worker),
		jobs:              make(map[string]store.Job),
		jobDependencies:   make(map[string][]string),
		steps:             make(map[string]store.Step),
		tasks:             make(map[string]store.Task),
		taskAttempts:      make(map[string]store.TaskAttempt),
		taskLogs:          make([]store.TaskLog, 0),
		auditEntries:      make([]store.AuditEntry, 0),
		users:             make(map[string]store.User),
		sessions:          make(map[string]store.Session),
		apiKeys:           make(map[string]store.APIKey),
		workerCredentials: make(map[string]store.WorkerCredential),
		workerJoinTokens:  make(map[string]store.WorkerJoinToken),
	}
}

// Close implements io.Closer; it is a no-op for the in-memory store.
func (*Store) Close() error {
	return nil
}
