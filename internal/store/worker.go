// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"time"
)

// WorkerStatus is the operational state of a worker as known to the server.
type WorkerStatus string

const (
	// WorkerStatusOnline means the worker is connected and accepting tasks.
	WorkerStatusOnline WorkerStatus = "online"
	// WorkerStatusOffline means the worker has not sent a heartbeat within the
	// configured timeout and is presumed unreachable.
	WorkerStatusOffline WorkerStatus = "offline"
	// WorkerStatusDisabled means an operator has administratively paused the
	// worker; it will not receive new task assignments until re-enabled.
	WorkerStatusDisabled WorkerStatus = "disabled"
)

// GPUInfo describes the GPU(s) available on a worker host.
//
// Phase 1 assumes a homogeneous GPU configuration: all GPUs on the host are
// the same model with the same VRAM. VRAMMb is the per-device VRAM capacity;
// Count is the number of identical devices. Mixed-GPU workers (e.g. a render
// card alongside a display adapter) are not modeled — workers with
// heterogeneous GPUs should report the lowest common VRAM to avoid
// over-scheduling. A []GPUDevice slice should replace this struct when
// heterogeneous per-device tracking is required.
type GPUInfo struct {
	Vendor string `json:"vendor,omitempty"`
	Model  string `json:"model,omitempty"`
	// VRAMMb is the VRAM capacity of each GPU device in mebibytes.
	// All devices are assumed to be identical (see type-level note above).
	VRAMMb int `json:"vram_mb,omitempty"`
	Count  int `json:"count,omitempty"`
}

// Worker represents a registered sqi-worker agent. Workers self-report their
// capabilities at registration; the server persists the reported values and
// uses them for task matching.
type Worker struct {
	ID      string
	FarmID  string
	QueueID string // empty = no queue affinity
	// Name is the worker's human-readable display label (the worker.name
	// config field, default the hostname). Distinguishes multiple workers
	// running on one host in the UI; may be empty for workers registered
	// before this field existed, in which case callers fall back to Hostname.
	Name            string
	Hostname        string
	IPAddress       string
	ComputeLocation string
	OS              string
	OSVersion       string
	CPUCount        int
	RAMMb           int
	GPUInfo         GPUInfo
	Tags            map[string]string // arbitrary capability tags
	Status          WorkerStatus
	LastHeartbeatAt *time.Time
	RegisteredAt    time.Time
	UpdatedAt       time.Time
}

// WorkerSortField is a column by which [WorkerStore.ListWorkers] results can
// be ordered.
type WorkerSortField string

const (
	// WorkerSortByHostname orders workers alphabetically by hostname (default).
	WorkerSortByHostname WorkerSortField = "hostname"
	// WorkerSortByStatus orders workers alphabetically by status string.
	WorkerSortByStatus WorkerSortField = "status"
	// WorkerSortByRegisteredAt orders workers by registration time.
	WorkerSortByRegisteredAt WorkerSortField = "registered_at"
	// WorkerSortByLastHeartbeatAt orders workers by most recent heartbeat time.
	WorkerSortByLastHeartbeatAt WorkerSortField = "last_heartbeat_at"
)

// WorkerStore is the persistence interface for [Worker] records.
type WorkerStore interface {
	// RegisterWorker inserts or replaces the worker record for the given ID.
	// Called by the server when a worker sends its registration message.
	// If the worker ID already exists its record is updated in full.
	RegisterWorker(ctx context.Context, worker Worker) (Worker, error)

	// GetWorker returns the worker with the given ID, or [ErrNotFound].
	GetWorker(ctx context.Context, id string) (Worker, error)

	// ListWorkers returns a paginated, filtered, and sorted page of workers
	// matching opts. Call [Pagination.Validate] on opts.Pagination before
	// passing it to ensure sensible defaults are applied.
	ListWorkers(ctx context.Context, opts ListWorkersOptions) (Page[Worker], error)

	// UpdateWorker replaces the mutable capability fields of an existing
	// worker (everything except ID and RegisteredAt) and updates UpdatedAt.
	// Returns [ErrNotFound] if the worker does not exist.
	UpdateWorker(ctx context.Context, worker Worker) (Worker, error)

	// UpdateWorkerStatus sets the status of the worker and updates UpdatedAt.
	// Returns [ErrNotFound] if the worker does not exist.
	UpdateWorkerStatus(ctx context.Context, id string, status WorkerStatus) error

	// UpdateWorkerHeartbeat records the most recent heartbeat time for the
	// given worker. This is a hot path; implementations should use a single
	// UPDATE statement with no unnecessary reads.
	UpdateWorkerHeartbeat(ctx context.Context, id string, at time.Time) error

	// ListStaleWorkers returns workers whose last heartbeat is older than
	// before and whose status is [WorkerStatusOnline]. Used by the heartbeat
	// timeout sweep to find workers to mark offline.
	ListStaleWorkers(ctx context.Context, before time.Time) ([]Worker, error)

	// CountIdleWorkers returns the number of online workers in the given farm
	// that have no task currently in [TaskStatusAssigned] or
	// [TaskStatusRunning] state. An empty farmID matches all farms.
	// Used by the scheduler to update the [SchedulerIdleWorkers] Prometheus
	// gauge.
	CountIdleWorkers(ctx context.Context, farmID string) (int, error)

	// DeleteWorker hard-deletes the worker record with the given ID. Returns
	// [ErrNotFound] if no such worker exists. Task and task-attempt rows that
	// reference the worker by ID are left intact (the ID lives on as a
	// snapshot); callers are responsible for ensuring the worker has no
	// in-flight work before removing it.
	DeleteWorker(ctx context.Context, id string) error

	// DeleteOfflineWorkersBefore hard-deletes every worker in
	// [WorkerStatusOffline] whose LastHeartbeatAt is strictly before cutoff,
	// and returns the deleted records. Workers in any other status (including
	// administratively disabled) are never touched. Used by the scheduler's
	// offline-retention sweep to bound the growth of the worker table.
	DeleteOfflineWorkersBefore(ctx context.Context, cutoff time.Time) ([]Worker, error)
}

// ListWorkersOptions filters and orders [WorkerStore.ListWorkers] results.
// Zero values mean "no filter / use defaults".
type ListWorkersOptions struct {
	// Filters
	FarmID          string
	QueueID         string
	ComputeLocation string
	Status          WorkerStatus // empty = all statuses

	// IncludeUnaffiliated, when true and FarmID is non-empty, also returns
	// workers whose FarmID is empty (unaffiliated workers that accept tasks
	// from any farm). Used by the scheduler's pickWorker so that workers
	// started without an explicit farm configuration are still dispatched to.
	IncludeUnaffiliated bool

	// Ordering — zero values use WorkerSortByHostname / SortAsc.
	SortBy  WorkerSortField
	SortDir SortDir

	// Pagination — call Pagination.Validate() before use.
	Pagination Pagination
}
