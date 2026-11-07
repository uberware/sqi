// SPDX-License-Identifier: AGPL-3.0-or-later

/**
 * Formats a millisecond duration as a compact "Ns" / "Nm Ns" / "Nh Nm"
 * string. Returns "—" for negative durations.
 */
export function formatDuration(ms: number): string {
  if (ms < 0) return '—'
  const s = Math.floor(ms / 1000)
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ${s % 60}s`
  return `${Math.floor(m / 60)}h ${m % 60}m`
}

/**
 * Duration between two ISO timestamps. When `endIso` is undefined the span runs
 * from `startIso` to `now` (epoch ms). Returns "—" when there is no start
 * timestamp or the computed duration is negative.
 */
export function formatTimespan(
  startIso: string | undefined,
  endIso: string | undefined,
  now: number,
): string {
  if (!startIso) return '—'
  const start = new Date(startIso).getTime()
  const end = endIso ? new Date(endIso).getTime() : now
  return formatDuration(end - start)
}

/**
 * Compact uptime string: "Ns" / "Nm" / "Nh Nm" / "Nd Nh". Returns "—" for
 * negative durations.
 */
export function formatUptime(registeredAt: string, now: number): string {
  const ms = now - new Date(registeredAt).getTime()
  if (ms < 0) return '—'
  const s = Math.floor(ms / 1000)
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h ${m % 60}m`
  const d = Math.floor(h / 24)
  return `${d}d ${h % 24}h`
}

/**
 * Short "updated N ago" age for a millisecond timestamp: "just now" under
 * five seconds, then seconds/minutes, falling back to a clock time after an
 * hour. Returns "—" for a zero timestamp (no data yet).
 */
export function formatAge(ts: number, now: number): string {
  if (ts === 0) return '—'
  const diff = now - ts
  if (diff < 5000) return 'just now'
  if (diff < 60_000) return `${Math.floor(diff / 1000)}s ago`
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)}m ago`
  return new Date(ts).toLocaleTimeString()
}

const MIN_ELAPSED_FOR_ESTIMATE_MS = 30_000

/**
 * Estimated time remaining for an attempt, extrapolated linearly from the
 * elapsed time and percent complete. Undefined until there is enough signal:
 * no start or progress, progress below 1%, or under 30s elapsed.
 */
export function timeLeftMs(
  startIso: string | undefined,
  now: number,
  progress: number | undefined,
): number | undefined {
  if (startIso === undefined || progress === undefined || progress < 1) return undefined
  const elapsed = now - new Date(startIso).getTime()
  if (Number.isNaN(elapsed) || elapsed < MIN_ELAPSED_FOR_ESTIMATE_MS) return undefined
  if (progress >= 100) return 0
  return (elapsed * (100 - progress)) / progress
}

/**
 * Seconds of work still left, counted down from the server's figure: each
 * running task retires one second of work per second since `anchorMs`.
 * Floors at zero; a `now` before the anchor counts nothing.
 */
export function countDownSeconds(
  remaining: number,
  running: number,
  anchorMs: number,
  now: number,
): number {
  return Math.max(0, remaining - (running * Math.max(0, now - anchorMs)) / 1000)
}

/** {@link countDownSeconds} rounded up, so a countdown never reads below the true figure. */
export function countDownCeil(
  remaining: number,
  running: number,
  anchorMs: number,
  now: number,
): number {
  return Math.ceil(countDownSeconds(remaining, running, anchorMs, now))
}

/** Wall-clock finish time (epoch ms) of `remainingSeconds` of work spread over `running` tasks. */
export function etaMs(remainingSeconds: number, running: number, now: number): number {
  return now + (remainingSeconds * 1000) / Math.max(1, running)
}

const pad2 = (n: number): string => String(n).padStart(2, '0')

/**
 * Local clock time: "HH:MM" on the same local day as `now`, otherwise
 * "Ddd HH:MM" (e.g. "Tue 02:40").
 */
export function formatClock(ms: number, now: number): string {
  const d = new Date(ms)
  const n = new Date(now)
  const hhmm = `${pad2(d.getHours())}:${pad2(d.getMinutes())}`
  if (
    d.getFullYear() === n.getFullYear() &&
    d.getMonth() === n.getMonth() &&
    d.getDate() === n.getDate()
  ) {
    return hhmm
  }
  const day = d.toLocaleDateString('en-US', { weekday: 'short' })
  return `${day} ${hhmm}`
}

/** Largest estimate the API accepts (int32 seconds). */
const MAX_DURATION_INPUT_SECONDS = 2147483647

const DURATION_INPUT = /^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$/

/**
 * Seconds from a typed duration such as "2h", "1h 30m", "90m" or "45s"
 * (case-insensitive, spaces optional). Undefined for a bare number, a zero
 * total, or anything else.
 */
export function parseDurationInput(text: string): number | undefined {
  const m = DURATION_INPUT.exec(text.toLowerCase().replace(/\s+/g, ''))
  if (!m || (m[1] === undefined && m[2] === undefined && m[3] === undefined)) return undefined
  const total = Number(m[1] ?? 0) * 3600 + Number(m[2] ?? 0) * 60 + Number(m[3] ?? 0)
  return total > 0 && total <= MAX_DURATION_INPUT_SECONDS ? total : undefined
}

/** Exact inverse of {@link parseDurationInput}: every non-zero h/m/s component, e.g. "1h 1m 1s". */
export function formatDurationInput(seconds: number): string {
  const h = Math.floor(seconds / 3600)
  const m = Math.floor((seconds % 3600) / 60)
  const s = seconds % 60
  const parts: string[] = []
  if (h > 0) parts.push(`${h}h`)
  if (m > 0) parts.push(`${m}m`)
  if (s > 0) parts.push(`${s}s`)
  return parts.join(' ')
}
