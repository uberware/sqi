// SPDX-License-Identifier: AGPL-3.0-or-later

/** Constrain value to the inclusive range [lo, hi]. */
export function clamp(value: number, lo: number, hi: number): number {
  return Math.min(Math.max(value, lo), hi)
}
