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
