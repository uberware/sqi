// SPDX-License-Identifier: AGPL-3.0-or-later

import styles from './UnschedulableBadge.module.css'

interface UnschedulableBadgeProps {
  /** Human-readable reason the task cannot currently be scheduled. */
  reason: string
  className?: string
}

/**
 * Small warning pill shown next to a task's {@link StatusBadge} while the task
 * is blocked from scheduling (e.g. no online workers with matching
 * capabilities). Deliberately separate from `StatusBadge` — that component is
 * a closed union keyed by lifecycle status, and "unschedulable" is an
 * orthogonal, transient condition rather than a status value.
 */
export default function UnschedulableBadge({ reason, className }: UnschedulableBadgeProps) {
  return (
    <span
      className={[styles.badge, className].filter(Boolean).join(' ')}
      title={reason}
      aria-label={`Unschedulable: ${reason}`}
    >
      Unschedulable
    </span>
  )
}
