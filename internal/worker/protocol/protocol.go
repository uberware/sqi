// SPDX-License-Identifier: AGPL-3.0-only

// Package protocol defines the versioned JSON wire protocol used between
// sqi-server and sqi-worker agents.
//
// # Message flow
//
// Each message type flows in a specific direction over NATS JetStream:
//
//	RegisterMsg    worker → server   (worker.register)
//	HeartbeatMsg   worker → server   (worker.heartbeat)
//	AssignMsg      server → worker   (work.assign.<queue>)
//	TaskStatusMsg  worker → server   (task.status.<job>)
//	LogChunkMsg    worker → server   (task.logs.<task>)
//
// # Versioning
//
// Every message carries a [Version] field set to [ProtocolVersion].  The
// server and worker both validate this field and reject messages with
// unsupported versions, logging a warning so operators know to upgrade.
// This simple major-version gate is sufficient for Phase 1; a more
// nuanced compatibility matrix can be added when a breaking change is required.
//
// # Encoding
//
// All messages are encoded as UTF-8 JSON.  Protobuf is a viable future
// migration path if bandwidth or parse latency become a concern at scale,
// but JSON keeps the protocol inspectable with standard tools (nats CLI,
// curl, jq) which is valuable during development and debugging.
package protocol

import "time"

// ProtocolVersion is the current wire-protocol version.  Increment this when a
// breaking change is made to any message type — workers and servers agree on
// a single version at a time.
const ProtocolVersion = "1"

// ── Message type constants ────────────────────────────────────────────────────

// Message type strings embedded in each message's Type field.
// Receivers use these to route or validate messages before unmarshaling into
// a concrete type. In practice the NATS subject already identifies the message
// type; the Type field provides a double-check and makes log entries
// self-describing.
const (
	TypeRegister   = "register"    // worker → server
	TypeDeregister = "deregister"  // worker → server
	TypeHeartbeat  = "heartbeat"   // worker → server
	TypeAssign     = "assign"      // server → worker
	TypeTaskStatus = "task_status" // worker → server
	TypeLogChunk   = "log_chunk"   // worker → server
)

// ── RegisterMsg ───────────────────────────────────────────────────────────────

// RegisterMsg is the JSON payload workers publish to worker.register.
// Workers MUST publish this on first connect and on every reconnect so the
// server always has a current view of their capabilities.
//
// Fields map 1:1 with [store.Worker]; the server upserts a Worker row from
// each received registration.
type RegisterMsg struct {
	// Version must equal [ProtocolVersion].
	Version string `json:"version"`
	// Type must equal [TypeRegister].
	Type string `json:"type"`

	// WorkerID is the stable unique identifier for this worker instance.
	// Workers MUST use the same ID across restarts so that re-registration
	// updates the existing record rather than creating a duplicate.
	WorkerID string `json:"worker_id"`

	// FarmID is the farm this worker belongs to.  Required.
	FarmID string `json:"farm_id"`

	// QueueID restricts the worker to a single queue if non-empty.
	// An empty value means the worker accepts tasks from any queue in FarmID.
	QueueID string `json:"queue_id,omitempty"`

	// Hostname and IPAddress are the worker's network identity.
	Hostname  string `json:"hostname"`
	IPAddress string `json:"ip_address,omitempty"`

	// ComputeLocation is the named compute location this worker belongs to
	// (e.g. "onprem_linux", "cloud_aws_us_east"). Used for path translation
	// and task affinity.
	ComputeLocation string `json:"compute_location,omitempty"`

	// OS and OSVersion are the worker's operating-system identity.
	OS        string `json:"os"`
	OSVersion string `json:"os_version,omitempty"`

	// CPUCount and RAMMb are the worker's hardware capacity.
	CPUCount int `json:"cpu_count,omitempty"`
	RAMMb    int `json:"ram_mb,omitempty"`

	// GPUInfo describes GPU(s) installed on the worker host.
	GPUInfo GPUInfo `json:"gpu_info"`

	// MaxConcurrentTasks is the maximum number of tasks this worker will
	// execute simultaneously. Advertised to the server for informational
	// purposes and future server-side admission control. In Phase 1 the server
	// does not persist or enforce this value; the worker enforces it locally
	// via a semaphore in the pull loop.
	MaxConcurrentTasks int `json:"max_concurrent_tasks,omitempty"`

	// Tags holds arbitrary key/value capability tags the worker self-reports.
	Tags map[string]string `json:"tags,omitempty"`
}

// GPUInfo describes the GPU(s) available on a worker host.
// See [store.GPUInfo] for the canonical type; this mirrors it for the
// protocol layer so callers do not need to import the store package.
type GPUInfo struct {
	Vendor string `json:"vendor,omitempty"`
	Model  string `json:"model,omitempty"`
	// VRAMMb is the VRAM capacity of each GPU in mebibytes.
	VRAMMb int `json:"vram_mb,omitempty"`
	Count  int `json:"count,omitempty"`
}

// ── DeregisterMsg ─────────────────────────────────────────────────────────────

// DeregisterMsg is the JSON payload workers publish to worker.deregister on
// graceful shutdown. The server marks the worker offline immediately upon
// receipt, rather than waiting for the heartbeat timeout sweep.
//
// Workers SHOULD publish this as the last action before closing the NATS
// connection so that the server's task scheduler stops dispatching new
// assignments to this worker immediately.
type DeregisterMsg struct {
	// Version must equal [ProtocolVersion].
	Version string `json:"version"`
	// Type must equal [TypeDeregister].
	Type string `json:"type"`

	// WorkerID identifies the departing worker.
	WorkerID string `json:"worker_id"`

	// Reason is an optional human-readable explanation for the departure
	// (e.g. "graceful shutdown", "maintenance").
	Reason string `json:"reason,omitempty"`
}

// ── HeartbeatMsg ─────────────────────────────────────────────────────────────

// HeartbeatMsg is the JSON payload workers publish to worker.heartbeat on a
// regular interval.  The server uses these to track worker liveness; workers
// that stop sending heartbeats are marked offline by the heartbeat sweep after
// [scheduler.Config.WorkerTimeout] elapses.
//
// Beyond the liveness signal, each heartbeat carries enough runtime state for
// the server to detect stale assignments without additional store queries:
// active task IDs, counts, and the last time this worker received an
// assignment.
type HeartbeatMsg struct {
	// Version must equal [ProtocolVersion].
	Version string `json:"version"`
	// Type must equal [TypeHeartbeat].
	Type string `json:"type"`

	// WorkerID identifies the sender.
	WorkerID string `json:"worker_id"`

	// At is the worker's local timestamp when the heartbeat was composed.
	// If zero or absent, the server substitutes its own clock.
	At time.Time `json:"at"`

	// ActiveTaskCount is the number of task processes currently running on
	// this worker at the moment the heartbeat was composed.
	ActiveTaskCount int `json:"active_task_count"`

	// MaxConcurrentTasks is the worker's configured concurrency ceiling.
	// Included so the server can compute available capacity without a separate
	// registration lookup.
	MaxConcurrentTasks int `json:"max_concurrent_tasks"`

	// ActiveTaskIDs is the list of task IDs currently executing on this
	// worker. The server uses this to detect assignments that are no longer
	// present on the worker (e.g. due to a worker crash and restart) so it
	// can mark them failed rather than waiting for a timeout.
	// Empty when no tasks are running.
	ActiveTaskIDs []string `json:"active_task_ids,omitempty"`

	// UptimeSeconds is the number of seconds the worker process has been
	// running since it started. Useful for diagnosing workers that are
	// restarting frequently.
	UptimeSeconds float64 `json:"uptime_seconds"`

	// LastAssignmentAt is the worker's local timestamp of the most recent
	// task assignment it received. Nil when the worker has not yet executed
	// any task in this process lifetime. The server uses this to distinguish
	// idle-but-healthy workers from workers that have stopped pulling
	// assignments.
	LastAssignmentAt *time.Time `json:"last_assignment_at,omitempty"`
}

// ── AssignMsg ─────────────────────────────────────────────────────────────────

// AssignMsg is the JSON payload the server publishes to work.assign.<queue>.
// It carries the full execution specification the worker needs to run the
// assigned task without making additional network calls to the server.
//
// Workers pull this message via their per-queue durable JetStream consumer,
// acknowledge it, and begin execution.  If a worker cannot accept the task
// it must nack immediately so NATS redelivers to another worker.
type AssignMsg struct {
	// Version must equal [ProtocolVersion].
	Version string `json:"version"`
	// Type must equal [TypeAssign].
	Type string `json:"type"`

	// ── Identity ──────────────────────────────────────────────────────────

	// TaskID, JobID, StepID, AttemptID identify the unit of work being assigned.
	TaskID    string `json:"task_id"`
	JobID     string `json:"job_id"`
	StepID    string `json:"step_id"`
	AttemptID string `json:"attempt_id"`

	// TaskName is the human-readable task name derived from the OpenJD template.
	TaskName string `json:"task_name"`

	// WorkerID is the ID of the worker this assignment targets.
	WorkerID string `json:"worker_id"`

	// AssignedAt is the server-side time the assignment was created.
	AssignedAt time.Time `json:"assigned_at"`

	// ── Parameter space ───────────────────────────────────────────────────

	// Parameters are the resolved task parameter values for this specific task,
	// materialized from the step's parameter-space expansion.
	// Format strings in commands and args reference these via {{Task.Param.Name}}.
	Parameters map[string]string `json:"parameters,omitempty"`

	// JobParameters are the job-level parameter values provided at submission.
	// Needed by the worker to resolve {{Param.Name}} format strings.
	JobParameters map[string]string `json:"job_parameters,omitempty"`

	// ── Execution specification ───────────────────────────────────────────

	// OnRun is the action the worker executes for each task.  It carries the
	// command, args, timeout, and cancelation policy from the OpenJD step script.
	// Nil means the step has no runnable body (unusual; included for completeness).
	OnRun *Action `json:"on_run,omitempty"`

	// EmbeddedFiles are plain-text files materialized to the session working
	// directory before OnRun executes.  Merged from both the step-level and
	// job-environment embedded file lists.
	EmbeddedFiles []EmbeddedFile `json:"embedded_files,omitempty"`

	// Environments are the ordered job and step environments that the worker
	// must enter (in order) before running tasks and exit (in reverse) afterward.
	// Job environments precede step environments, matching the OpenJD spec.
	Environments []AssignEnvironment `json:"environments,omitempty"`

	// ── Path translation ─────────────────────────────────────────────────

	// PathMap is the ordered list of source→destination root path translation
	// rules the worker writes to the OpenJD path-mapping JSON file in the
	// session working directory.  Generated from the server's named storage
	// location configuration for the worker's compute location.
	// Empty in Phase 1 (tasks 60–62 implement named storage location CRUD and
	// resolved-mode path translation).
	PathMap []PathMapRule `json:"path_map,omitempty"`
}

// Action is an executable command included in an [AssignMsg].
// It mirrors [openjd.Action] so the worker does not need to import the openjd
// package.
type Action struct {
	// Command is the executable path or name (may be a format string).
	Command string `json:"command"`
	// Args is the argument list (each entry may be a format string).
	Args []string `json:"args,omitempty"`
	// TimeoutSeconds, when > 0, is the maximum wall-clock time this action may
	// run before the worker kills it.  0 means no timeout.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// Cancelation describes how to stop a running action on cancel signal.
	Cancelation *CancelationMethod `json:"cancelation,omitempty"`
}

// CancelationMethod configures how a running [Action] is interrupted.
type CancelationMethod struct {
	// Mode is "TERMINATE" (immediate kill) or "NOTIFY_THEN_TERMINATE" (SIGTERM
	// then kill after NotifyPeriodSeconds).
	Mode string `json:"mode"`
	// NotifyPeriodSeconds is the grace period between SIGTERM and SIGKILL when
	// Mode is "NOTIFY_THEN_TERMINATE".  0 means use the OpenJD spec default.
	NotifyPeriodSeconds int `json:"notify_period_seconds,omitempty"`
}

// EmbeddedFile is a plain-text file included in an [AssignMsg] that the worker
// materializes to the session working directory before running actions.
type EmbeddedFile struct {
	// Name is the identifier for this embedded file.
	Name string `json:"name"`
	// Filename is the on-disk name; if empty the worker generates one.
	Filename string `json:"filename,omitempty"`
	// Data is the file content (may contain format-string references).
	Data string `json:"data"`
	// Runnable, when true, sets the file's execute permission.
	Runnable bool `json:"runnable,omitempty"`
	// EndOfLine is "AUTO", "LF", or "CRLF".  Empty means "AUTO".
	EndOfLine string `json:"end_of_line,omitempty"`
}

// AssignEnvironment is a single OpenJD environment included in an [AssignMsg].
// Environments are entered once per session (in order) and exited in reverse.
type AssignEnvironment struct {
	// Name identifies the environment; must be unique within the list.
	Name string `json:"name"`
	// OnEnter is the action the worker runs when entering this environment.
	OnEnter *Action `json:"on_enter,omitempty"`
	// OnExit is the action the worker runs when exiting this environment.
	OnExit *Action `json:"on_exit,omitempty"`
	// Variables are environment variables set for all actions in this session.
	Variables map[string]string `json:"variables,omitempty"`
	// EmbeddedFiles are files materialized before the onEnter action.
	EmbeddedFiles []EmbeddedFile `json:"embedded_files,omitempty"`
}

// PathMapRule is one source→destination root mapping in the OpenJD path-mapping
// file.  The worker writes all rules to the JSON file at
// <session_dir>/assetreferences.json (or whichever name the OpenJD spec
// designates) before running any actions.
type PathMapRule struct {
	// SourcePathFormat is the source root path (may be a filesystem path or
	// an S3 URI such as "s3://bucket/prefix").
	SourcePathFormat string `json:"source_path_format"`
	// DestinationPath is the local concrete path on the worker host.
	DestinationPath string `json:"destination_path"`
}

// ── TaskStatusMsg ─────────────────────────────────────────────────────────────

// TaskStatusMsg is the JSON payload workers publish to task.status.<job>
// to report a task-state transition.
//
// A worker MUST publish:
//   - TypeTaskStatus with Status "running" when it begins executing OnRun.
//   - TypeTaskStatus with Status "succeeded", "failed", or "canceled" when
//     the task reaches a terminal state.
//
// The server persists each message to the store and updates the task's status,
// the attempt record, and any downstream dependencies.
type TaskStatusMsg struct {
	// Version must equal [ProtocolVersion].
	Version string `json:"version"`
	// Type must equal [TypeTaskStatus].
	Type string `json:"type"`

	// TaskID and AttemptID identify the task execution being reported.
	// AttemptID must match the one in the [AssignMsg]; mismatches are silently
	// discarded (the assignment may have been superseded by a requeue).
	TaskID    string `json:"task_id"`
	AttemptID string `json:"attempt_id"`

	// JobID is included so the server can route the message to the correct NATS
	// subject consumer without looking it up from the store.
	JobID string `json:"job_id"`

	// Status is the new task state.  Valid values:
	//   "running"   — task is executing; at most one running message per attempt.
	//   "succeeded" — task exited successfully.
	//   "failed"    — task exited with a non-zero code or the worker reported a fatal error.
	//   "canceled"  — task was interrupted by a cancel signal.
	Status string `json:"status"`

	// ExitCode is the process exit code. Non-nil when Status is "succeeded" or
	// "failed". Nil for "running" and "canceled" (SIGKILL leaves no meaningful
	// exit code).
	ExitCode *int `json:"exit_code,omitempty"`

	// SessionID is the OpenJD session identifier for this task's execution
	// context. Persisted on the [store.TaskAttempt] for grouping/attribution.
	// See sqi.md §7.4.
	SessionID string `json:"session_id,omitempty"`

	// At is the worker's local timestamp for this event.
	// The server uses this for the attempt's EndedAt; if zero the server clock
	// is used.
	At time.Time `json:"at"`

	// Message is an optional human-readable description of the status — useful
	// for failure detail or progress annotations surfaced in the UI.
	Message string `json:"message,omitempty"`
}

// ── LogChunkMsg ───────────────────────────────────────────────────────────────

// LogChunkMsg is the JSON payload workers publish to task.logs.<task> as a
// running task emits output.
//
// Workers SHOULD publish log chunks continuously as output is produced rather
// than buffering until task completion, so that operators can tail live logs
// via the REST or WebSocket APIs.
//
// The server persists each chunk to the task_logs table with a monotonic
// sequence number (per attempt) so that chunks can be replayed in order
// regardless of NATS delivery order.
type LogChunkMsg struct {
	// Version must equal [ProtocolVersion].
	Version string `json:"version"`
	// Type must equal [TypeLogChunk].
	Type string `json:"type"`

	// TaskID and AttemptID identify the task execution producing this output.
	TaskID    string `json:"task_id"`
	AttemptID string `json:"attempt_id"`

	// SeqNum is the worker's monotonic sequence number for this chunk within
	// the attempt.  Workers start at 1 and increment by 1 for each published
	// chunk.  The server stores this for in-order replay via the log-tail API.
	// The server also assigns its own server-side sequence number (the NATS
	// stream sequence) for persistence ordering; SeqNum is the worker-side
	// logical counter.
	SeqNum int64 `json:"seq_num"`

	// At is the worker's local timestamp when the chunk was captured.
	At time.Time `json:"at"`

	// Stream identifies the output stream: "stdout" or "stderr".
	Stream string `json:"stream"`

	// Data is the raw log text content of this chunk.  Workers may include
	// multiple lines per chunk; lines are not stripped or normalized by the
	// server.
	Data string `json:"data"`
}
