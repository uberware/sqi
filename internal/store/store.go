// SPDX-License-Identifier: AGPL-3.0-or-later

// Package store defines the storage interface for sqi-server and the domain
// model types shared across all components.
//
// # Design
//
// [Store] is a composite interface that embeds one sub-interface per aggregate
// (Farm, Queue, Worker, Job, Step, Task, etc.). Callers declare the narrowest
// interface they need rather than depending on the full Store:
//
//	// Scheduler only needs tasks and workers.
//	func NewScheduler(tasks store.TaskStore, workers store.WorkerStore) *Scheduler
//
//	// REST handler only needs jobs and queues.
//	func NewJobHandler(jobs store.JobStore, queues store.QueueStore) *JobHandler
//
// The SQLite-backed implementation implements Store in full.
// The in-memory fake also implements Store in full, used by unit
// tests that must not touch the filesystem.
//
// # JSON columns
//
// Fields that are stored as JSON in SQLite (e.g. Worker.Tags, Task.Parameters)
// are typed as Go maps or slices. The SQLite implementation is responsible for
// marshaling and unmarshaling; callers work with native Go types throughout.
//
// # Errors
//
// Methods return [ErrNotFound] when the requested row does not exist, and
// [ErrConflict] when a uniqueness constraint would be violated. All other
// errors are driver-level and should be treated as opaque.
package store

import (
	"errors"
	"io"
)

// Store is the top-level storage interface for sqi-server. It is composed of
// one sub-interface per aggregate so callers can depend on the narrowest slice
// they need.
//
// Store embeds [io.Closer]; implementations must release any open database
// connections or file handles when Close is called.
type Store interface {
	FarmStore
	QueueStore
	StorageLocationStore
	ComputeLocationStore
	ProductStore
	UsagePoolStore
	UsageClaimStore
	WorkerStore
	JobStore
	StepStore
	TaskStore
	TaskAttemptStore
	TaskLogStore
	AuditStore
	UserStore
	SessionStore
	APIKeyStore
	io.Closer
}

// ── Sentinel errors ───────────────────────────────────────────────────────────

// ErrNotFound is returned by Get* methods when the requested row does not
// exist. Use errors.Is to check:
//
//	job, err := store.GetJob(ctx, id)
//	if errors.Is(err, store.ErrNotFound) { ... }
var ErrNotFound = errors.New("store: not found")

// ErrConflict is returned when an insert or update would violate a uniqueness
// constraint (e.g. creating a farm with a name that already exists).
var ErrConflict = errors.New("store: conflict")
