// SPDX-License-Identifier: AGPL-3.0-or-later

// Package protocol defines the versioned JSON wire protocol used between
// sqi-server and sqi-worker agents.
//
// # Message flow
//
// Each message type flows in a specific direction over NATS. Five of the eight
// ride JetStream streams for at-least-once delivery. AssignMsg is the reply
// half of a core-NATS request/reply exchange with no stream behind it, and
// ControlMsg is the request half of another.
// TaskCancelMsg is published through JetStream but received on a core
// subscription, so a cancel sent while the worker is disconnected is missed
// (see internal/bus for the full subject/stream split):
//
//	RegisterMsg    worker → server   JetStream    (worker.register.<worker>)
//	DeregisterMsg  worker → server   JetStream    (worker.deregister.<worker>)
//	HeartbeatMsg   worker → server   JetStream    (worker.heartbeat.<worker>)
//	AssignMsg      server → worker   core NATS    (reply to work.lease.<worker>.<queue>)
//	TaskStatusMsg  worker → server   JetStream    (task.status.<worker>.<job>)
//	LogChunkMsg    worker → server   JetStream    (task.logs.<worker>.<task>)
//	TaskCancelMsg  server → worker   core NATS    (received on task.cancel.<worker>)
//	ControlMsg     server → worker   core NATS    (request on worker.control.<worker>; reply is ControlReplyMsg)
//
// # Versioning
//
// Messages other than TaskCancelMsg carry a [Version] field set to
// [ProtocolVersion], and the receiver rejects a message whose Version is not
// exactly its own instead of decoding it anyway and silently ignoring the
// fields it does not recognize. Four channels are gated:
//
//	AssignMsg      server → worker   internal/worker/lease.decodeAssignment
//	RegisterMsg    worker → server   scheduler.handleWorkerRegister
//	HeartbeatMsg   worker → server   scheduler.handleWorkerHeartbeat
//	TaskStatusMsg  worker → server   scheduler.handleTaskStatusMessage
//
// A rejected message is discarded and never redelivered, because its version
// cannot change on a retry. See scheduler.discardOnVersionMismatch.
//
// LogChunkMsg and DeregisterMsg are not gated: a mismatched worker is already
// fenced out by the registration gate, so neither channel can carry
// work-affecting state on its own. The worst case is a persisted log line or a
// worker marked offline slightly early.
//
// The gate is an exact match on one version string; there is no compatibility
// matrix.
//
// # Encoding
//
// All messages are encoded as UTF-8 JSON, which keeps the protocol inspectable
// with standard tools (nats CLI, curl, jq).
package protocol

import (
	"fmt"
	"time"
)

// ProtocolVersion is the wire-protocol version. A worker and a server must
// agree on it exactly: the receiver rejects a message carrying a different one
// (internal/worker/lease.decodeAssignment on the worker, the scheduler's
// version gate on the server). A worker whose version differs from the
// server's rejects every assignment, and its tasks churn through reclaim, so
// upgrade the server before the workers. A change to a message type that an
// older peer would misread bumps the version; adding an omitempty field that an
// older peer ignores harmlessly does not.
//
// The current wire shape:
//   - Every worker → server NATS subject carries the publishing worker's
//     identity: task.status.<worker>.<job>, task.logs.<worker>.<task>,
//     work.lease.<worker>.<queue> and worker.{register,heartbeat,deregister}.<worker>
//     (see the subject table in internal/bus). A worker publishing on shorter
//     subjects reaches no consumer.
//   - A cancel goes to the holding worker's task.cancel.<workerID>, which the
//     worker subscribes to once before it asks for work. A worker subscribed
//     only to per-task subjects would never be told to stop a run.
//   - [TaskCancelMsg.AttemptID] narrows a cancel to one attempt. A worker that
//     does not know the field would read the cancel as task-wide and could stop
//     a task's newer attempt on a cancel meant for a stale one.
//   - The EXPR fields of [AssignMsg] and [AssignEnvironment] are omitempty and
//     populated only for a template that declares the EXPR extension, so a
//     base-spec assignment's wire bytes carry none of them.
//   - A worker subscribes to worker.control.<workerID> before it asks for work
//     and answers power commands ([ControlMsg]) there. A worker that does not
//     listen would leave a sleep or shutdown request unanswered.
const ProtocolVersion = "6"

// ── Message type constants ────────────────────────────────────────────────────

// Message type strings embedded in each message's Type field. Receivers use
// them to validate a message before unmarshaling it into a concrete type. The
// NATS subject already identifies the message type; the Type field makes log
// entries self-describing.
const (
	TypeRegister   = "register"    // worker → server
	TypeDeregister = "deregister"  // worker → server
	TypeHeartbeat  = "heartbeat"   // worker → server
	TypeAssign     = "assign"      // server → worker
	TypeTaskStatus = "task_status" // worker → server
	TypeLogChunk   = "log_chunk"   // worker → server
	TypeControl    = "control"     // server → worker
	// TypeTaskCancel completes the set of message-type constants.
	// TaskCancelMsg has no Version/Type envelope (the subject
	// task.cancel.<workerID> and the message's task and attempt IDs route it),
	// so worker receive code does not reference this constant.
	TypeTaskCancel = "task_cancel" // server → worker
)

// MessageWorkerShutdown is the Message of the "failed" TaskStatusMsg a worker
// publishes for each task it abandons on a forced shutdown. The server treats
// it as a reclaim, not a genuine failure.
const MessageWorkerShutdown = "worker_shutdown"

// TimeoutReason is the failure reason of a task killed at its onRun timeout.
func TimeoutReason(timeout time.Duration) string {
	return fmt.Sprintf("execution timeout after %s", timeout)
}

// ── RegisterMsg ───────────────────────────────────────────────────────────────

// RegisterMsg is the JSON payload workers publish to worker.register.<worker>.
// Workers publish this on first connect and on every reconnect so the server
// has a current view of their capabilities.
//
// The server upserts a [store.Worker] row from each received registration.
type RegisterMsg struct {
	// Version must equal [ProtocolVersion].
	Version string `json:"version"`
	// Type must equal [TypeRegister].
	Type string `json:"type"`

	// WorkerID is the stable unique identifier for this worker instance.
	// Workers use the same ID across restarts so that re-registration updates
	// the existing record rather than creating a duplicate.
	WorkerID string `json:"worker_id"`

	// InstanceID is a random identifier generated once per worker process and
	// sent unchanged in every registration it makes (boot and NATS reconnect).
	// A new value tells the server the previous process is gone, so its
	// in-flight tasks are reclaimed. Optional: a server that does not know it
	// ignores it, and an empty value means "unknown".
	InstanceID string `json:"instance_id,omitempty"`

	// FarmID is the farm this worker belongs to. Required.
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
	// (e.g. "onprem_linux", "cloud_aws_us_east"). The server uses it to pick the
	// path-mapping rules for the worker's assignments and to match steps that
	// require a compute location.
	ComputeLocation string `json:"compute_location,omitempty"`

	// OS and OSVersion are the worker's operating-system identity.
	OS        string `json:"os"`
	OSVersion string `json:"os_version,omitempty"`

	// Arch is the worker's CPU architecture as runtime.GOARCH spells it
	// ("amd64", "arm64"). The server translates it onto OpenJD's
	// attr.worker.cpu.arch tokens ("x86_64", "arm64") when matching.
	//
	// A registration-payload field, not a ProtocolVersion change, for the
	// reason ExprLimits below gives. A server that does not know the field
	// ignores it, and a worker that omits it is stored with "", which matches
	// no arch requirement.
	Arch string `json:"arch,omitempty"`

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
	// execute simultaneously. It is informational: the server does not persist
	// or enforce it; the worker enforces it locally via a semaphore in the pull
	// loop.
	MaxConcurrentTasks int `json:"max_concurrent_tasks,omitempty"`

	// Tags holds arbitrary key/value capability tags the worker self-reports.
	Tags map[string]string `json:"tags,omitempty"`

	// ExprLimits advertises the OpenJD EXPR evaluation caps this worker
	// enforces when it resolves an assignment's expressions (its expr.*
	// configuration section). The server compares them against its own
	// openjd.expr_* limits and does not dispatch an EXPR job to a worker whose
	// caps are tighter; see [ExprLimits].
	//
	// It is a registration-payload field, not a [ProtocolVersion] change:
	// nothing about an assignment depends on it. A server that does not know
	// this field ignores it, and a worker that does not send it reports zeroes,
	// which the server reads as "not advertised". Adding a field is
	// compatible. Renaming one must bump the version, because old and new
	// workers would otherwise disagree about the key undetected.
	ExprLimits ExprLimits `json:"expr_limits,omitzero"`

	// Extensions lists the worker-enforced OpenJD extensions this worker
	// implements (see [ExtensionLogChecks]). The server withholds a job that
	// declares one of them from a worker that does not list it, because a job
	// whose checks nobody enforces would pass or fail on the process exit status
	// alone and drop the guarantee its author asked for.
	//
	// A registration-payload field, not a [ProtocolVersion] change, for the
	// reason ExprLimits above gives: a server that does not know it ignores it,
	// and a worker that omits it is read as enforcing nothing.
	Extensions []string `json:"extensions,omitempty"`

	// Interfaces lists the worker's network interfaces that carry a hardware
	// address, so the server can wake a sleeping worker with a Wake-on-LAN
	// packet from a peer on the same subnet. Optional: a worker that omits it
	// cannot be woken.
	Interfaces []NetInterface `json:"interfaces,omitempty"`

	// CloudInstanceID is the cloud provider's identifier for the instance this
	// worker runs on, when it is one. The server uses it to find the instance
	// a power command or a scaling decision names. Optional.
	CloudInstanceID string `json:"cloud_instance_id,omitempty"`

	// CapacityGroup names the capacity group this worker asks to join (its
	// worker.capacity_group configuration). The server honors it only with
	// broker authentication off, and only for a sized group in the worker's
	// farm. With authentication on, membership comes from the group token the
	// worker enrolled with and the field is ignored.
	//
	// A registration-payload field, not a [ProtocolVersion] change: an older
	// server ignores it, and an older worker omits it and joins nothing.
	CapacityGroup string `json:"capacity_group,omitempty"`
}

// NetInterface is one network interface a worker reports in
// [RegisterMsg.Interfaces].
type NetInterface struct {
	// Name is the operating system's name for the interface ("eth0").
	Name string `json:"name"`
	// MAC is the hardware address, as "aa:bb:cc:dd:ee:ff".
	MAC string `json:"mac"`
	// Addrs are the interface's addresses in CIDR form, "192.168.1.20/24".
	Addrs []string `json:"addrs,omitempty"`
}

// Limits on [RegisterMsg.Interfaces]: a worker reports at most MaxInterfaces
// interfaces, each with at most MaxAddrsPerInterface addresses, and the server
// drops anything beyond.
const (
	MaxInterfaces        = 32
	MaxAddrsPerInterface = 16
)

// ControlCommand names a power command the server sends a worker.
type ControlCommand string

// The control commands.
const (
	// ControlSleep asks the worker to suspend its host.
	ControlSleep ControlCommand = "sleep"
	// ControlShutdown asks the worker to power its host off.
	ControlShutdown ControlCommand = "shutdown"
	// ControlWake asks the worker to send Wake-on-LAN packets for the
	// addresses in [ControlMsg.MACs] to the endpoints in [ControlMsg.Targets].
	// It goes to a running peer, never to the sleeping host.
	ControlWake ControlCommand = "wake"
)

// ControlMsg is the request the server sends to worker.control.<workerID> over
// core NATS request/reply. The worker answers with a [ControlReplyMsg].
type ControlMsg struct {
	// Version must equal [ProtocolVersion].
	Version string `json:"version"`
	// Type must equal [TypeControl].
	Type string `json:"type"`
	// Command is the power command.
	Command ControlCommand `json:"command"`

	// MACs and Targets apply to [ControlWake] only: the hardware addresses to
	// wake and the "ip:port" endpoints to send the packets to.
	MACs    []string `json:"macs,omitempty"`
	Targets []string `json:"targets,omitempty"`
}

// ControlReplyMsg is the worker's answer to a [ControlMsg].
type ControlReplyMsg struct {
	// Accepted is true when the worker will carry the command out.
	Accepted bool `json:"accepted"`
	// Reason says why the worker refused. Set only when Accepted is false.
	Reason string `json:"reason,omitempty"`
}

// The reasons a worker refuses a sleep or shutdown with, as
// [ControlReplyMsg.Reason] carries them. The server matches
// [ControlRefusalAlreadyRetiring] and reports the others as given.
const (
	// ControlRefusalBusy: the worker is running a task.
	ControlRefusalBusy = "busy"
	// ControlRefusalNotAllowed: the operator has not allowed remote sleep.
	ControlRefusalNotAllowed = "worker.power.allow_remote_sleep is false"
	// ControlRefusalAlreadyRetiring: the worker is already carrying out a
	// sleep or shutdown. The command is under way, so a server repeating it
	// reads this as accepted.
	ControlRefusalAlreadyRetiring = "already retiring"
)

// ExtensionLogChecks is the name of the sqi-defined step-level log-check
// extension, as a worker advertises it in [RegisterMsg.Extensions].
//
// It duplicates internal/openjd's ExtensionLogChecks because no production file
// under internal/worker may import internal/openjd (which imports the store).
// TestProtocolAndRegistryAgreeOnWorkerEnforcedExtensions in internal/scheduler,
// which imports both, checks that the two sets match.
const ExtensionLogChecks = "SQI_LOG_CHECKS"

// WorkerEnforcedExtensions returns the worker-enforced extensions this worker
// binary implements: what it advertises in [RegisterMsg.Extensions]. Keep it in
// step with internal/worker/executor, and sorted: the test that pins it to
// openjd's registry compares the two in order. It returns a new slice on each
// call.
func WorkerEnforcedExtensions() []string {
	return []string{ExtensionLogChecks}
}

// ExprLimits is a worker's self-reported OpenJD EXPR evaluation caps.
//
// It mirrors store.WorkerExprLimits field for field and json tag for tag. The
// two are separate types because no production file under internal/worker or
// cmd/sqi-worker imports internal/store: the worker carries the wire contract,
// the server carries the persistence model. A renamed tag on either side
// decodes to zero, which the server reads as "not advertised" rather than as
// an error.
// TestHandleWorkerRegister_EveryWireFieldReachesTheStore in internal/scheduler
// sends a fully populated [RegisterMsg] as JSON through the real registration
// handler and checks that every field, the five inside this struct included,
// arrives on the stored worker.
//
// Zero means "not advertised". A worker always populates every field (its
// config layer rejects 0), so only a worker that does not know this struct
// sends zeroes.
type ExprLimits struct {
	// OperationLimit is the §1.3.10 operation budget for one evaluation
	// (expr.operation_limit).
	OperationLimit int64 `json:"operation_limit,omitempty"`
	// MemoryLimit is the §1.3.9 live-byte budget for one evaluation
	// (expr.memory_limit).
	MemoryLimit int64 `json:"memory_limit,omitempty"`
	// AssignmentPositions is how many expression positions this worker will
	// resolve for one assignment (expr.assignment_positions).
	AssignmentPositions int64 `json:"assignment_positions,omitempty"`
	// AssignmentRetainedBytes is how many bytes let: bindings may retain
	// across one assignment (expr.assignment_retained_bytes).
	AssignmentRetainedBytes int64 `json:"assignment_retained_bytes,omitempty"`
	// LetRetainedBytes is how many bytes one symbol table may hold live
	// (expr.let_retained_bytes). The server has no per-table counterpart, but
	// a worker set too low here rejects work the server accepted, so it is
	// compared too; see internal/scheduler/exprcaps.go.
	LetRetainedBytes int64 `json:"let_retained_bytes,omitempty"`
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

// DeregisterKind says whether a departing worker may come back. The empty
// kind means it may: a service restart or upgrade, after which the worker
// returns with the same identity.
type DeregisterKind string

const (
	// DeregisterRetired is a machine leaving for good because it was retired.
	DeregisterRetired DeregisterKind = "retired"
	// DeregisterPreempted is a machine leaving for good because its provider
	// reclaimed it (a spot or preemptible instance).
	DeregisterPreempted DeregisterKind = "preempted"
)

// DeregisterMsg is the JSON payload workers publish to worker.deregister.<worker> on
// graceful shutdown. The server marks the worker offline immediately upon
// receipt, rather than waiting for the heartbeat timeout sweep.
//
// Workers publish this as the last action before closing the NATS connection
// so that the scheduler stops dispatching new assignments to this worker.
type DeregisterMsg struct {
	// Version must equal [ProtocolVersion].
	Version string `json:"version"`
	// Type must equal [TypeDeregister].
	Type string `json:"type"`

	// WorkerID identifies the departing worker.
	WorkerID string `json:"worker_id"`

	// InstanceID is the departing process's instance ID, the one it sends in
	// every [RegisterMsg]. The server ignores a deregister whose instance ID
	// differs from the one it last registered: that message comes from a
	// process that has since been replaced (a late or redelivered message), and
	// applying it would take the new process offline. Empty from a worker that
	// sends none; the server then applies the deregister.
	InstanceID string `json:"instance_id,omitempty"`

	// Reason is an optional human-readable explanation for the departure
	// (e.g. "graceful shutdown", "maintenance").
	Reason string `json:"reason,omitempty"`

	// Kind says whether the worker may come back. With [DeregisterRetired] or
	// [DeregisterPreempted] the server deletes an ephemeral capacity-group
	// member's worker row and revokes its credential; any other value, the
	// empty one included, keeps both. Optional and omitted when empty, so it
	// is not a [ProtocolVersion] change: a server that does not know it
	// ignores it.
	Kind DeregisterKind `json:"kind,omitempty"`
}

// ── HeartbeatMsg ─────────────────────────────────────────────────────────────

// HeartbeatMsg is the JSON payload workers publish to worker.heartbeat.<worker> on a
// regular interval.  The server uses these to track worker liveness; workers
// that stop sending heartbeats are marked offline by the heartbeat sweep after
// [scheduler.Config.WorkerTimeout] elapses.
//
// Beyond the liveness signal, each heartbeat reports the worker's runtime
// state: active task and attempt IDs, counts, uptime and the time of its last
// assignment. The server acts only on AttemptsReported and
// [HeartbeatMsg.ActiveAttemptIDs] (and reads At to spot a delayed heartbeat);
// the other fields are informational.
type HeartbeatMsg struct {
	// Version must equal [ProtocolVersion].
	Version string `json:"version"`
	// Type must equal [TypeHeartbeat].
	Type string `json:"type"`

	// WorkerID identifies the sender.
	WorkerID string `json:"worker_id"`

	// At is the worker's local timestamp when the heartbeat was composed. The
	// server uses it only to spot a delayed or skewed worker; liveness is timed
	// by the broker's stream timestamp, so a wrong worker clock cannot take a
	// worker offline. If zero or absent, the server substitutes its own clock.
	At time.Time `json:"at"`

	// ActiveTaskCount is the number of task processes currently running on
	// this worker at the moment the heartbeat was composed.
	ActiveTaskCount int `json:"active_task_count"`

	// MaxConcurrentTasks is the worker's configured concurrency ceiling. It is
	// informational: the server does not read it.
	MaxConcurrentTasks int `json:"max_concurrent_tasks"`

	// ActiveTaskIDs is the list of task IDs currently executing on this
	// worker, empty when none are running. It is informational: the server
	// reconciles on ActiveAttemptIDs instead.
	ActiveTaskIDs []string `json:"active_task_ids,omitempty"`

	// AttemptsReported is true when ActiveAttemptIDs is authoritative. The
	// server never reconciles a worker that sends neither field.
	AttemptsReported bool `json:"attempts_reported,omitempty"`

	// ActiveAttemptIDs lists every attempt this worker process holds, from the
	// moment its lease reply is decoded until its task goroutine exits (after
	// the terminal status is published). The server reclaims an attempt a
	// live worker has stopped reporting (scheduler.reconcile_grace), and sends
	// a cancel naming one the worker still reports after the store closed it.
	ActiveAttemptIDs []string `json:"active_attempt_ids,omitempty"`

	// UptimeSeconds is the number of seconds the worker process has been
	// running since it started.
	UptimeSeconds float64 `json:"uptime_seconds"`

	// LastAssignmentAt is the worker's local timestamp of the most recent
	// task assignment it received. Nil when the worker has not executed any
	// task in this process lifetime. It is informational: the server does not
	// read it.
	LastAssignmentAt *time.Time `json:"last_assignment_at,omitempty"`
}

// ── AssignMsg ─────────────────────────────────────────────────────────────────

// AssignMsg is one assignment in the server's reply to a worker's
// work.lease.<worker>.<queue> request. It carries the full execution
// specification the worker needs to run the assigned task without making
// additional network calls to the server.
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

	// JobName and StepName are the job's and step's own declared names: the
	// template's job "name" field and the matching StepTemplate's "name",
	// verbatim. Neither is derivable from another field on this message:
	// JobID/StepID are opaque IDs, and TaskName is the per-task display name,
	// which equals the bare step name only when the step declares no task
	// parameters. Worker-side EXPR evaluation needs the Job.Name and Step.Name
	// symbols (section 1.2.2). Present only when EXPR is true: a base-spec
	// assignment has no format-string position that can reference either
	// symbol.
	JobName  string `json:"job_name,omitempty"`
	StepName string `json:"step_name,omitempty"`

	// WorkerID is the ID of the worker this assignment targets.
	WorkerID string `json:"worker_id"`

	// AssignedAt is the server-side time the assignment was created.
	AssignedAt time.Time `json:"assigned_at"`

	// ComputeLocation is reserved: the server never sets it and the worker
	// never reads it. The server builds PathMap from the compute location the
	// worker registered with ([RegisterMsg.ComputeLocation]).
	ComputeLocation string `json:"compute_location,omitempty"`

	// Isolation is the OS identity this task must execute as, resolved from the
	// task's queue. Nil means no isolation: the worker runs the task as its own
	// user.
	//
	// This carries a username only, never a credential. Worker to server
	// transport authentication is opt-in and off by default, so nothing secret
	// may travel on this channel.
	Isolation *IsolationSpec `json:"isolation,omitempty"`

	// ── Parameter space ───────────────────────────────────────────────────

	// Parameters are the resolved task parameter values for this specific task,
	// materialized from the step's parameter-space expansion.
	// Format strings in commands and args reference these via {{Task.Param.Name}}.
	Parameters map[string]string `json:"parameters,omitempty"`

	// JobParameters are the job-level parameter values provided at submission.
	// The worker uses them to resolve {{Param.Name}} format strings.
	JobParameters map[string]string `json:"job_parameters,omitempty"`

	// ParameterTypes declares the OpenJD type each entry in Parameters was
	// declared with in the step's taskParameterDefinitions ("STRING", "PATH",
	// "INT", "FLOAT", or sqi's "CHUNK[INT]"), keyed by parameter name. Worker-side
	// expression evaluation needs a parameter's declared type to render
	// Task.Param.<name> correctly (Param.Count + 1 must add, not concatenate).
	// The type cannot be inferred from the value text, since "1" is a valid
	// STRING parameter. Present only when EXPR is true; nil for a base-spec
	// assignment.
	ParameterTypes map[string]string `json:"parameter_types,omitempty"`

	// JobParameterTypes is JobParameters' counterpart: the declared OpenJD
	// type of each job parameter from the template's parameterDefinitions,
	// keyed by parameter name. Same population rule and reason as
	// ParameterTypes.
	JobParameterTypes map[string]string `json:"job_parameter_types,omitempty"`

	// ── Expression evaluation (EXPR extension) ────────────────────────────

	// EXPR reports whether the job template that produced this task declares
	// the EXPR extension (extensions: [EXPR]). The worker has no template of
	// its own to inspect, so it uses this flag to choose between the
	// plain-substitution fmtstring.Resolve path and an EXPR-aware evaluator;
	// a template that does not declare EXPR takes the plain-substitution path.
	// False is omitted from the wire.
	EXPR bool `json:"expr,omitempty"`

	// StepTemplateLet holds the step template's own let: block (Template
	// Schemas §3.6), as the raw, unparsed "name = expression" strings in
	// declaration order. It governs OnRun and the step's embedded files.
	// The bindings ship as raw source and are evaluated on the worker. The
	// block references only Param., RawParam., Job.Name and Step.Name, so the
	// server could resolve it, but its bindings are the table StepScriptLet is
	// evaluated against: sending values here and source there would split one
	// ordered evaluation across two machines. Present only when EXPR is true.
	StepTemplateLet []string `json:"step_template_let,omitempty"`

	// StepScriptLet holds the step script's let: block. Template Schemas §3.6
	// forbids a script's let from shadowing a name the step template already
	// bound, which requires the two blocks to stay distinguishable on the wire,
	// so this is a field separate from StepTemplateLet. Step-template bindings
	// are evaluated first and are visible when this block is evaluated.
	// Present only when EXPR is true.
	StepScriptLet []string `json:"step_script_let,omitempty"`

	// ── Execution specification ───────────────────────────────────────────

	// OnRun is the action the worker executes for each task.  It carries the
	// command, args, timeout, and cancelation policy from the OpenJD step script.
	// Nil means the step has no runnable body.
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

	// LogChecks is the step's SQI_LOG_CHECKS block, present only when the step
	// carries one. The server only leases such a task to a worker that
	// advertised [ExtensionLogChecks]. Omitted when nil, so it does not
	// affect [ProtocolVersion].
	LogChecks *LogChecks `json:"log_checks,omitempty"`

	// LogProgress is the step's SQI_LOG_PROGRESS block, present only when the
	// step carries one. Omitted when nil, so it does not affect
	// [ProtocolVersion]. It is not worker-enforced: progress is advisory, so the
	// server leases the task to any worker, and a worker that does not know the
	// key drops it.
	LogProgress *LogProgress `json:"log_progress,omitempty"`
}

// LogProgress is the wire form of a step's SQI_LOG_PROGRESS block: RE2 patterns
// matched unanchored against each line the step's onRun action writes.
// FrameParameter is resolved server-side to the step's INT or CHUNK[INT] task
// parameter, and is empty when no progress pattern measures frames.
type LogProgress struct {
	FrameParameter string   `json:"frame_parameter,omitempty"`
	Progress       []string `json:"progress,omitempty"`
	Status         []string `json:"status,omitempty"`
}

// LogChecks is the wire form of a step's SQI_LOG_CHECKS block: RE2 patterns
// matched unanchored against each line the step's onRun action writes. A task
// fails when any line matches a FailOn pattern, or when no line matched a
// RequireLine pattern by the time the process exited.
type LogChecks struct {
	FailOn      []string `json:"fail_on,omitempty"`
	RequireLine []string `json:"require_line,omitempty"`
}

// IsolationSpec names the OS identity a task must run as. It is resolved
// server-side from the queue and contains no secret: on Windows the credential
// is read worker-side from the local credential store.
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
	// embedded files, and is evaluated on the worker for the same reason as
	// [AssignMsg.StepTemplateLet]. Present only when the owning AssignMsg's
	// EXPR flag is true.
	Let []string `json:"let,omitempty"`

	// StepEnvironment reports whether this entry came from a StepTemplate's
	// stepEnvironments (true) rather than the JobTemplate's jobEnvironments
	// (false/absent). Both lists share one []AssignEnvironment slice, so a
	// worker-side symbol-table builder needs this flag to tell them apart: per
	// Template Schemas §3.6.2, a step environment's script sees Step.Name while
	// a job environment's does not. True only when the owning AssignMsg's EXPR
	// flag is true and this is a step environment.
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

// TaskStatusMsg is the JSON payload workers publish to task.status.<worker>.<job>
// to report a task-state transition.
//
// A worker publishes:
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
	// AttemptID must match the one in the [AssignMsg]; a mismatch is discarded
	// (the assignment may have been superseded by a requeue).
	TaskID    string `json:"task_id"`
	AttemptID string `json:"attempt_id"`

	// JobID lets the server route the message to the correct NATS subject
	// consumer without a store lookup.
	JobID string `json:"job_id"`

	// Status is the new task state.  Valid values:
	//   "running"   — task is executing; sent when the attempt starts and again as
	//                 a throttled progress/status snapshot (LastProgress, Message).
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
	// It lets the server correlate status messages with the worker record
	// without a store lookup.
	WorkerID string `json:"worker_id,omitempty"`

	// LastProgress is the most-recent openjd_progress value (0–100) seen before
	// this status event, or null if no progress directive was emitted by the
	// task process up to this point.
	LastProgress *int `json:"last_progress,omitempty"`

	// Message is an optional human-readable description of the status, such as
	// failure detail or a progress annotation shown in the UI.
	Message string `json:"message,omitempty"`
}

// ── LogChunkMsg ───────────────────────────────────────────────────────────────

// LogChunkMsg is the JSON payload workers publish to task.logs.<worker>.<task> as a
// running task emits output.
//
// Workers publish log chunks as output is produced rather than buffering until
// task completion, so that operators can tail live logs via the REST or
// WebSocket APIs.
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

// TaskCancelMsg is the JSON payload the server publishes to the holding
// worker's task.cancel.<workerID> when a running task must be interrupted.
//
// The worker's cancel subscription reads this message and calls Cancel on the
// matching in-progress run, triggering SIGTERM → SIGKILL escalation.
//
// The message has no Version/Type envelope: the NATS subject names the worker
// and the message names the run.
type TaskCancelMsg struct {
	// TaskID is the task to cancel. It is empty when the server stops an
	// attempt its store has no record of (a database restored from before
	// the lease): AttemptID alone then names the run.
	TaskID string `json:"task_id"`

	// AttemptID, when set, limits the cancel to that attempt: a run of another
	// attempt of the task is left alone. The server sets it when it stops an
	// attempt it has already closed (a stale "running" report, or a heartbeat
	// still listing the attempt), because the worker may also be running the
	// task's newer attempt. Empty cancels every attempt of the task the worker
	// is running, which is what a job or task cancel wants.
	AttemptID string `json:"attempt_id,omitempty"`

	// JobID is the job the task belongs to, for log attribution.
	JobID string `json:"job_id"`

	// CanceledAt is the server-side timestamp of the cancellation request.
	CanceledAt time.Time `json:"canceled_at"`
}
