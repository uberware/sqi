// SPDX-License-Identifier: AGPL-3.0-or-later

import type {
  JobStatus,
  TaskStatus,
  StepStatus,
  AttemptStatus,
  WorkerStatus,
  PowerState,
} from '@/api/types'
import styles from './StatusBadge.module.css'

export type BadgeStatus =
  JobStatus | TaskStatus | StepStatus | AttemptStatus | WorkerStatus | PowerState

const LABELS: Record<BadgeStatus, string> = {
  pending: 'Pending',
  ready: 'Ready',
  assigned: 'Assigned',
  running: 'Running',
  succeeded: 'Succeeded',
  completed: 'Completed',
  failed: 'Failed',
  canceled: 'Canceled',
  paused: 'Paused',
  blocked: 'Blocked',
  online: 'Online',
  offline: 'Offline',
  disabled: 'Disabled',
  awake: 'Awake',
  waking: 'Waking',
  retiring: 'Retiring',
  asleep: 'Asleep',
  wake_failed: 'Wake failed',
}

interface StatusBadgeProps {
  status: BadgeStatus
  /**
   * What the badge reports, which names it to assistive technology ("Status:
   * Online"). Set it where a second badge sits beside a status one, as a
   * worker's power state ("Power") does.
   */
  kind?: string
  className?: string
}

export default function StatusBadge({ status, kind = 'Status', className }: StatusBadgeProps) {
  const modifierClass = styles[status] ?? ''
  return (
    <span
      className={[styles.badge, modifierClass, className].filter(Boolean).join(' ')}
      aria-label={`${kind}: ${LABELS[status] ?? status}`}
    >
      {LABELS[status] ?? status}
    </span>
  )
}
