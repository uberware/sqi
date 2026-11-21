// SPDX-License-Identifier: AGPL-3.0-or-later

import type { Job, JobStatus, TaskCounts, TaskStatus } from '@/api/types'

/** Job statuses that are final: the job will not run again. */
export const TERMINAL_JOB_STATUSES: ReadonlySet<string> = new Set([
  'completed',
  'failed',
  'canceled',
])

/**
 * Job statuses the server cancels (`POST /jobs/{id}/cancel`): every unfinished
 * one. `cancelJob` in `internal/api/jobs.go` refuses completed and failed jobs,
 * and a canceled one is already done.
 */
export const CANCELABLE_JOB_STATUSES: ReadonlySet<JobStatus> = new Set([
  'pending',
  'running',
  'paused',
  'blocked',
])

/**
 * Job statuses the server accepts a pause for: `resolveAction` in
 * `internal/api/jobs.go`.
 */
export const PAUSABLE_JOB_STATUSES: ReadonlySet<JobStatus> = new Set([
  'pending',
  'running',
  'blocked',
])

/**
 * Job statuses the server accepts a resume for: `resolveAction` in
 * `internal/api/jobs.go`.
 */
export const RESUMABLE_JOB_STATUSES: ReadonlySet<JobStatus> = new Set(['paused'])

/**
 * Task statuses the server retries, for a whole job (`POST /jobs/{id}/retry`)
 * or a single task (`POST /tasks/{id}/retry`).
 */
export const RETRYABLE_TASK_STATUSES: ReadonlySet<TaskStatus> = new Set(['failed', 'canceled'])

/** Number of a job's tasks a retry would re-queue. */
export function retryableTaskCount(counts: TaskCounts | undefined): number {
  return counts === undefined ? 0 : counts.failed + counts.canceled
}

/** The job fields the action rules read. */
type JobState = Pick<Job, 'status'> & Partial<Pick<Job, 'task_counts'>>

export const canPauseJob = (job: JobState) => PAUSABLE_JOB_STATUSES.has(job.status)
export const canResumeJob = (job: JobState) => RESUMABLE_JOB_STATUSES.has(job.status)
export const canCancelJob = (job: JobState) => CANCELABLE_JOB_STATUSES.has(job.status)
export const canRetryJob = (job: JobState) => retryableTaskCount(job.task_counts) > 0

/** The job actions the UI offers; also the verb its failure banners name. */
export type JobAction = 'Pause' | 'Resume' | 'Cancel' | 'Retry' | 'Delete'
