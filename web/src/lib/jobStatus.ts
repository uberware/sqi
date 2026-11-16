// SPDX-License-Identifier: AGPL-3.0-or-later

import type { JobStatus } from '@/api/types'

/** Job statuses that are final: the job will not run again. */
export const TERMINAL_JOB_STATUSES: ReadonlySet<string> = new Set([
  'completed',
  'failed',
  'canceled',
])

/** Job statuses the list page offers Cancel for (`POST /jobs/{id}/cancel`). */
export const CANCELABLE_JOB_STATUSES: ReadonlySet<JobStatus> = new Set([
  'pending',
  'running',
  'paused',
])

/**
 * Job statuses the server accepts a pause for: `resolveAction` in
 * `internal/api/jobs.go`.
 */
export const PAUSABLE_JOB_STATUSES: ReadonlySet<JobStatus> = new Set(['pending', 'running'])

/**
 * Job statuses the server accepts a resume for: `resolveAction` in
 * `internal/api/jobs.go`.
 */
export const RESUMABLE_JOB_STATUSES: ReadonlySet<JobStatus> = new Set(['paused'])
