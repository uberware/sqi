// SPDX-License-Identifier: AGPL-3.0-or-later

/** First eight characters of an id — enough to distinguish rows in a table. */
export function truncateId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id
}
