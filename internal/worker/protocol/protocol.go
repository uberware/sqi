// SPDX-License-Identifier: AGPL-3.0-or-later

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
// Every message carries a [Version] field set to [ProtocolVersion].  As of
// [ProtocolVersion] "2", the worker's lease loop rejects an [AssignMsg] whose
// Version does not match this worker's ProtocolVersion instead of decoding it
// anyway and silently ignoring fields it does not recognize (see
// internal/worker/lease.decodeAssignment).  This is a receiver-side check on
// the worker only: nothing yet enforces Version on the messages the server
// receives (RegisterMsg, HeartbeatMsg, TaskStatusMsg, LogChunkMsg), so a
// version mismatch on those channels still decodes silently.  This simple
// major-version gate is sufficient for Phase 1; a more nuanced compatibility
// matrix can be added when a breaking change is required.
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
//
// "2" adds [AssignMsg.EXPR], [AssignMsg.StepTemplateLet],
// [AssignMsg.StepScriptLet], [AssignMsg.ParameterTypes],
// [AssignMsg.JobParameterTypes], [AssignEnvironment.Let], [AssignMsg.JobName],
// [AssignMsg.StepName] and [AssignEnvironment.StepEnvironment] for EXPR
// phase-3 (worker-side) expression evaluation.  All nine are omitempty and
// populated only for a template that declares the EXPR extension, so the
// ADDED FIELDS leave a base-spec assignment's wire bytes unchanged.  The
// bump itself of course does not: "version":"1" becomes "version":"2" on
// every message, which is precisely what the worker's receiver-side check
// (internal/worker/lease.decodeAssignment) keys off, and why workers must
// be upgraded AFTER the server -- a "2" worker rejects every assignment a
// "1" server offers it and the tasks churn through reclaim.
const ProtocolVersion = "2"

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
	// TypeTaskCancel is defined here for protocol documentation and symmetry
	// with the other message-type constants.  TaskCancelMsg does not embed a
	// Version/Type envelope — the NATS subject (task.cancel.<taskID>) provides
	// routing — so this constant is not referenced by worker receive code.
	TypeTaskCancel = "task_cancel" // server → worker
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

	// Name is the worker's human-readable display label (the worker.name
	// config field, default the hostname). The server stores it so the UI can
	// distinguish multiple workers running on a single host.
	Name string `json:"name,omitempty"`

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

	// WorkerVersion is the sqi-worker build version (internal/version.Version),
	// distinct from the protocol Version above. Reported so the UI can show
	// which worker build is running.
	WorkerVersion string `json:"worker_version,omitempty"`

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

	// ExprLimits advertises the OpenJD EXPR evaluation caps this worker will
	// enforce when it resolves an assignment's expressions (its expr.*
	// configuration section).  The server compares them against its own
	// openjd.expr_* limits and refuses to dispatch an EXPR job to a worker
	// that is tighter — see [ExprLimits], which also names the two tests that
	// keep this key and its four inner keys matching the server's duplicate.
	//
	// Adding it here is deliberately a REGISTRATION-payload change and not a
	// [ProtocolVersion] bump: the version gate is the worker's receiver-side
	// check on AssignMsg, and nothing about an assignment changes.  A server
	// that does not know this field ignores it; a worker that does not send
	// it reports zeroes, which the server reads as "not advertised".
	ExprLimits ExprLimits `json:"expr_limits,omitzero"`
}

// ExprLimits is a worker's self-reported OpenJD EXPR evaluation caps.
//
// It mirrors store.WorkerExprLimits field for field and json tag for tag; the
// two are separate types because the worker binary must never import
// internal/store.  A renamed tag on either side silently decodes to zero, which
// the server reads as "not advertised" rather than as an error, so TWO tests in
// internal/scheduler keep them in step and neither subsumes the other:
// TestWorkerExprLimits_WireKeysMatchTheProtocol covers the four keys INSIDE
// this struct, and TestRegisterMsg_WireFieldsSurviveTheDuplication covers the
// outer expr_limits key on [RegisterMsg] (and every other field of it) by
// round-tripping a populated message into the server's duplicate struct.
//
// Zero means "not advertised".  A current worker always populates every field
// (its config layer rejects 0), so only a binary older than EXPR sub-project
// E4d Task 3 — the change that added this field — sends zeroes.
type ExprLimits struct {
	// OperationLimit is the §1.3.10 operation budget for ONE evaluation
	// (expr.operation_limit).
	OperationLimit int64 `json:"operation_limit,omitempty"`
	// MemoryLimit is the §1.3.9 live-byte budget for ONE evaluation
	// (expr.memory_limit).
	MemoryLimit int64 `json:"memory_limit,omitempty"`
	// AssignmentPositions is how many expression positions this worker will
	// resolve for one assignment (expr.assignment_positions).
	AssignmentPositions int64 `json:"assignment_positions,omitempty"`
	// AssignmentRetainedBytes is how many bytes let: bindings may retain
	// across one assignment (expr.assignment_retained_bytes).
	AssignmentRetainedBytes int64 `json:"assignment_retained_bytes,omitempty"`
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

	// JobName and StepName are the job's and step's own declared names — the
	// template's job "name" field and the matching StepTemplate's "name",
	// verbatim. Neither is derivable from another field on this message:
	// JobID/StepID are opaque IDs, and TaskName is the (possibly
	// parameter-decorated, see buildTaskName) per-task display name, which
	// equals the bare step name only when the step declares no task
	// parameters. Phase-3 EXPR evaluation needs the real Job.Name/Step.Name
	// symbols (section 1.2.2), so this pair exists specifically to carry
	// them. Present only when EXPR is true, like the other phase-3 fields —
	// a base-spec assignment has no format-string position that can
	// reference either symbol, so populating them unconditionally would grow
	// its wire bytes for no reason.
	JobName  string `json:"job_name,omitempty"`
	StepName string `json:"step_name,omitempty"`

	// WorkerID is the ID of the worker this assignment targets.
	WorkerID string `json:"worker_id"`

	// AssignedAt is the server-side time the assignment was created.
	AssignedAt time.Time `json:"assigned_at"`

	// ComputeLocation is the compute-location name the server targeted when
	// building this assignment's path map. Workers compare this against their
	// own configured compute location and nack the message if they differ,
	// ensuring a reassigned worker (after a config change without restart) does
	// not execute a task with path mappings intended for a different location.
	// Empty means the server sent no location constraint (e.g., when no path
	// map was generated).
	ComputeLocation string `json:"compute_location,omitempty"`

	// Isolation is the OS identity this task must execute as, resolved from the
	// task's queue. Nil means no isolation: the worker runs the task as its own
	// user, which is the pre-isolation behavior.
	//
	// This carries a USERNAME ONLY and never a credential. Worker↔server
	// transport authentication does not exist yet (deferred to Phase 4), so
	// nothing secret may travel on this channel.
	Isolation *IsolationSpec `json:"isolation,omitempty"`

	// ── Parameter space ───────────────────────────────────────────────────

	// Parameters are the resolved task parameter values for this specific task,
	// materialized from the step's parameter-space expansion.
	// Format strings in commands and args reference these via {{Task.Param.Name}}.
	Parameters map[string]string `json:"parameters,omitempty"`

	// JobParameters are the job-level parameter values provided at submission.
	// Needed by the worker to resolve {{Param.Name}} format strings.
	JobParameters map[string]string `json:"job_parameters,omitempty"`

	// ParameterTypes declares the OpenJD type each entry in Parameters was
	// declared with in the step's taskParameterDefinitions ("STRING", "PATH",
	// "INT", "FLOAT", or sqi's "CHUNK[INT]"), keyed by parameter name. Phase-3
	// expression evaluation needs a parameter's declared type to render
	// Task.Param.<name> correctly — Param.Count + 1 must add, not concatenate
	// — and that type must never be inferred from the value text, since "1"
	// is a valid STRING parameter and inference would silently change an
	// expression's meaning. Present only when EXPR is true; omitted (nil) for
	// a base-spec assignment, so its wire bytes are unchanged by this field.
	ParameterTypes map[string]string `json:"parameter_types,omitempty"`

	// JobParameterTypes is JobParameters' counterpart: the declared OpenJD
	// type of each job parameter from the template's parameterDefinitions,
	// keyed by parameter name. Same population rule and reason as
	// ParameterTypes.
	JobParameterTypes map[string]string `json:"job_parameter_types,omitempty"`

	// ── Expression evaluation (EXPR extension) ────────────────────────────

	// EXPR reports whether the job template that produced this task declares
	// the EXPR extension (extensions: [EXPR]). The worker has no template of
	// its own to inspect, so this flag is what it must use to choose between
	// the existing plain-substitution fmtstring.Resolve path and an
	// EXPR-aware evaluator: a template that does not declare EXPR must take
	// exactly the code path it took before this field existed, byte for
	// byte. False (the zero value) is omitted, so a base-spec assignment's
	// wire bytes are unchanged.
	EXPR bool `json:"expr,omitempty"`

	// StepTemplateLet holds the step template's own let: block (Template
	// Schemas §3.6), as the raw, unparsed "name = expression" strings in
	// declaration order. It governs OnRun and the step's embedded files.
	// Bindings are shipped as raw source and re-evaluated on the worker, but
	// NOT for the reason the other two let fields carry: a step template's
	// block is the one scope that COULD be resolved server-side.
	// internal/openjd/scope.go's ScopeStepTemplate exposes only Param.,
	// RawParam., Job.Name and Step.Name — no Task.* and no Session.* — and
	// checkLetBindings enforces that at submission, so every name this block
	// can reference is already concrete by phase 2.
	//
	// It ships raw anyway for two reasons. Its bindings are the table
	// StepScriptLet is evaluated against, so sending values here and source
	// there would split one ordered evaluation across two machines; and
	// re-evaluating every block the same way on the host is what keeps phase
	// 3 "the same walk with a different table" rather than a second
	// mechanism. Present only when EXPR is true.
	StepTemplateLet []string `json:"step_template_let,omitempty"`

	// StepScriptLet holds the step *script's* let: block. Template Schemas
	// §3.6 forbids a script's let from shadowing a name the step template
	// already bound — a rule that only means anything if the two blocks stay
	// distinguishable on the wire, so this travels as a field separate from
	// StepTemplateLet rather than the two being concatenated. Step-template
	// bindings are evaluated first and are visible when this block is
	// re-evaluated. Present only when EXPR is true.
	StepScriptLet []string `json:"step_script_let,omitempty"`

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
	// location configuration for the worker's compute location.  May be empty
	// when a task references no named storage locations.
	PathMap []PathMapRule `json:"path_map,omitempty"`

	// PathDeliveries is the ordered set of enabled path-delivery mechanisms for
	// this task (the declared SQI_PATH_TRANSLATION set, or the implicit default).
	PathDeliveries []PathDelivery `json:"path_deliveries,omitempty"`

	// Staging is the manifest of job-level PATH parameters to stage locally,
	// present only when the stage_locally delivery is enabled.
	Staging []StageEntry `json:"staging,omitempty"`
}

// IsolationSpec names the OS identity a task must run as. It is resolved
// server-side from the queue and contains no secret: on Windows the credential
// is obtained worker-side from the local credential store or via S4U.
type IsolationSpec struct {
	// User is the OS username to run as. Required when IsolationSpec is present.
	User string `json:"user"`
	// Group is the OS group. Empty means the user's primary group.
	Group string `json:"group,omitempty"`
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
	// Filename is the on-disk name; if empty, Name is used.
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

	// Let holds this environment's script let: block (Template Schemas
	// §3.6), as raw "name = expression" strings in declaration order. It
	// governs this environment's onEnter/onExit actions, variables, and
	// embedded files, and is re-evaluated on the worker for the same reason
	// as [AssignMsg.StepTemplateLet]. Present only when the owning
	// AssignMsg's EXPR flag is true.
	Let []string `json:"let,omitempty"`

	// StepEnvironment reports whether this entry came from a StepTemplate's
	// stepEnvironments (true) rather than the JobTemplate's jobEnvironments
	// (false/absent). assign.go's convertEnvironment builds BOTH lists into
	// this same []AssignEnvironment slice with no other distinguishing
	// field, so a worker-side symbol-table builder has no other way to tell
	// a step environment from a job one — and per Template Schemas §3.6.2,
	// a step environment's script legitimately sees Step.Name while a job
	// environment's does not (internal/openjd's ScopeStepEnvironment =
	// ScopeJobEnvironment + Step.Name — scope.go). Present (true) only when
	// the owning AssignMsg's EXPR flag is true and this is a step
	// environment; omitted for a job environment or a base-spec assignment.
	StepEnvironment bool `json:"step_environment,omitempty"`
}

// PathMapRule is one source→destination path mapping in the OpenJD
// pathmapping-1.0 file.  The worker writes all rules to the JSON file at
// <session_dir>/path_mapping.json before running any actions, conformant with
// the OpenJD pathmapping-1.0 schema.
type PathMapRule struct {
	// SourcePathFormat is the format of SourcePath: the OpenJD enum "POSIX" or
	// "WINDOWS" (and "URI" only with the path-mapping EXPR extension).  It tells
	// an OpenJD-aware application how to interpret and compare SourcePath.
	SourcePathFormat string `json:"source_path_format"`
	// SourcePath is the source path to match (the shared/canonical root).
	SourcePath string `json:"source_path"`
	// DestinationPath is the worker-local concrete path that replaces SourcePath.
	DestinationPath string `json:"destination_path"`
}

// PathDelivery mirrors openjd.PathDelivery: one enabled delivery + settings.
type PathDelivery struct {
	Kind     string `json:"kind"`
	Pattern  string `json:"pattern,omitempty"`
	Variable string `json:"variable,omitempty"`
}

// StageEntry is one path to stage locally, with its dataFlow direction and
// objectType, resolved to a concrete worker-visible path by the server.
type StageEntry struct {
	Path       string `json:"path"`
	Direction  string `json:"direction"` // IN | OUT | INOUT
	ObjectType string `json:"object_type,omitempty"`
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
	// See docs/architecture.md ("SQLite schema overview").
	SessionID string `json:"session_id,omitempty"`

	// At is the worker's local timestamp for this event.
	// The server uses this for the attempt's EndedAt; if zero the server clock
	// is used.
	At time.Time `json:"at"`

	// WorkerID is the ID of the worker that executed (or is executing) this task.
	// Included so the server can correlate status messages with the worker record
	// without a separate store lookup.
	WorkerID string `json:"worker_id,omitempty"`

	// LastProgress is the most-recent openjd_progress value (0–100) seen before
	// this status event, or null if no progress directive was emitted by the
	// task process up to this point.
	LastProgress *int `json:"last_progress,omitempty"`

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

// ── TaskCancelMsg ─────────────────────────────────────────────────────────────

// TaskCancelMsg is the JSON payload the server publishes to
// task.cancel.<taskID> when a running task must be interrupted.
//
// The worker's cancel handler reads this message and calls Cancel on the
// matching in-progress task, triggering SIGTERM → SIGKILL escalation.
//
// Note: this message has no Version/Type envelope — the NATS subject
// (task.cancel.<taskID>) provides sufficient routing.  The struct matches
// the scheduler's internal cancelPayload wire format.
type TaskCancelMsg struct {
	// TaskID is the task to cancel, redundant with the NATS subject but
	// included for log attribution.
	TaskID string `json:"task_id"`

	// JobID is the job the task belongs to, for log attribution.
	JobID string `json:"job_id"`

	// CanceledAt is the server-side timestamp of the cancellation request.
	CanceledAt time.Time `json:"canceled_at"`
}
