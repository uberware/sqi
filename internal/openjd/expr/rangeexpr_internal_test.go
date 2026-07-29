// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"slices"
	"testing"

	"github.com/uberware/sqi/internal/openjd/intrange"
)

// TestExpandOneRange_MatchesGeneralPath is the safety net under rangeInts's
// single-sub-range fast path (expandOneRange): it skips the de-duplication map
// AND the sort that expandRanges performs, on the argument that one arithmetic
// progression is already strictly monotonic. Every case here is run through
// BOTH functions and the results compared element for element, so the fast
// path cannot quietly answer something the general path would not.
//
// The descending rows are the reason this test exists rather than a spot check:
// Iterate walks in the step's own direction, so a negative step produces the
// right values in exactly the wrong order, and only the reversal in
// expandOneRange puts them back into the increasing order section 3.4.1.1.1
// requires. A sort cannot be skipped without noticing that.
func TestExpandOneRange_MatchesGeneralPath(t *testing.T) {
	for _, text := range []string{
		"1-5",       // ascending, unit step
		"1-10000:7", // ascending, stepped, not landing on the end
		"1-5:2",     // ascending, stepped, landing on the end
		"7",         // a single bare value
		"-5--1",     // negative bounds
		"5-5",       // start equals end
		"10-1:-1",   // descending, unit step
		"10-1:-3",   // descending, stepped
		"-1--5:-2",  // descending through negative bounds
		"5-5:-1",    // descending with start equal to end
	} {
		t.Run(text, func(t *testing.T) {
			ranges, err := intrange.Parse(text)
			if err != nil {
				t.Fatalf("intrange.Parse(%q): %v", text, err)
			}
			if len(ranges) != 1 {
				t.Fatalf("intrange.Parse(%q) gave %d sub-ranges, want 1 — this test only covers the fast path", text, len(ranges))
			}
			total := ranges[0].Count()
			fast := expandOneRange(ranges[0], total)
			general := expandRanges(ranges, total)
			if !slices.Equal(fast, general) {
				t.Fatalf("expandOneRange(%q) = %v, expandRanges = %v", text, fast, general)
			}
			if !slices.IsSorted(fast) {
				t.Fatalf("expandOneRange(%q) = %v, want increasing order", text, fast)
			}
		})
	}
}

func TestRangeExpr_ConstructAndExpand(t *testing.T) {
	tests := []struct {
		text string
		want []int64
	}{
		{"1-5", []int64{1, 2, 3, 4, 5}},
		{"1-5:2", []int64{1, 3, 5}},
		{"1,5,10", []int64{1, 5, 10}},
		{"10-15:2,1-5", []int64{1, 2, 3, 4, 5, 10, 12, 14}},
		{"3,1,2", []int64{1, 2, 3}},
		{"1-5,3-7", []int64{1, 2, 3, 4, 5, 6, 7}},
	}
	for _, tc := range tests {
		t.Run(tc.text, func(t *testing.T) {
			v, err := RangeExpr(tc.text)
			if err != nil {
				t.Fatalf("RangeExpr(%q): %v", tc.text, err)
			}
			if v.Type.Code != CodeRangeExpr {
				t.Fatalf("Type = %s, want range_expr", v.Type)
			}
			if got := v.AsRangeExpr(); got != tc.text {
				t.Fatalf("AsRangeExpr() = %q, want the text verbatim %q", got, tc.text)
			}
			got, err := rangeInts(v)
			if err != nil {
				t.Fatalf("rangeInts: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("rangeInts = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("rangeInts = %v, want %v (increasing order, de-duplicated)", got, tc.want)
				}
			}
		})
	}
}

// TestRangeExpr_SpecWorkedTable checks every row of the base specification's
// own worked example table (2023-09 Template Schemas, section 3.4.1.1.1)
// directly, rather than relying only on cases this package's own author chose.
// This package has no differential-oracle coverage — nothing in the language
// constructs a range_expr until a later sub-project adds the range_expr()
// function — so this table is the only external check on rangeInts's
// ordering and de-duplication.
//
// The table's last row, "1-10:4,10-15" -> "Error: ranges overlap", is
// deliberately NOT reproduced here: that constraint belongs to
// <IntTaskParameterDefinition>'s use of the <IntRangeExpr> grammar to define a
// step's task parameter space (internal/openjd/validate.go's
// intRangeHasOverlap), not to the EXPR language's range_expr type. The EXPR
// spec (2026-02 Expression-Language.md) defines range_expr(s: string) and
// list(value: range_expr) with no overlap restriction, so overlapping
// sub-ranges are legal here and simply de-duplicate — which is exactly what
// TestRangeExpr_ConstructAndExpand's "1-5,3-7" case above already confirms.
func TestRangeExpr_SpecWorkedTable(t *testing.T) {
	tests := []struct {
		text string
		want []int64
	}{
		{"1 - 5", []int64{1, 2, 3, 4, 5}},
		{"1 - -1", []int64{1}},
		{"-1 - 1", []int64{-1, 0, 1}},
		{"1-5:2", []int64{1, 3, 5}},
		{"10-15:2,1-5", []int64{1, 2, 3, 4, 5, 10, 12, 14}},
		{"1-10:4", []int64{1, 5, 9}},
	}
	for _, tc := range tests {
		t.Run(tc.text, func(t *testing.T) {
			v, err := RangeExpr(tc.text)
			if err != nil {
				t.Fatalf("RangeExpr(%q): %v", tc.text, err)
			}
			got, err := rangeInts(v)
			if err != nil {
				t.Fatalf("rangeInts(%q): %v", tc.text, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("rangeInts(%q) = %v, want %v", tc.text, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("rangeInts(%q) = %v, want %v", tc.text, got, tc.want)
				}
			}
		})
	}
}

func TestRangeExpr_RejectsInvalidText(t *testing.T) {
	for _, text := range []string{"", "   ", "bad", "1-", "1-10:0"} {
		if _, err := RangeExpr(text); err == nil {
			t.Errorf("RangeExpr(%q) = nil error, want an error", text)
		}
	}
}

func TestRangeExpr_RejectsTooManyValues(t *testing.T) {
	v, err := RangeExpr("1-2000000000")
	if err != nil {
		t.Fatalf("RangeExpr: %v (construction must not expand)", err)
	}
	if _, err := rangeInts(v); err == nil {
		t.Fatal("rangeInts on a 2-billion-value range = nil error, want the bound to reject it")
	}
}

func TestCanonicalRange(t *testing.T) {
	tests := []struct {
		name string
		in   []int64
		want string
	}{
		{"single", []int64{7}, "7"},
		{"contiguous run", []int64{1, 2, 3, 4, 5}, "1-5"},
		{"stepped run", []int64{1, 3, 5}, "1-5:2"},
		{"pair is two singles", []int64{1, 4}, "1,4"},
		{"run then single", []int64{1, 2, 3, 9}, "1-3,9"},
		{"two runs", []int64{1, 2, 3, 10, 12, 14}, "1-3,10-14:2"},
		{"negatives", []int64{-3, -2, -1}, "-3--1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := canonicalRange(tc.in)
			if got != tc.want {
				t.Fatalf("canonicalRange(%v) = %q, want %q", tc.in, got, tc.want)
			}
			// Whatever it renders must parse back to the same integers.
			v, err := RangeExpr(got)
			if err != nil {
				t.Fatalf("RangeExpr(%q) round-trip: %v", got, err)
			}
			back, err := rangeInts(v)
			if err != nil {
				t.Fatalf("rangeInts round-trip: %v", err)
			}
			if len(back) != len(tc.in) {
				t.Fatalf("round-trip of %q = %v, want %v", got, back, tc.in)
			}
			for i := range back {
				if back[i] != tc.in[i] {
					t.Fatalf("round-trip of %q = %v, want %v", got, back, tc.in)
				}
			}
		})
	}
}
