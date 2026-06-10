// SPDX-License-Identifier: AGPL-3.0-only

import { useReducer, useEffect } from 'react'

// ── Shared tick subscription ──────────────────────────────────────────────────
// A single 30-second interval is shared across all mounted RelativeTime
// instances so the page does not accumulate one timer per component.

type TickListener = () => void

const tickListeners = new Set<TickListener>()
let sharedInterval: ReturnType<typeof setInterval> | null = null

function subscribeToTick(fn: TickListener): () => void {
  tickListeners.add(fn)
  if (sharedInterval === null) {
    sharedInterval = setInterval(() => {
      tickListeners.forEach((listener) => listener())
    }, 30_000)
  }
  return () => {
    tickListeners.delete(fn)
    if (tickListeners.size === 0 && sharedInterval !== null) {
      clearInterval(sharedInterval)
      sharedInterval = null
    }
  }
}

// ── Relative time formatting ──────────────────────────────────────────────────

const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' })

function formatRelative(timestamp: string | number | Date): string {
  const date = timestamp instanceof Date ? timestamp : new Date(timestamp)
  const now = Date.now()
  const diffMs = date.getTime() - now
  const diffSeconds = Math.round(diffMs / 1000)
  const absSeconds = Math.abs(diffSeconds)

  if (absSeconds < 60) {
    return rtf.format(diffSeconds, 'second')
  }
  if (absSeconds < 3_600) {
    return rtf.format(Math.round(diffSeconds / 60), 'minute')
  }
  if (absSeconds < 86_400) {
    return rtf.format(Math.round(diffSeconds / 3_600), 'hour')
  }
  return rtf.format(Math.round(diffSeconds / 86_400), 'day')
}

// ── Component ─────────────────────────────────────────────────────────────────

interface RelativeTimeProps {
  timestamp: string | number | Date
  className?: string
}

export default function RelativeTime({ timestamp, className }: RelativeTimeProps) {
  const [, forceUpdate] = useReducer((n: number) => n + 1, 0)

  useEffect(() => subscribeToTick(forceUpdate), [])

  const iso =
    timestamp instanceof Date
      ? timestamp.toISOString()
      : typeof timestamp === 'string'
        ? timestamp
        : new Date(timestamp).toISOString()

  return (
    <time className={className} dateTime={iso} title={new Date(iso).toLocaleString()}>
      {formatRelative(timestamp)}
    </time>
  )
}
