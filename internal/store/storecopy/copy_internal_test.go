// SPDX-License-Identifier: AGPL-3.0-or-later

package storecopy

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/store/schemainfo"
)

func TestRowsPerInsert_StaysUnderTheBindParameterLimit(t *testing.T) {
	for _, tc := range []struct{ cols, want int }{
		{1, 500}, {23, 500}, {131, 500}, {132, 496}, {1000, 65}, {70000, 1},
	} {
		got := rowsPerInsert(tc.cols)
		if got != tc.want {
			t.Errorf("rowsPerInsert(%d) = %d, want %d", tc.cols, got, tc.want)
		}
		if tc.cols <= maxBindParams && got*tc.cols > maxBindParams {
			t.Errorf("rowsPerInsert(%d) = %d: %d parameters, over PostgreSQL's %d", tc.cols, got, got*tc.cols, maxBindParams)
		}
	}
}

func TestSameShape_NamesEveryDifference(t *testing.T) {
	cols := func(names ...string) []schemainfo.Column {
		var out []schemainfo.Column
		for _, n := range names {
			out = append(out, schemainfo.Column{Name: n, Type: "text"})
		}
		return out
	}
	src := map[string]schemainfo.Table{
		"farms": {Name: "farms", Columns: cols("id", "name", "old")},
		"jobs":  {Name: "jobs", Columns: cols("id")},
	}
	dst := map[string]schemainfo.Table{
		"farms": {Name: "farms", Columns: cols("id", "name", "extra")},
		"notes": {Name: "notes", Columns: cols("id")},
	}
	err := sameShape(src, dst)
	if err == nil {
		t.Fatal("sameShape accepted different schemas")
	}
	for _, want := range []string{
		"table notes exists only in the PostgreSQL database",
		"table jobs exists only in the SQLite database",
		"column farms.extra exists only in the PostgreSQL database",
		"column farms.old exists only in the SQLite database",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
	if err := sameShape(src, src); err != nil {
		t.Errorf("sameShape of a schema with itself = %v", err)
	}
}

func TestConvertValue(t *testing.T) {
	for _, tc := range []struct {
		name    string
		k       kind
		in      any
		want    any
		wantErr string
	}{
		{"null boolean", kindBool, nil, nil, ""},
		{"zero is false", kindBool, int64(0), false, ""},
		{"one is true", kindBool, int64(1), true, ""},
		{"two is no boolean", kindBool, int64(2), nil, "not 0 or 1"},
		{"text is no boolean", kindBool, "1", nil, "not 0 or 1"},
		{"integer", kindInt, int64(-7), int64(-7), ""},
		{"real is no integer", kindInt, 1.5, nil, "not an integer"},
		{"text", kindText, "héllo\n", "héllo\n", ""},
		{"bytes are text", kindText, []byte("x"), "x", ""},
		{"NUL", kindText, "a\x00b", nil, "NUL"},
		{"invalid UTF-8", kindText, "\xff", nil, "UTF-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := convertValue(tc.k, tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("convertValue = %v, %v; want an error with %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("convertValue = %#v, %v; want %#v", got, err, tc.want)
			}
		})
	}
}

func TestKindOf_RefusesAnUntaughtType(t *testing.T) {
	for _, typ := range []string{"bytea", "double precision", "numeric", "jsonb", "timestamp with time zone"} {
		if _, err := kindOf(typ); err == nil || !strings.Contains(err.Error(), typ) {
			t.Errorf("kindOf(%q) = %v, want a refusal naming it", typ, err)
		}
	}
}

func TestQuoteIdent(t *testing.T) {
	if got := quoteIdent(`a"b`); got != `"a""b"` {
		t.Fatalf("quoteIdent = %s", got)
	}
}
