// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"strings"

	"github.com/uberware/sqi/internal/store"
)

// searchClause builds a WHERE fragment that matches every whitespace-separated
// term in search (per [store.SearchTerms]) against any of cols, using
// case-insensitive LIKE. It returns "" and no args when search has no terms.
//
// Each term becomes its own ANDed "(col1 LIKE ? OR col2 LIKE ? ...)" group, so
// a multi-word query narrows results and is order-independent (e.g. "foo bar"
// requires both "foo" and "bar" to appear somewhere in cols). This matches the
// fake store's [store.MatchesSearch] exactly, keeping the two backends in
// lockstep. The fragment begins with " AND " so it appends to an existing WHERE.
func searchClause(cols []string, search string) (clause string, args []any) {
	terms := store.SearchTerms(search)
	if len(terms) == 0 {
		return "", nil
	}
	var b strings.Builder
	args = make([]any, 0, len(terms)*len(cols))
	for _, term := range terms {
		b.WriteString(" AND (")
		for i, col := range cols {
			if i > 0 {
				b.WriteString(" OR ")
			}
			b.WriteString(col)
			b.WriteString(" LIKE ?")
		}
		b.WriteString(")")
		like := "%" + term + "%"
		for range cols {
			args = append(args, like)
		}
	}
	return b.String(), args
}
