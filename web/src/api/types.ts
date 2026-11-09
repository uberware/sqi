// SPDX-License-Identifier: AGPL-3.0-or-later

// Domain types matching the sqi REST API wire format (JSON field names).
// Keep in sync with the OpenAPI spec at GET /api/v1/openapi.yaml and with the
// Go wire-format types in internal/api/*.go.

// ── Status enums ──────────────────────────────────────────────────────────────

/** Aggregate lifecycle status of a job. */
export type JobStatus =
  'pending' | 'running' | 'paused' | 'blocked' | 'completed' | 'failed' | 'canceled'
/** Aggregate status of a step within a job. */
export type StepStatus = 'pending' | 'ready' | 'running' | 'completed' | 'failed' | 'canceled'
/** Lifecycle status of an individual task. */
export type TaskStatus =
  'pending' | 'ready' | 'assigned' | 'running' | 'succeeded' | 'failed' | 'canceled'
/** Terminal/active status of a single task execution attempt. */
export type AttemptStatus = 'running' | 'succeeded' | 'failed' | 'canceled'
/** Connectivity/administrative status of a worker node. */
export type WorkerStatus = 'online' | 'offline' | 'disabled'
/** Output stream a log chunk came from. */
export type LogStream = 'stdout' | 'stderr'
/** Wire format of a submitted OpenJD template. */
export type TemplateFormat = 'yaml' | 'json'
/** Backing storage kind for a storage location. */
export type StorageLocationType = 'filesystem' | 's3' | 'mixed'
/** Source/origin of a product in the catalog. */
export type ProductSource = 'builtin' | 'custom' | 'installed'

// ── Meta ──────────────────────────────────────────────────────────────────────

/** Wire shape returned by GET /api/v1/version. */
export interface VersionInfo {
  version: string
  commit: string
  build_date: string
  go_version: string
}

// ── Pagination ────────────────────────────────────────────────────────────────

/** Generic paginated list response returned by all list endpoints. */
export interface ListResponse<T> {
  items: T[]
  total: number
  limit: number
  offset: number
}

// ── Job ───────────────────────────────────────────────────────────────────────

/** Wire shape returned by GET /api/v1/jobs and POST /api/v1/jobs. */
export interface Job {
  /** Aggregate completion across the job's tasks, 0-100; null when the job has no non-canceled tasks. */
  progress_percent?: number | null
  id: string
  farm_id: string
  queue_id: string
  queue_name?: string
  name: string
  owner: string
  submitter: string
  priority: number
  status: JobStatus
  project?: string
  template_format: TemplateFormat
  task_counts?: TaskCounts
  created_at: string
  updated_at: string
  started_at?: string
  completed_at?: string
  /** Cumulative count of genuine task failures for this job. */
  failed_attempts: number
  /** Set when the failure-limit sweep auto-parked the job; empty for a manual pause. */
  park_reason?: string
  /** Per-job override for the maximum attempts per task; absent means inherit (queue -> farm -> server default). */
  max_attempts?: number
  /** Per-job override for the delay before retrying a failed task; absent means inherit. */
  retry_delay_seconds?: number
  /** Per-job override for the number of task failures that auto-parks the job; absent means inherit. */
  failure_limit?: number
  /** IDs of upstream jobs this job is waiting on; present (non-empty) on job detail, omitted on list. */
  depends_on?: string[]
  /** Submitter- or preset-supplied estimate of total task run time summed over all tasks, in seconds (not wall-clock). */
  estimated_seconds?: number
  /** Name of the product the job was submitted from, when it came from one. */
  product_name?: string
  /** Version of that product. */
  product_version?: string
  /** Seconds of work left; null when unknown. Present on job list items and job detail; absent from create and update responses. */
  remaining_seconds?: number | null
  /** Where `remaining_seconds` comes from; omitted when unknown. */
  remaining_basis?: RemainingBasis
  /** Server time (RFC 3339) the remaining figure was computed; the client counts down from here. */
  as_of?: string
  /**
   * Client-only: local time (epoch ms) this row arrived from the server. The remaining-time
   * countdown anchors here so patching a cached row (WebSocket events) cannot restart it.
   */
  received_at?: number
}

/** How a job's remaining time was derived. */
export type RemainingBasis = 'finished' | 'progress' | 'measured' | 'estimate'

/**
 * Per-status task count summary. Optional on {@link Job} (list endpoints may
 * omit it) and required on {@link JobDetail}.
 */
export interface TaskCounts {
  total: number
  pending: number
  ready: number
  assigned: number
  running: number
  succeeded: number
  failed: number
  canceled: number
  /** Count of tasks currently blocked from scheduling (subset of `ready`). */
  unschedulable?: number
}

/** Aggregate summary of task failures for a job; absent when no tasks have failed. */
export interface FailureSummary {
  failed_count: number
  dominant_reason?: string
  distinct_reasons: number
}

/**
 * Resolved retry policy after job -> queue -> farm -> server-default
 * inheritance; all values are concrete (a `failure_limit` of 0 means the
 * auto-park failure limit is off).
 */
export interface EffectiveRetryPolicy {
  max_attempts: number
  retry_delay_seconds: number
  failure_limit: number
}

/** Wire shape returned by GET /api/v1/jobs/{id}. */
export interface JobDetail extends Job {
  steps: Step[]
  task_counts: TaskCounts
  failure_summary?: FailureSummary
  /** Resolved retry policy; absent when farm/queue lookups fail server-side. */
  effective_retry?: EffectiveRetryPolicy
}

// ── Step ──────────────────────────────────────────────────────────────────────

/** Wire shape for a step within a job detail response. */
export interface Step {
  id: string
  name: string
  step_order: number
  status: StepStatus
  depends_on?: string[]
  created_at: string
  updated_at: string
}

// ── Task ──────────────────────────────────────────────────────────────────────

/** Wire shape returned by GET /api/v1/tasks/{id} and task list endpoints. */
export interface Task {
  id: string
  job_id: string
  step_id: string
  name: string
  parameters?: Record<string, string>
  status: TaskStatus
  assigned_worker_id?: string
  assigned_at?: string
  /** Set while the task cannot currently be scheduled (e.g. no online workers with matching capabilities); absent when schedulable. */
  unschedulable_reason?: string
  created_at: string
  updated_at: string
  /** Count of attempts that genuinely ran and failed for this task. */
  failed_attempts?: number
  /** Set and in the future while a ready task is backing off after a failed attempt. */
  retry_after?: string
  /** Human-readable reason the task's most recent attempt failed; set only for failed tasks. */
  failure_reason?: string
  /** Self-reported completion, 0-100; absent until the task reports progress. */
  progress?: number
  /** Free-text status line the task reported alongside its progress. */
  status_text?: string
  /** When the task last reported progress. */
  progress_at?: string
}

/** Wire shape returned by POST /api/v1/tasks/{id}/retry. */
export interface RetryResponse {
  task_id: string
  status: TaskStatus
}

/** Wire shape returned by POST /api/v1/jobs/{id}/retry. */
export interface RetryJobResponse {
  job_id: string
  retried: number
}

/** Wire shape returned by POST /api/v1/tasks/{id}/cancel. */
export interface CancelResponse {
  task_id: string
  status: TaskStatus
}

// ── Task log ──────────────────────────────────────────────────────────────────

/** Wire shape for a single log chunk. */
export interface TaskLog {
  id: string
  task_id: string
  attempt_id: string
  seq_num: number
  nats_seq: number
  stream: LogStream
  data: string
  at: string
  received_at: string
}

/** Wire shape returned by GET /api/v1/tasks/{id}/logs (non-streaming). */
export interface TaskLogsResponse {
  items: TaskLog[]
  after_nats_seq: number
  limit: number
}

// ── Task attempt ──────────────────────────────────────────────────────────────

/** Wire shape for a single task execution attempt. */
export interface TaskAttempt {
  /** 1-based; incremented on each retry. */
  attempt_number: number
  status: AttemptStatus
  /** ID of the worker that ran this attempt. */
  worker_id?: string
  /** Process exit code; absent while running or if signaled. */
  exit_code?: number
  /** Human-readable reason for a terminal attempt. */
  message?: string
  started_at: string
  /** Absent while the attempt is running. */
  ended_at?: string
  /** Last self-reported completion for this attempt, 0-100. */
  progress?: number
  /** Last free-text status line the attempt reported. */
  status_text?: string
  /** When the attempt last reported progress. */
  progress_at?: string
}

/** Wire shape returned by GET /api/v1/tasks/{id}/attempts (oldest first). */
export interface TaskAttemptsResponse {
  items: TaskAttempt[]
}

// ── Worker ────────────────────────────────────────────────────────────────────

/** GPU capability info reported by a worker. */
export interface GPUInfo {
  vendor?: string
  model?: string
  vram_mb?: number
  count?: number
}

/** Wire shape returned by GET /api/v1/workers and worker list endpoint. */
export interface Worker {
  id: string
  farm_id: string
  queue_id?: string
  /**
   * Human-readable display label (the worker's worker.name config, default the
   * hostname). Distinguishes multiple workers on one host. May be empty; fall
   * back to {@link hostname}.
   */
  name?: string
  hostname: string
  ip_address?: string
  compute_location?: string
  os?: string
  os_version?: string
  /**
   * CPU architecture as the worker reports it (Go's GOARCH: 'amd64', 'arm64').
   * An OpenJD `attr.worker.cpu.arch` requirement uses the spec's tokens
   * ('x86_64', 'arm64') instead; the scheduler translates. Absent when the
   * worker does not report it.
   */
  arch?: string
  /** sqi-worker build version the worker self-reports; empty/absent if unknown. */
  version?: string
  cpu_count?: number
  ram_mb?: number
  gpu: GPUInfo
  tags?: Record<string, string>
  status: WorkerStatus
  /**
   * Server-authoritative flag: true when the worker may be hard-deleted via
   * DELETE /workers/{id} — offline workers, disabled or not (a disabled worker
   * counts as offline once the heartbeat sweep finds it gone). Online workers,
   * disabled or not, are never removable.
   */
  removable: boolean
  /**
   * Where the worker is in its capacity group's power cycle, apart from its
   * liveness (`status`). `awake` for every worker no capacity group has moved.
   */
  power_state: PowerState
  /** When `power_state` last changed; absent when it has not changed since creation. */
  power_state_at?: string
  /** Why the power state is what it is (a failed wake, an idle retirement); absent when empty. */
  power_reason?: string
  /** The cloud provider's identifier for the worker's machine; absent when it reported none. */
  cloud_instance_id?: string
  /**
   * Whether the worker reported a network interface with a hardware address,
   * which a wake-on-LAN packet is addressed to. The interfaces themselves are
   * in {@link WorkerDetail} only.
   */
  has_wake_address: boolean
  last_heartbeat_at?: string
  registered_at: string
  updated_at: string
}

/** A worker's position in its capacity group's power cycle. */
export type PowerState = 'awake' | 'waking' | 'retiring' | 'asleep' | 'wake_failed'

/** One network interface of a worker's machine. */
export interface NetInterface {
  name: string
  /** The hardware address, such as "aa:bb:cc:dd:ee:ff"; may be empty. */
  mac: string
  /** Addresses in CIDR form; empty when the interface has none. */
  addrs: string[]
}

/** Minimal active-task info embedded in {@link WorkerDetail}. */
export interface CurrentTask {
  id: string
  job_id: string
  name: string
  status: TaskStatus
  assigned_at?: string
}

/** Wire shape returned by GET /api/v1/workers/{id}. */
export interface WorkerDetail extends Worker {
  /** The network interfaces reported at registration (what a wake-on-LAN packet is addressed with). */
  interfaces: NetInterface[]
  /** Every task the worker is currently executing; empty when idle. */
  current_tasks: CurrentTask[]
}

/** Wire shape returned by POST /api/v1/workers/{id}/disable|enable. */
export interface WorkerActionResponse {
  id: string
  status: WorkerStatus
}

// ── Farm ──────────────────────────────────────────────────────────────────────

/** Wire shape returned by farm endpoints. */
export interface Farm {
  id: string
  name: string
  description?: string
  max_concurrent_tasks: number
  created_at: string
  updated_at: string
  /** Farm-level default for the maximum attempts per task; absent means inherit the server default. */
  max_attempts?: number
  /** Farm-level default for the delay before retrying a failed task; absent means inherit the server default. */
  retry_delay_seconds?: number
  /** Farm-level default for the number of task failures that auto-parks a job; absent means inherit the server default. */
  failure_limit?: number
}

// ── Queue ─────────────────────────────────────────────────────────────────────

/** Wire shape returned by queue endpoints. */
export interface Queue {
  id: string
  farm_id: string
  name: string
  description?: string
  priority: number
  max_concurrent_tasks: number
  paused: boolean
  created_at: string
  updated_at: string
  /** Queue-level override for the maximum attempts per task; absent means inherit (farm -> server default). */
  max_attempts?: number
  /** Queue-level override for the delay before retrying a failed task; absent means inherit. */
  retry_delay_seconds?: number
  /** Queue-level override for the number of task failures that auto-parks a job; absent means inherit. */
  failure_limit?: number
  /** Number of jobs in the queue that are not yet finished. */
  unfinished_jobs?: number
  /** Total seconds of work left across the queue's unfinished jobs with a known figure. */
  work_remaining_seconds?: number
  /** Unfinished jobs that contribute nothing to `work_remaining_seconds` for want of an estimate. */
  jobs_without_estimate?: number
  /** Tasks currently running in the queue. */
  running_tasks?: number
  /** Server time (RFC 3339) these figures were computed. */
  as_of?: string
}

// ── Storage location ──────────────────────────────────────────────────────────

/** Wire shape returned by storage location endpoints. */
export interface StorageLocation {
  id: string
  name: string
  type: StorageLocationType
  description?: string
  roots?: Record<string, string>
  created_at: string
  updated_at: string
}

// ── Compute location ──────────────────────────────────────────────────────────

/** Wire shape returned by compute location endpoints. */
export interface ComputeLocation {
  id: string
  name: string
  description?: string
  worker_count: number
  created_at: string
  updated_at: string
}

// ── Usage pool ────────────────────────────────────────────────────────────────

/** Wire shape returned by usage pool endpoints. */
export interface UsagePool {
  id: string
  name: string
  server_hint?: string
  max_concurrent: number
  /** Slots currently claimed (active, unreleased). */
  in_use: number
  /** Free slots: max(max_concurrent - in_use, 0). */
  available: number
  created_at: string
  updated_at: string
}

// ── Capacity group ────────────────────────────────────────────────────────────

/** `named` groups hold specific machines added by name; `sized` groups hold interchangeable ones. */
export type CapacityGroupKind = 'named' | 'sized'

/** What one machine of a capacity group offers; a field left out is not declared. */
export interface MachineProfile {
  cpu_count?: number
  ram_mb?: number
  gpu_count?: number
  gpu_memory_mb?: number
  os?: string
  arch?: string
  compute_location?: string
  tags?: Record<string, string>
  extensions?: string[]
  /** The EXPR caps the machine's worker enforces. Not edited in the UI; kept on a save. */
  expr_limits?: Record<string, number>
}

/** The scheduler's last evaluation of a capacity group. */
export interface CapacityGroupStatus {
  demand_cores: number
  /** The total machine count the group should have, retire candidates included. */
  desired_machines: number
  online_members: number
  offline_members: number
  /** Members in each non-awake power state, whatever their liveness and disabled flag. */
  waking: number
  retiring: number
  asleep: number
  wake_failed: number
  /** A named group's members to wake now, by worker ID; empty for a sized group. */
  wake_candidates: string[]
  /** Worker IDs idle past the group's idle timeout, longest idle first. */
  retire_candidates: string[]
  mismatches: Array<{ worker_id: string; reason: string }>
  /** The size a sized `script` group last asked its provider for; null for every other group. */
  requested_machines: number | null
  /** Why the group stopped growing, and until when; null while it is not degraded. */
  degraded: CapacityGroupDegraded | null
  /** The group's latest provider action; null before its first. */
  last_action: CapacityGroupAction | null
  evaluated_at: string
}

export interface CapacityGroupDegraded {
  reason: string
  since: string
  /** The group asks its provider for no more machines before this time. */
  backoff_until: string
}

export type CapacityActionName = 'wake' | 'grow' | 'retire' | 'list' | 'configure'
export type CapacityActionResult =
  'ok' | 'failed' | 'refused' | 'unsupported' | 'timeout' | 'expired'

export interface CapacityGroupAction {
  action: CapacityActionName
  result: CapacityActionResult
  /** What the provider or the server said about the result; may be empty. */
  message: string
  at: string
}

/** A provider a capacity group may name, as `GET /capacity-providers` lists it. */
export interface CapacityProvider {
  /** `external`, `wake_on_lan`, or `script:<name>`. */
  name: string
  /** The group kinds that may name the provider. */
  kinds: CapacityGroupKind[]
}

/** Wire shape returned by capacity group endpoints. */
export interface CapacityGroup {
  id: string
  farm_id: string
  name: string
  kind: CapacityGroupKind
  provider: string
  enabled: boolean
  queue_ids: string[]
  min_machines: number
  max_machines: number
  priority: number
  max_step_up: number
  scale_up_cooldown_seconds: number
  idle_timeout_seconds: number
  boot_timeout_seconds: number
  retire_action: 'sleep' | 'shutdown'
  machine_profile: MachineProfile
  created_at: string
  updated_at: string
  /** Null until the scheduler has evaluated the group, while it is disabled, and on a standby server. */
  status: CapacityGroupStatus | null
}

/** A group join token as listed; it carries no secret. */
export interface CapacityGroupToken {
  id: string
  name: string
  prefix: string
  expires_at: string
  revoked_at: string | null
  created_by: string
  created_at: string
}

/** The create-only shape carrying the raw token, returned exactly once. */
export interface CapacityGroupTokenCreated {
  id: string
  token: string
  prefix: string
  name: string
  expires_at: string
  created_at: string
}

// ── Path translation ──────────────────────────────────────────────────────────

/** Delivery mechanism kind for the SQI_PATH_TRANSLATION extension. */
export type PathDeliveryKind =
  'swap_in_place' | 'translation_file' | 'command_flags' | 'environment' | 'stage_locally'

/** A single path-delivery entry inside SQI_PATH_TRANSLATION.deliveries. */
export interface PathDelivery {
  kind: PathDeliveryKind
  pattern?: string
  variable?: string
}

/** Parsed SQI_PATH_TRANSLATION block from an OpenJD template. */
export interface PathTranslation {
  deliveries: PathDelivery[]
}

// ── Preset ────────────────────────────────────────────────────────────────────

/** Installation status of a preset in the catalog. */
export type PresetStatus = 'not_installed' | 'installed' | 'update_available'

/** Wire shape returned by GET /api/v1/presets (list item). */
export interface PresetListItem {
  name: string
  title: string
  description: string
  category: string
  version: string
  status: PresetStatus
}

/** Wire shape returned by GET /api/v1/presets/{name} (detail). */
export interface PresetDetail extends PresetListItem {
  readme: string
  template: string
  format: string
}

// ── Product ───────────────────────────────────────────────────────────────────

/** Wire shape returned by GET /api/v1/products and GET /api/v1/products/{name}. */
export interface Product {
  name: string
  title: string
  description: string
  readme: string
  category: string
  version: string
  source: ProductSource
  template: string
  format: TemplateFormat
}

export type ControlType =
  | 'LINE_EDIT'
  | 'MULTILINE_EDIT'
  | 'DROPDOWN_LIST'
  | 'CHECK_BOX'
  | 'HIDDEN'
  | 'SPIN_BOX'
  | 'CHOOSE_INPUT_FILE'
  | 'CHOOSE_OUTPUT_FILE'
  | 'CHOOSE_DIRECTORY'
  // Defined by the EXPR extension (RFC 0007) for the list parameter types.
  | 'LINE_EDIT_LIST'
  | 'SPIN_BOX_LIST'
  | 'CHECK_BOX_LIST'
  | 'CHOOSE_INPUT_FILE_LIST'
  | 'CHOOSE_OUTPUT_FILE_LIST'
  | 'CHOOSE_DIRECTORY_LIST'

/** OpenJD base-spec userInterface hints on a job parameter. */
export interface ParameterUserInterface {
  control: ControlType | ''
  label: string
  group_label: string
  decimals: number | null
}

/** One named file type offered by a PATH parameter's chooser dialog. */
export interface PathFileFilter {
  label: string
  patterns: string[]
}

/** Per-element constraints on a LIST[*] parameter (RFC 0007's nested item:
 * block). `item` recurses exactly once, for LIST[LIST[INT]]. */
export interface ItemConstraint {
  allowed_values: string[] | null
  min_value: string | null
  max_value: string | null
  min_length: number | null
  max_length: number | null
  item: ItemConstraint | null
}

/** Declared type of a job parameter. The eight beyond the base spec's four come
 * from the EXPR extension (RFC 0007). */
export type ParameterType =
  | 'INT'
  | 'FLOAT'
  | 'STRING'
  | 'PATH'
  | 'BOOL'
  | 'RANGE_EXPR'
  | 'LIST[STRING]'
  | 'LIST[PATH]'
  | 'LIST[INT]'
  | 'LIST[FLOAT]'
  | 'LIST[BOOL]'
  | 'LIST[LIST[INT]]'

/** Parsed job parameter from GET /products/{name}/parameters. */
export interface ProductParameter {
  name: string
  type: ParameterType
  description: string
  default: string | null
  allowed_values: string[] | null
  min_value: string | null
  max_value: string | null
  min_length: number | null
  max_length: number | null
  object_type: string
  data_flow: string
  user_interface: ParameterUserInterface | null
  file_filters: PathFileFilter[] | null
  file_filter_default: PathFileFilter | null
  item: ItemConstraint | null
}

/** Input for the submitProductJob mutation. */
export interface SubmitProductJobInput {
  productName: string
  name: string
  farmId: string
  queueId: string
  owner?: string
  /**
   * UX affordance only, not enforcement: when false/omitted, `owner` is
   * dropped from the request so the client never provokes a server 403 for a
   * caller lacking `jobs.submit_as`. The server independently derives and
   * authorizes the submitter regardless of what the client sends.
   */
  maySubmitAs?: boolean
  priority?: number
  project?: string
  parameters: Record<string, string>
  /** Per-job override for the maximum attempts per task; omitted means inherit. */
  maxAttempts?: number
  /** Per-job override for the retry delay (seconds); omitted means inherit. */
  retryDelaySeconds?: number
  /** Per-job override for the auto-park failure count; omitted means inherit. */
  failureLimit?: number
  /** IDs of upstream jobs this job waits for (whole-job, same farm). */
  dependsOn?: string[]
}

// ── Auth ──────────────────────────────────────────────────────────────────────

/** The current authenticated principal, from GET /auth/me. */
export interface Principal {
  subject: string
  display_name: string
  roles: string[]
  kind: string
  username?: string
  permissions: string[]
}

/** Body of PUT /auth/password. */
export interface ChangePasswordInput {
  current_password: string
  new_password: string
}

/**
 * Body of PATCH /auth/me. Only display_name is accepted by the server —
 * role, disabled, and username are ignored, so this cannot escalate.
 */
export interface UpdateMeInput {
  display_name: string
}

/**
 * One single-sign-on option advertised by `GET /auth/providers`.
 *
 * `login_url` is a navigation target, not a fetch target: the flow leaves this
 * page for the identity provider. Fetching it would follow the redirect in the
 * background and land the provider's login HTML in a response body, where
 * nobody can see or interact with it.
 */
export interface SSOProvider {
  id: string
  label: string
  login_url: string
}

/** Response of `GET /auth/providers` — the login methods this server offers. */
export interface AuthProviders {
  password: boolean
  sso: SSOProvider[]
}

/**
 * Response of `POST /auth/logout`. `redirect_url` is present only when the
 * server is configured for provider-side logout (`auth.oidc.logout_mode:
 * provider`) and the provider advertises an end-session endpoint; the client
 * must navigate to it, or the provider-side half of the logout never happens.
 */
export interface LogoutResult {
  redirect_url?: string
}

/** Credential backend for a user account (mirrors store.AuthSource). */
export type AuthSource = 'local' | 'ldap' | 'oidc'

/** A user account (secrets are never included in the wire format). */
export interface User {
  id: string
  username: string
  display_name?: string
  role: string
  /**
   * Which backend verifies this account. `ldap` and `oidc` accounts are
   * provisioned on first directory/SSO login; their role may be
   * directory-managed, in which case the server rejects a role edit with 409.
   */
  auth_source: AuthSource
  /**
   * Whether `PATCH /users/{id}` accepts a `role` change for this account.
   * Computed by the server from both halves of its guard: `auth_source` AND
   * that source's own `role_source` setting (`auth.ldap.role_source` for
   * `ldap`, `auth.oidc.role_source` for `oidc`). Do not re-derive it from
   * `auth_source` alone: `role_source: local` leaves an LDAP or OIDC
   * account's role editable, and the server never exposes `role_source` for
   * a client to check.
   */
  role_editable: boolean
  /**
   * Whether `PUT /users/{id}/password` accepts a new password for this
   * account. Computed by the server from the same predicate that guards that
   * endpoint (and the self-service `PUT /auth/password`): true only for
   * `auth_source: local`, since an `ldap` or `oidc` account has no local
   * password and either endpoint answers `409`. Read it rather than testing
   * `auth_source` yourself, so the control and the guard cannot drift apart.
   */
  password_editable: boolean
  disabled: boolean
  created_at: string
  updated_at: string
}

/** Input for POST /auth/login. */
export interface LoginRequest {
  username: string
  password: string
}

/** An API key (metadata only; the secret is returned once, at creation). */
export interface ApiKey {
  id: string
  name: string
  prefix: string
  expires_at?: string
  last_used_at?: string
  created_at: string
}

/** Response of POST /api-keys — the only place the raw secret appears. */
export interface ApiKeyCreated extends ApiKey {
  secret: string
}

// ── Job submission ────────────────────────────────────────────────────────────

/** Input for the submitJob mutation. */
export interface SubmitJobInput {
  farmId: string
  queueId: string
  template: string
  format: TemplateFormat
  owner?: string
  submitter?: string
  /**
   * UX affordance only, not enforcement: when false/omitted, `owner` and
   * `submitter` are dropped from the request so the client never provokes a
   * server 403 for a caller lacking `jobs.submit_as`. The server
   * independently derives and authorizes the submitter regardless of what
   * the client sends.
   */
  maySubmitAs?: boolean
  priority?: number
  project?: string
  /** Per-job override for the maximum attempts per task; omitted means inherit. */
  maxAttempts?: number
  /** Per-job override for the delay (seconds) before retrying a failed task; omitted means inherit. */
  retryDelaySeconds?: number
  /** Per-job override for the number of task failures that auto-parks the job; omitted means inherit. */
  failureLimit?: number
  /** IDs of upstream jobs this job waits for (whole-job, same farm). */
  dependsOn?: string[]
}

// ── Servers (high availability) ───────────────────────────────────────────────

/** A server's place in its high-availability group. */
export type ServerRole = 'leader' | 'standby' | 'missing'

/** One sqi-server in the farm's high-availability group (mirrors OpenAPI `ServerMember`). */
export interface ServerMember {
  /** Identifies this run of the server; a restart is a new instance. */
  instance_id: string
  name: string
  host: string
  pid: number
  version: string
  role: ServerRole
  /** RFC 3339 timestamp. */
  started_at: string
  /** Seconds since the server last refreshed its membership row, on the database's clock. */
  last_seen_seconds_ago: number
  /** Whether the server reached its message broker within `ha.lease_ttl`. */
  broker_ok: boolean
}

/** Response of GET /servers (mirrors OpenAPI `ServersResponse`). */
export interface ServersResponse {
  /** Whether this farm runs a high-availability group. */
  ha: boolean
  /** The current leadership epoch; 0 without HA. */
  epoch: number
  /** Leader first, then by name. */
  servers: ServerMember[]
}
