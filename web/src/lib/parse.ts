// SPDX-License-Identifier: AGPL-3.0-or-later

/**
 * Parses an optional integer form field. Blank or non-numeric input yields
 * undefined (the field is omitted so the server default is inherited);
 * anything else is truncated toward zero.
 */
export function parseOptionalInt(s: string): number | undefined {
  const trimmed = s.trim()
  if (trimmed === '') return undefined
  const n = Number(trimmed)
  return Number.isFinite(n) ? Math.trunc(n) : undefined
}
