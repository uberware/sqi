// SPDX-License-Identifier: AGPL-3.0-or-later

import { countDownCeil, formatDuration } from '@/lib/time'

interface RemainingTimeProps {
  /** Server-reported seconds of work left; null/undefined when unknown. */
  seconds: number | null | undefined
  /** Tasks running now; each retires a second of work per second. */
  running: number
  /** Epoch ms the server figure was current (the query's last update). */
  anchorMs: number
  /** Current epoch ms. */
  now: number
}

/** A job's remaining time, counted down client-side from the last server figure. */
export default function RemainingTime({ seconds, running, anchorMs, now }: RemainingTimeProps) {
  if (seconds === null || seconds === undefined) return <span>—</span>
  const left = countDownCeil(seconds, running, anchorMs, now)
  return <span>{formatDuration(left * 1000)}</span>
}
