// SPDX-License-Identifier: AGPL-3.0-or-later

/** Product metadata length caps, mirroring internal/product/limits.go.
 *
 * These are client-side HINTS only — the server is authoritative. The server
 * counts RUNES; the `maxLength` attribute counts UTF-16 code units, so the two
 * disagree for astral-plane characters. Acceptable for a hint that stops typing
 * early; never rely on it as validation. */
export const PRODUCT_LIMITS = {
  name: 128,
  title: 200,
  description: 500,
  readme: 8000,
  category: 64,
  version: 32,
} as const
