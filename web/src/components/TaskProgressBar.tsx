// SPDX-License-Identifier: AGPL-3.0-or-later

import type { TaskCounts } from '@/api/types'
import { clamp } from '@/lib/math'
import styles from './TaskProgressBar.module.css'

interface TaskProgressBarProps {
  /** Per-status task counts for the job. */
  counts: TaskCounts
  /** Job-level progress (0-100) including running tasks' own progress; null/absent when unknown. */
  progressPercent?: number | null | undefined
  /** Extra class for the outer wrapper (e.g. to adjust layout in a table cell). */
  className?: string | undefined
}

/** Segment width as a percentage of total, trimmed of floating-point noise. */
const widthPct = (n: number, total: number): string =>
  `${total > 0 ? Math.round((n / total) * 10000) / 100 : 0}%`

/**
 * Task-completion meter: a `done/total` fraction above a segmented bar with
 * one fill per outcome (succeeded, failed, canceled) plus the running tasks'
 * partial progress. "Done" counts terminal tasks (succeeded + failed + canceled).
 */
export default function TaskProgressBar({
  counts,
  progressPercent,
  className,
}: TaskProgressBarProps) {
  const { total, succeeded, failed, canceled } = counts
  const done = succeeded + failed + canceled
  const runningFill =
    progressPercent == null
      ? 0
      : clamp((progressPercent / 100) * (total - canceled) - succeeded, 0, counts.running)
  const pct = total > 0 ? Math.round(((done + runningFill) / total) * 100) : 0
  const segments = [
    { name: 'succeeded', n: succeeded },
    { name: 'failed', n: failed },
    { name: 'canceled', n: canceled },
    { name: 'running', n: runningFill },
  ] as const
  const valueText = `${succeeded} succeeded, ${failed} failed, ${canceled} canceled, ${counts.running} running of ${total}`
  return (
    <div className={[styles.progress, className].filter(Boolean).join(' ')}>
      <div className={styles.progressFrac}>
        {done}/{total}
      </div>
      <div
        className={styles.progressBar}
        role="progressbar"
        aria-label="Task progress"
        aria-valuenow={pct}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuetext={valueText}
      >
        {segments.map(({ name, n }) => (
          <div
            key={name}
            data-segment={name}
            className={`${styles.segment} ${styles[name]}`}
            style={{ width: widthPct(n, total) }}
          />
        ))}
      </div>
    </div>
  )
}
