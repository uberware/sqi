// SPDX-License-Identifier: AGPL-3.0-or-later

// Wire payload types for WebSocket push events.
// These mirror the hub's internal wire structs (internal/ws/hub.go) serialised
// to JSON.  Keep in sync with that file when the event shapes change.

import type { JobStatus, TaskStatus, WorkerStatus } from '@/api/types'

/** Payload received on the "jobs" subject when a job changes aggregate status. */
export interface WsJobEvent {
  job_id: string
  name?: string
  owner?: string
  queue_id?: string
  status: JobStatus | typeof JOB_REMOVED_STATUS
  updated_at: string
}

/**
 * Payload received when a task changes status. Delivered on the
 * "jobs/{jobId}/tasks" subject (per-job detail view) and, with the same shape,
 * on the aggregate "jobs" subject so list subscribers can update progress
 * without subscribing to every job. Distinguished from {@link WsJobEvent} by
 * the presence of `task_id`.
 */
export interface WsTaskEvent {
  job_id: string
  task_id: string
  name?: string
  status: TaskStatus
  worker_id?: string
  updated_at: string
}

/**
 * Synthetic status value delivered on the "jobs" subject when a job row is
 * hard-deleted (manual delete or the job-retention sweep). Not a real
 * {@link JobStatus}; subscribers should drop the job rather than update it.
 */
export const JOB_REMOVED_STATUS = 'removed'

/**
 * Synthetic status value delivered on the "workers" subject when a worker row
 * is hard-deleted (manual remove or the offline-retention sweep). Not a real
 * {@link WorkerStatus}; subscribers should drop the worker rather than update it.
 */
export const WORKER_REMOVED_STATUS = 'removed'

/** Payload received on the "workers" subject when a worker status changes. */
export interface WsWorkerEvent {
  worker_id: string
  name?: string
  hostname?: string
  farm_id?: string
  status: WorkerStatus | typeof WORKER_REMOVED_STATUS
}

/** Returns true when payload is a WsJobEvent (has job_id but no task_id). */
export function isJobEvent(payload: unknown): payload is WsJobEvent {
  return (
    typeof payload === 'object' &&
    payload !== null &&
    'job_id' in payload &&
    'status' in payload &&
    !('task_id' in payload)
  )
}

/** Returns true when payload is a WsTaskEvent (has both job_id and task_id). */
export function isTaskEvent(payload: unknown): payload is WsTaskEvent {
  return (
    typeof payload === 'object' &&
    payload !== null &&
    'job_id' in payload &&
    'task_id' in payload &&
    'status' in payload
  )
}

/** Returns true when payload is a WsWorkerEvent (has worker_id and status). */
export function isWorkerEvent(payload: unknown): payload is WsWorkerEvent {
  return (
    typeof payload === 'object' && payload !== null && 'worker_id' in payload && 'status' in payload
  )
}
