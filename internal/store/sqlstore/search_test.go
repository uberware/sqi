// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore_test

import (
	"slices"
	"testing"

	"github.com/uberware/sqi/internal/store/sqlstore"
)

// TestSearchClause_SQLiteOutput pins the fragment and arguments searchClause
// builds with the SQLite dialect: one ANDed group per term, each term LIKE-ed
// against every column.
func TestSearchClause_SQLiteOutput(t *testing.T) {
	d := openTestStoreWB(t).Dialect()
	for _, c := range []struct {
		name   string
		cols   []string
		search string
		clause string
		args   []any
	}{
		{"empty search", []string{"name"}, "  ", "", nil},
		{"one column one term", []string{"name"}, "Foo", " AND (name LIKE ?)", []any{"%foo%"}},
		{
			"three columns one term",
			[]string{"name", "id", "owner"},
			"foo",
			" AND (name LIKE ? OR id LIKE ? OR owner LIKE ?)",
			[]any{"%foo%", "%foo%", "%foo%"},
		},
		{
			"three columns two terms",
			[]string{"name", "id", "owner"},
			" foo  BAR ",
			" AND (name LIKE ? OR id LIKE ? OR owner LIKE ?) AND (name LIKE ? OR id LIKE ? OR owner LIKE ?)",
			[]any{"%foo%", "%foo%", "%foo%", "%bar%", "%bar%", "%bar%"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			clause, args := sqlstore.SearchClause(d, c.cols, c.search)
			if clause != c.clause || !slices.Equal(args, c.args) {
				t.Fatalf("searchClause = (%q, %q), want (%q, %q)", clause, args, c.clause, c.args)
			}
		})
	}
}
