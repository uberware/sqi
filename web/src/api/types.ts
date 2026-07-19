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
}

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
  /** Count of tasks currently blocked from scheduling (subset of `ready`); optional for backward compatibility. */
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
   * hostname). Distinguishes multiple workers on one host. May be empty for
   * workers registered before this field existed; fall back to {@link hostname}.
   */
  name?: string
  hostname: string
  ip_address?: string
  compute_location?: string
  os?: string
  os_version?: string
  /** sqi-worker build version the worker self-reports; empty/absent if unknown. */
  version?: string
  cpu_count?: number
  ram_mb?: number
  gpu: GPUInfo
  tags?: Record<string, string>
  status: WorkerStatus
  /**
   * Server-authoritative flag: true when the worker may be hard-deleted via
   * DELETE /workers/{id} — offline workers, and disabled workers whose last
   * heartbeat is older than the heartbeat-timeout window (the machine is gone).
   * Online and live-disabled workers are never removable.
   */
  removable: boolean
  last_heartbeat_at?: string
  registered_at: string
  updated_at: string
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
  template: string
  format: string
}

// ── Product ───────────────────────────────────────────────────────────────────

/** Wire shape returned by GET /api/v1/products and GET /api/v1/products/{name}. */
export interface Product {
  name: string
  title: string
  description: string
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
  | 'CHIP_INPUT'
  | 'HIDDEN'
  | 'SPIN_BOX'

/** OpenJD base-spec userInterface hints on a job parameter. */
export interface ParameterUserInterface {
  control: ControlType | ''
  label: string
  group_label: string
  decimals: number | null
  single_step_removal: boolean | null
}

/** Parsed job parameter from GET /products/{name}/parameters. */
export interface ProductParameter {
  name: string
  type: 'INT' | 'FLOAT' | 'STRING' | 'PATH'
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

/** A local user account (secrets are never included in the wire format). */
export interface User {
  id: string
  username: string
  display_name?: string
  role: string
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
