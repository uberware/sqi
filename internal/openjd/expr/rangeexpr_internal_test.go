// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"slices"
	"testing"
	"time"

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

// TestRangeExprCount_AgreesWithExpansion pins rangeExprCount against the
// actual expansion (len(rangeInts(v))) for every case, not against
// hand-written numbers: a table of expected COUNTS could accidentally repeat
// the same overcounting mistake rangeExprCount itself once made (summing
// Count() across every sub-range unconditionally, which double-counts
// overlap). Comparing against rangeInts's own de-duplicated, sorted
// expansion — already pinned elsewhere in this file against the base
// specification's worked table — is the stronger check, because the two
// helpers would have to be wrong in exactly the same way to agree by
// accident.
//
// "1-5,3-7" and "10-15:2,1-5" are the two cases from the review finding:
// TestRangeExpr_ConstructAndExpand and TestRangeExpr_SpecWorkedTable already
// pin their expansions at 7 and 8 values respectively, which is what makes
// the naive sum (10 and 13) visibly wrong.
func TestRangeExprCount_AgreesWithExpansion(t *testing.T) {
	for _, text := range []string{
		"1-5",           // single sub-range, ascending, unit step
		"1-5:2",         // single sub-range, stepped
		"7",             // single bare value
		"10-1:-1",       // single sub-range, descending
		"1-5,3-7",       // two OVERLAPPING sub-ranges — the review finding's case
		"10-15:2,1-5",   // two sub-ranges, out of order and non-overlapping
		"1,5,10",        // three bare values, no overlap
		"1-5,1-5",       // two IDENTICAL sub-ranges — total overlap
		"1-10,5-15,3-7", // three sub-ranges with staggered overlap
		"1-10:2,2-10:2", // two sub-ranges that interleave without ever coinciding
	} {
		t.Run(text, func(t *testing.T) {
			v, err := RangeExpr(text)
			if err != nil {
				t.Fatalf("RangeExpr(%q): %v", text, err)
			}
			want, err := rangeInts(v)
			if err != nil {
				t.Fatalf("rangeInts(%q): %v", text, err)
			}
			got, err := rangeExprCount(v)
			if err != nil {
				t.Fatalf("rangeExprCount(%q): %v", text, err)
			}
			if got != len(want) {
				t.Errorf("rangeExprCount(%q) = %d, want %d (len of the actual expansion %v)", text, got, len(want), want)
			}
		})
	}
}

// TestRangeExprCount_LargeSingleRangeStaysArithmetic proves the single-
// sub-range path never materializes anything, rather than merely asserting
// it returns the right answer. "1-2000000000" is 2 BILLION values, 200 times
// this package's own maxElements bound: actually expanding it would allocate
// gigabytes and take long enough that this test's own time budget below would
// catch it, and TestRangeExpr_RejectsTooManyValues shows rangeInts still
// rejects the identical text via the bound check rather than by finishing an
// expansion — that is unchanged, because rangeInts DOES materialize.
//
// rangeExprCount must NOT reject it, and that is Task 12's correction to this
// test, not a new behavior invented here: this same function's own doc
// comment already named "len(range_expr('1-20000000'))" as the motivating
// case for answering arithmetically, but a prior revision ran EVERY
// sub-range's Count() through checkElementCount before ever branching on
// len(ranges) == 1, so the single-range path was bound-checked exactly like
// the materializing multi-range path it was meant to be exempt from. That
// bound guards MATERIALIZATION (maxElements' own doc comment: "how many
// elements one operation may materialize") — irrelevant here, since
// intrange.Range.Count() answers arithmetically and allocates nothing however
// large the range is, and Count() itself already saturates to math.MaxInt
// rather than overflow if the arithmetic would. The point of THIS test is
// that the right, unbounded answer comes back in microseconds.
func TestRangeExprCount_LargeSingleRangeStaysArithmetic(t *testing.T) {
	v, err := RangeExpr("1-2000000000")
	if err != nil {
		t.Fatalf("RangeExpr: %v (construction must not expand)", err)
	}
	start := time.Now()
	got, err := rangeExprCount(v)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("rangeExprCount on a 2-billion-value range: %v, want the arithmetic answer, not a rejection", err)
	}
	if got != 2_000_000_000 {
		t.Fatalf("rangeExprCount(%q) = %d, want 2000000000", "1-2000000000", got)
	}
	// A generous ceiling: real arithmetic finishes in microseconds, while
	// appending 2 billion int64s (rangeInts's own path, deliberately NOT
	// taken here) would not complete in under a second on any machine this
	// suite runs on, and would more likely exhaust memory first.
	if elapsed > time.Second {
		t.Fatalf("rangeExprCount on a 2-billion-value range took %s, want it to answer arithmetically without materializing", elapsed)
	}
}
