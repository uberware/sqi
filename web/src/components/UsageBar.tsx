// SPDX-License-Identifier: AGPL-3.0-or-later

import styles from './UsageBar.module.css'

interface UsageBarProps {
  /** Current usage. */
  inUse: number
  /** Capacity; values <= 0 render an empty bar. */
  max: number
  /** Extra class for the bar track (e.g. to make it flex within a row). */
  className?: string | undefined
}

/** Fraction (0–1) of capacity in use, clamped. */
function usageFraction(inUse: number, max: number): number {
  if (max <= 0) return 0
  return Math.min(inUse / max, 1)
}

/** A horizontal capacity meter: a track with an accent fill sized to in-use/max. */
export default function UsageBar({ inUse, max, className }: UsageBarProps) {
  return (
    <span className={[styles.bar, className].filter(Boolean).join(' ')} aria-hidden="true">
      <span className={styles.fill} style={{ width: `${usageFraction(inUse, max) * 100}%` }} />
    </span>
  )
}
