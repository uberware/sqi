// SPDX-License-Identifier: AGPL-3.0-or-later

import { useState, useEffect } from 'react'

const FAST_MS = 1_000
const SLOW_MS = 30_000

/**
 * Returns a `Date.now()` value that advances on an interval so relative and
 * elapsed time displays stay current without any server traffic.
 *
 * Ticks every second while `active` is true (e.g. a job or task is running)
 * and the tab is visible; otherwise every 30 seconds. While the tab is hidden
 * it stops ticking entirely, snapping to the current time when the tab becomes
 * visible again.
 */
export function useLiveNow(active: boolean): number {
  const [now, setNow] = useState(() => Date.now())

  useEffect(() => {
    let interval: ReturnType<typeof setInterval> | null = null

    const stop = () => {
      if (interval !== null) {
        clearInterval(interval)
        interval = null
      }
    }

    const start = () => {
      stop()
      setNow(Date.now())
      interval = setInterval(() => setNow(Date.now()), active ? FAST_MS : SLOW_MS)
    }

    const handleVisibility = () => {
      if (document.visibilityState === 'visible') start()
      else stop()
    }

    if (document.visibilityState === 'visible') {
      start()
    }
    document.addEventListener('visibilitychange', handleVisibility)

    return () => {
      stop()
      document.removeEventListener('visibilitychange', handleVisibility)
    }
  }, [active])

  return now
}
