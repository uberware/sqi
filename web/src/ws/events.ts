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
  status: JobStatus
  updated_at: string
}

/**
 * Payload received on the "jobs/{jobId}/tasks" subject when a task changes
 * status. (The "jobs" subject carries the aggregate {@link WsJobEvent}, not
 * this per-task event.)
 */
export interface WsTaskEvent {
  job_id: string
  task_id: string
  name?: string
  status: TaskStatus
  worker_id?: string
  updated_at: string
}

/** Payload received on the "workers" subject when a worker status changes. */
export interface WsWorkerEvent {
  worker_id: string
  name?: string
  hostname?: string
  farm_id?: string
  status: WorkerStatus
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
