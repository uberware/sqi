// SPDX-License-Identifier: AGPL-3.0-or-later

/**
 * Case-insensitive substring filter for client-side list search. Keeps items
 * where any field produced by `pick` contains the trimmed query. An empty or
 * whitespace-only query returns `items` unchanged.
 */
export function filterBySearch<T>(
  items: T[],
  query: string,
  pick: (item: T) => (string | undefined)[],
): T[] {
  const q = query.trim().toLowerCase()
  if (q === '') return items
  return items.filter((item) =>
    pick(item).some((field) => field !== undefined && field.toLowerCase().includes(q)),
  )
}
