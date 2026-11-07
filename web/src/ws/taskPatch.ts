// SPDX-License-Identifier: AGPL-3.0-or-later

import type { Task } from '@/api/types'
import type { WsTaskEvent } from './events'

/** Statuses in which a task has no live progress to report. */
const PROGRESS_CLEARING: ReadonlySet<string> = new Set(['pending', 'ready', 'assigned'])

/**
 * Applies a WebSocket task event to a cached task row. An absent
 * `unschedulable_reason` means "cleared"; absent `progress`/`status_text`
 * mean "unchanged" except when the task returns to a pre-run status, which
 * drops all progress fields.
 */
export function patchTaskFromEvent(prev: Task, ev: WsTaskEvent): Task {
  const patched: Task = {
    ...prev,
    status: ev.status,
    updated_at: ev.updated_at,
    ...(ev.worker_id ? { assigned_worker_id: ev.worker_id } : {}),
  }
  if (ev.unschedulable_reason !== undefined) {
    patched.unschedulable_reason = ev.unschedulable_reason
  } else {
    delete patched.unschedulable_reason
  }
  if (ev.progress !== undefined) patched.progress = ev.progress
  if (ev.status_text !== undefined) patched.status_text = ev.status_text
  if (PROGRESS_CLEARING.has(ev.status)) {
    delete patched.progress
    delete patched.status_text
    delete patched.progress_at
  }
  return patched
}

/** True when the event reports the same status the cached task already has. */
export function isProgressOnly(prev: Task | undefined, ev: WsTaskEvent): boolean {
  return prev !== undefined && prev.status === ev.status
}
