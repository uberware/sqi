// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"time"
)

// WorkerStatus is the operational state of a worker as known to the server.
//
// A stored worker's [Worker.Status] is its liveness, online or offline, and is
// written by registration, deregistration and the heartbeat sweep. Whether an
// operator has disabled it is the separate [Worker.Disabled] flag, written only
// by [WorkerStore.SetWorkerDisabled]. [WorkerStatusDisabled] is the status the
// API, the status filter and the worker gauge report for a disabled worker
// ([Worker.EffectiveStatus]); it is never stored as liveness.
type WorkerStatus string

const (
	// WorkerStatusOnline means the worker is connected and accepting tasks.
	WorkerStatusOnline WorkerStatus = "online"
	// WorkerStatusOffline means the worker has not sent a heartbeat within the
	// configured timeout and is presumed unreachable.
	WorkerStatusOffline WorkerStatus = "offline"
	// WorkerStatusDisabled is the effective status of a worker an operator has
	// administratively paused; it will not receive new task assignments until
	// re-enabled.
	WorkerStatusDisabled WorkerStatus = "disabled"
)

// GPUInfo describes the GPU(s) available on a worker host.
//
// sqi assumes a homogeneous GPU configuration: all GPUs on the host are
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

// WorkerExprLimits holds the OpenJD EXPR evaluation caps a worker self-reports
// at registration: its expr.* worker-configuration section. The server
// persists them so the scheduler can refuse to dispatch an EXPR job to a worker
// that cannot run what the server accepted.
//
// Expressions in an EXPR template are metered twice: against the server's
// openjd.expr_* limits when the template is submitted, and again against the
// worker's expr.* limits when a task runs. If a worker's cap is tighter than
// the server's, a job is accepted, created and persisted and then every task of
// it fails on that host, after submission, naming a budget the submitter never
// saw (measured with the server at 10,000 positions and the worker at 5,000).
// Both sides are operator configuration, so the relation "worker cap >= server
// cap" can be broken by a YAML file; these fields are what let the server see
// it.
//
// Every value is self-reported, like CPUCount, RAMMb and Tags. It is a
// statement of what that worker will enforce, not a promise the server can
// verify.
//
// A zero field means "not advertised": a current worker always reports a real
// value (its config layer rejects 0), so silence means an older binary. The
// scheduler reads that as the compiled-in defaults, which is exact for a binary
// with no expr.* configuration and a documented guess for one that had the
// configuration but did not advertise it — see internal/scheduler's
// legacyWorkerExprCaps.
//
// All five of the worker's expr.* keys are carried. The server has no
// per-table counterpart to compare LetRetainedBytes against, but leaving it
// out is reachable through legal configuration: a worker at that key's floor
// rejects, once per task, a let: block the server accepted under a raised
// openjd.expr_template_retained_bytes. The server does bound the same values
// at a wider scope, and a wider scope is a valid upper bound; see
// internal/scheduler's exprCapShortfall for the comparison and for what it
// still cannot promise.
type WorkerExprLimits struct {
	// OperationLimit is the worker's §1.3.10 operation budget for ONE
	// expression evaluation (expr.operation_limit).
	OperationLimit int64 `json:"operation_limit,omitempty"`
	// MemoryLimit is the worker's §1.3.9 live-byte budget for ONE expression
	// evaluation (expr.memory_limit).
	MemoryLimit int64 `json:"memory_limit,omitempty"`
	// AssignmentPositions is how many expression positions the worker will
	// resolve for ONE assignment (expr.assignment_positions).
	AssignmentPositions int64 `json:"assignment_positions,omitempty"`
	// AssignmentRetainedBytes is how many bytes let: bindings may retain
	// across one assignment (expr.assignment_retained_bytes).
	AssignmentRetainedBytes int64 `json:"assignment_retained_bytes,omitempty"`
	// LetRetainedBytes is how many bytes ONE phase-3 symbol table may hold
	// live (expr.let_retained_bytes), measured across the whole table rather
	// than only its let-bound names.
	LetRetainedBytes int64 `json:"let_retained_bytes,omitempty"`
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
	// Arch is the worker's CPU architecture as the worker reports it, which is
	// runtime.GOARCH ("amd64", "arm64", ...). It backs the reserved OpenJD
	// attr.worker.cpu.arch host requirement, whose accepted values are the
	// SPECIFICATION's tokens ("x86_64", "arm64") rather than Go's — see
	// scheduler.cpuArch for the translation, which is the same shape as the
	// os.family darwin/macos one.
	//
	// Empty for workers registered before this field existed, and for any
	// worker running an older binary that does not send it. An empty value can
	// never satisfy an attr.worker.cpu.arch requirement; such a worker starts
	// reporting its architecture as soon as it restarts and re-registers.
	Arch string
	// Version is the sqi-worker build version the worker self-reports at
	// registration (the worker binary's internal/version.Version). May be empty
	// for workers registered before this field existed.
	Version string
	// InstanceID identifies the worker process that last registered. It
	// changes when the worker restarts and is empty for a worker that does not
	// send one.
	InstanceID string
	CPUCount   int
	RAMMb      int
	GPUInfo    GPUInfo
	Tags       map[string]string // arbitrary capability tags
	// ExprLimits holds the worker's self-reported OpenJD EXPR evaluation caps.
	// Zero-valued for workers registered before this field existed; see
	// [WorkerExprLimits] for what the server does with them.
	ExprLimits WorkerExprLimits
	// Status is the worker's liveness: [WorkerStatusOnline] or
	// [WorkerStatusOffline]. See [WorkerStatus].
	Status WorkerStatus
	// Disabled is true while an operator has the worker disabled. It is
	// independent of Status: a disabled worker still goes online and offline,
	// and stays disabled through both.
	Disabled        bool
	LastHeartbeatAt *time.Time
	RegisteredAt    time.Time
	UpdatedAt       time.Time
}

// EffectiveStatus is the status the API and the worker gauge report:
// [WorkerStatusDisabled] while the worker is disabled, otherwise its liveness.
func (w Worker) EffectiveStatus() WorkerStatus {
	if w.Disabled {
		return WorkerStatusDisabled
	}
	return w.Status
}

// Removable reports whether the worker may be hard-deleted. This is the one
// statement of the removability rule: a worker is removable when it is offline,
// disabled or not. A disabled worker that is still online is a paused machine,
// not a gone one, and is not removable.
//
// [WorkerStore.DeleteWorkerIfRemovable] applies this rule inside its write; the
// SQLite statement restates it in SQL, which cannot call Go, so a change here
// must be made there too. The API's pre-check and the fake call this method.
// The store adds one condition a Worker value alone cannot see: no task may be
// assigned to or running on the worker, so a true here does not
// guarantee the delete succeeds.
func (w Worker) Removable() bool {
	return w.Status == WorkerStatusOffline
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
	// Called by the server when a worker sends its registration message. If the
	// worker ID already exists its record is updated in full, except that an
	// empty InstanceID keeps the stored one and Disabled is never written: a
	// disabled worker stays disabled, and a new one is enabled. When the stored
	// InstanceID is non-empty and differs from a non-empty incoming one, the
	// previous worker process is gone: in the same transaction its assigned and
	// running tasks are reclaimed exactly as [WorkerStore.OfflineWorker]
	// reclaims them (attempts closed as failed with
	// [FailureReasonWorkerRestarted], claims released, tasks ready) and
	// returned as they are after the reset.
	RegisterWorker(ctx context.Context, worker Worker) (Worker, []Task, error)

	// GetWorker returns the worker with the given ID, or [ErrNotFound].
	GetWorker(ctx context.Context, id string) (Worker, error)

	// ListWorkers returns a paginated, filtered, and sorted page of workers
	// matching opts. Call [Pagination.Validate] on opts.Pagination before
	// passing it to ensure sensible defaults are applied.
	ListWorkers(ctx context.Context, opts ListWorkersOptions) (Page[Worker], error)

	// SetWorkerDisabled sets or clears the worker's [Worker.Disabled] flag,
	// updates UpdatedAt and returns the stored worker. It is the admin
	// enable/disable path and the only writer of the flag; it never changes
	// the worker's liveness. Setting the value the flag already holds is not an
	// error. Returns [ErrNotFound] if the worker does not exist.
	SetWorkerDisabled(ctx context.Context, id string, disabled bool) (Worker, error)

	// UpdateWorkerHeartbeat records the most recent heartbeat time for the
	// given worker. This is a hot path; implementations should use a single
	// UPDATE statement with no unnecessary reads.
	UpdateWorkerHeartbeat(ctx context.Context, id string, at time.Time) error

	// ListStaleWorkers returns workers whose last heartbeat is older than
	// before and whose status is [WorkerStatusOnline], disabled or not (a dead
	// disabled worker is swept like any other). Used by the
	// heartbeat timeout sweep to find workers to mark offline. The result is a
	// candidate list only; [WorkerStore.OfflineStaleWorker] re-checks the guard
	// inside its own write.
	ListStaleWorkers(ctx context.Context, before time.Time) ([]Worker, error)

	// OfflineStaleWorker takes a worker offline if, and only if, it is still
	// stale: the write is guarded by status = [WorkerStatusOnline] AND a
	// last_heartbeat_at strictly older than cutoff, so a heartbeat or a
	// re-registration that landed after the caller listed its candidates keeps
	// the worker online and its tasks running (invariant I1). Disabled does not
	// enter the guard and is not written: a disabled worker goes offline like
	// any other and stays disabled.
	//
	// When the guard matches, the same transaction closes the running attempts
	// of the worker's [TaskStatusAssigned] and [TaskStatusRunning] tasks as
	// [AttemptStatusFailed] with the message [FailureReasonWorkerOffline],
	// releases the claims of those attempts (invariant I3), and returns the
	// tasks to [TaskStatusReady] with their worker cleared. It returns exactly
	// the tasks it reclaimed (invariant I2), as they are after the reset, and
	// true. When the guard does not match (the worker is unknown, is not
	// online, or has a heartbeat at or after cutoff) nothing is written and it
	// returns
	// (nil, false, nil). A worker with no recorded heartbeat is never stale.
	//
	// now stamps the worker's and the reclaimed tasks' updated_at, the closed
	// attempts' ended_at and the released claims' released_at.
	//
	// Anchor rows and statement order: the worker row first, then each affected
	// job row sorted by id; within the transaction the worker is marked
	// offline, the tasks are reclaimed, and only then are their attempts closed
	// and their claims released. See the SQLite implementation for the exact
	// Postgres order.
	OfflineStaleWorker(ctx context.Context, id string, cutoff, now time.Time) ([]Task, bool, error)

	// OfflineWorker is the sibling of [WorkerStore.OfflineStaleWorker] for a
	// graceful deregister: the worker is taken offline whatever its heartbeat
	// (a disabled worker stays disabled), and its in-flight tasks are reclaimed
	// exactly as OfflineStaleWorker reclaims them (attempts closed, claims
	// released, tasks back to ready). It returns the reclaimed tasks as they
	// are after the reset and true, or [ErrNotFound] for an unknown worker. A
	// worker that is already offline is not an error: it has nothing left to
	// reclaim and the call returns no tasks.
	//
	// instanceID is the deregistering process's instance ID. When it and the
	// stored [Worker.InstanceID] are both non-empty and differ, the row belongs
	// to a newer process of the same worker, whose registration has already
	// landed: the deregister is from a superseded process (a late or redelivered
	// message), and nothing is written; the call returns (nil, false, nil). The
	// check is part of the guarded write (invariant I1). An empty ID on either
	// side proves nothing, and the worker is taken offline as before.
	OfflineWorker(ctx context.Context, id, instanceID string, now time.Time) ([]Task, bool, error)

	// CountIdleWorkers returns the number of online, enabled workers in the
	// given farm that have no task currently in [TaskStatusAssigned] or
	// [TaskStatusRunning] state. An empty farmID matches all farms.
	// Used by the scheduler to update the [SchedulerIdleWorkers] Prometheus
	// gauge.
	CountIdleWorkers(ctx context.Context, farmID string) (int, error)

	// DeleteWorkerIfRemovable deletes the worker only while it is removable
	// ([Worker.Removable]: offline, disabled or not), and only while no task is
	// assigned to or running on it. The rule is evaluated inside the DELETE
	// (invariant I1), so a worker that came back between the caller's read and
	// this write keeps its row. Returns [ErrConflict] when the worker exists
	// but is not removable and [ErrNotFound] when it does not exist. Task and
	// task-attempt rows that reference the worker by ID are left intact (the ID
	// lives on as a snapshot).
	DeleteWorkerIfRemovable(ctx context.Context, id string) error

	// DeleteOfflineWorkersBefore hard-deletes every enabled worker in
	// [WorkerStatusOffline] whose LastHeartbeatAt is strictly before cutoff,
	// and returns the deleted records. Online workers and disabled ones are
	// never touched: a disabled worker stays until an operator removes it.
	// Used by the scheduler's offline-retention sweep to bound the growth of
	// the worker table.
	DeleteOfflineWorkersBefore(ctx context.Context, cutoff time.Time) ([]Worker, error)
}

// ListWorkersOptions filters and orders [WorkerStore.ListWorkers] results.
// Zero values mean "no filter / use defaults".
type ListWorkersOptions struct {
	// Filters
	FarmID          string
	QueueID         string
	ComputeLocation string
	// Status filters by [Worker.EffectiveStatus]: disabled matches every
	// disabled worker, online and offline match only enabled ones. Empty = all.
	Status WorkerStatus
	// Search is a case-insensitive substring matched against name, hostname,
	// id, and compute_location. Empty = no search filter.
	Search string

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
