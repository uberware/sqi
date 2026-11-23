// SPDX-License-Identifier: AGPL-3.0-or-later

// Wire payload types for WebSocket push events.
// These mirror the hub's push structs (internal/ws/hub.go) serialised to JSON.
// Keep in sync with that file when the event shapes change.

import type { JobStatus, TaskStatus, WorkerStatus } from '@/api/types'

/** Payload received on the "jobs" subject when a job changes aggregate status. */
export interface WsJobEvent {
  job_id: string
  name?: string
  owner?: string
  queue_id?: string
  status: JobStatus | typeof JOB_REMOVED_STATUS
  /**
   * Set when the event is not a status change. `estimate`: only the job's
   * estimate moved, and `status` is the status the server read, which may
   * already be stale; refetch the job instead of applying it.
   */
  change?: typeof JOB_CHANGE_ESTIMATE
  updated_at: string
}

/** {@link WsJobEvent.change} of an event sent because a job's estimate moved. */
export const JOB_CHANGE_ESTIMATE = 'estimate'

/**
 * Payload received when a task changes status. Delivered on the
 * "jobs/{jobId}/tasks" subject (per-job detail view) and, without `worker_id`
 * and `unschedulable_reason`, on the aggregate "jobs" subject so list
 * subscribers can update progress without subscribing to every job. Distinguished from {@link WsJobEvent} by
 * the presence of `task_id`.
 */
export interface WsTaskEvent {
  job_id: string
  task_id: string
  name?: string
  status: TaskStatus
  worker_id?: string
  /** Present while the task cannot currently be scheduled; omitted (not empty) when cleared. */
  unschedulable_reason?: string
  /** Self-reported completion, 0-100; omitted when the task has not reported any. */
  progress?: number
  /** Free-text status line accompanying `progress`; omitted when empty. */
  status_text?: string
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

/**
 * Returns true when a WebSocket event is older than the job or task state
 * already held, so applying it would move that row backwards. A new subscriber
 * is replayed the server's buffered events, and that buffer can hold a row's
 * "running" event without the terminal one that followed it.
 *
 * The server stamps each event no earlier than the row write it reports, so an
 * event at the same instant as the held state still applies. That orders an
 * event against its own write only: when another write to the same row commits
 * first with a later stamp, a genuine event can be judged stale, and the row
 * then waits for the next refetch. An unparseable timestamp never counts as
 * stale.
 */
export function isStaleEvent(
  event: { updated_at: string },
  current: { updated_at: string },
): boolean {
  const eventAt = Date.parse(event.updated_at)
  const currentAt = Date.parse(current.updated_at)
  return !Number.isNaN(eventAt) && !Number.isNaN(currentAt) && eventAt < currentAt
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
