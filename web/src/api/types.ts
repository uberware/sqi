// SPDX-License-Identifier: AGPL-3.0-or-later

// Domain types matching the sqi REST API wire format (JSON field names).
// Keep in sync with the OpenAPI spec at GET /api/v1/openapi.yaml and with the
// Go wire-format types in internal/api/*.go.

// ── Status enums ──────────────────────────────────────────────────────────────

/** Aggregate lifecycle status of a job. */
export type JobStatus = 'pending' | 'running' | 'paused' | 'completed' | 'failed' | 'canceled'
/** Aggregate status of a step within a job. */
export type StepStatus = 'pending' | 'ready' | 'running' | 'completed' | 'failed' | 'canceled'
/** Lifecycle status of an individual task. */
export type TaskStatus =
  | 'pending'
  | 'ready'
  | 'assigned'
  | 'running'
  | 'succeeded'
  | 'failed'
  | 'canceled'
/** Terminal/active status of a single task execution attempt. */
export type AttemptStatus = 'running' | 'succeeded' | 'failed' | 'canceled'
/** Connectivity/administrative status of a worker node. */
export type WorkerStatus = 'online' | 'offline' | 'disabled'
/** Output stream a log chunk came from. */
export type LogStream = 'stdout' | 'stderr'
/** Wire format of a submitted OpenJD template. */
export type TemplateFormat = 'yaml' | 'json'
/** Backing storage kind for a storage location. */
export type StorageLocationType = 'filesystem' | 's3'
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
}

/** Wire shape returned by GET /api/v1/jobs/{id}. */
export interface JobDetail extends Job {
  steps: Step[]
  task_counts: TaskCounts
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
  created_at: string
  updated_at: string
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
  id: string
  task_id: string
  worker_id: string
  session_id: string
  attempt_number: number
  status: AttemptStatus
  exit_code?: number
  started_at: string
  ended_at?: string
  created_at: string
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

// ── Job submission ────────────────────────────────────────────────────────────

/** Input for the submitJob mutation. */
export interface SubmitJobInput {
  farmId: string
  queueId: string
  template: string
  format: TemplateFormat
  owner?: string
  submitter?: string
  priority?: number
  project?: string
}
