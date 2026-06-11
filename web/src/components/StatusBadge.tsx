// SPDX-License-Identifier: AGPL-3.0-or-later

import type { JobStatus, TaskStatus, StepStatus, AttemptStatus, WorkerStatus } from '@/api/types'
import styles from './StatusBadge.module.css'

export type BadgeStatus = JobStatus | TaskStatus | StepStatus | AttemptStatus | WorkerStatus

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
  online: 'Online',
  offline: 'Offline',
  disabled: 'Disabled',
}

interface StatusBadgeProps {
  status: BadgeStatus
  className?: string
}

export default function StatusBadge({ status, className }: StatusBadgeProps) {
  const modifierClass = styles[status] ?? ''
  return (
    <span
      className={[styles.badge, modifierClass, className].filter(Boolean).join(' ')}
      aria-label={`Status: ${LABELS[status] ?? status}`}
    >
      {LABELS[status] ?? status}
    </span>
  )
}
