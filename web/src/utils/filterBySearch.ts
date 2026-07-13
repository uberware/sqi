// SPDX-License-Identifier: AGPL-3.0-or-later

/**
 * Splits a free-text query into whitespace-separated, lower-cased terms.
 * An empty or whitespace-only query yields no terms.
 */
export function searchTerms(query: string): string[] {
  return query.toLowerCase().split(/\s+/).filter(Boolean)
}

/**
 * True when every whitespace-separated term in `query` appears in `haystack`
 * as a case-insensitive substring. Terms are ANDed and order-independent; an
 * empty query matches everything. e.g. "foo bar" matches "fool-grabbing-rebar"
 * and "barfoo".
 */
export function matchesSearch(haystack: string, query: string): boolean {
  const hay = haystack.toLowerCase()
  return searchTerms(query).every((term) => hay.includes(term))
}

/**
 * Multi-term, case-insensitive client-side list filter. Keeps items where
 * every whitespace-separated term in `query` matches (as a substring) somewhere
 * in the fields produced by `pick` (joined into one haystack). An empty or
 * whitespace-only query returns `items` unchanged.
 */
export function filterBySearch<T>(
  items: T[],
  query: string,
  pick: (item: T) => (string | undefined)[],
): T[] {
  const terms = searchTerms(query)
  if (terms.length === 0) return items
  return items.filter((item) => {
    const haystack = pick(item)
      .filter((field): field is string => field !== undefined)
      .join(' ')
      .toLowerCase()
    return terms.every((term) => haystack.includes(term))
  })
}
