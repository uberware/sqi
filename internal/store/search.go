// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import "strings"

// SearchTerms splits a free-text search query into whitespace-separated,
// lower-cased terms. An empty or whitespace-only query yields no terms.
//
// Terms are ANDed by callers and matched as case-insensitive substrings, so a
// multi-word query narrows results and is order-independent: "foo bar" keeps
// "fool-grabbing-rebar" and "barfoo". This mirrors the web UI's client-side
// filter (utils/filterBySearch.ts) so search behaves identically everywhere.
func SearchTerms(query string) []string {
	return strings.Fields(strings.ToLower(query))
}

// MatchesSearch reports whether every term in query (per [SearchTerms]) appears
// as a case-insensitive substring in at least one of fields. An empty query
// matches. It is the in-memory fake store's equivalent of the SQLite LIKE
// search, kept in the shared package so both stay in lockstep.
func MatchesSearch(query string, fields ...string) bool {
	terms := SearchTerms(query)
	if len(terms) == 0 {
		return true
	}
	lowered := make([]string, len(fields))
	for i, f := range fields {
		lowered[i] = strings.ToLower(f)
	}
	for _, term := range terms {
		matched := false
		for _, f := range lowered {
			if strings.Contains(f, term) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}
