// SPDX-License-Identifier: AGPL-3.0-or-later

/** Product metadata length caps, mirroring internal/product/limits.go.
 *
 * These are client-side hints only; the server is authoritative. The server
 * counts runes and the `maxLength` attribute counts UTF-16 code units, so the
 * two disagree for astral-plane characters. That is acceptable for a hint that
 * stops typing early; do not rely on it as validation. */
export const PRODUCT_LIMITS = {
  name: 128,
  title: 200,
  description: 500,
  readme: 8000,
  category: 64,
  version: 32,
} as const
