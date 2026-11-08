// SPDX-License-Identifier: AGPL-3.0-or-later

/** Job statuses that are final: the job will not run again. */
export const TERMINAL_JOB_STATUSES: ReadonlySet<string> = new Set([
  'completed',
  'failed',
  'canceled',
])
