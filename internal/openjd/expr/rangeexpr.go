// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/uberware/sqi/internal/openjd/intrange"
)

// RangeExpr returns a range_expr value from its text, which must satisfy the
// base specification's <IntRangeExpr> grammar (2023-09 Template Schemas,
// section 3.4.1.1.1).
//
// The TEXT is what the value carries; the integers are expanded only when an
// operation needs them. That is the point of section 1.2.2 giving CHUNK[INT] the
// type range_expr rather than list[int] — a frame range need not be expanded to
// be passed around — and it is what lets range_expr -> string (section 1.2.3)
// answer without materializing anything.
//
// Nothing in the language produces a range_expr yet: the constructor function
// range_expr() is sub-project C's and the CHUNK[INT] symbol is sub-project E's.
// This constructor is how a caller's symbol table supplies one in the meantime.
func RangeExpr(text string) (Value, error) {
	if _, err := intrange.Parse(text); err != nil {
		return Value{}, fmt.Errorf("invalid range expression %q: %w", text, err)
	}
	return Value{Type: TRangeExpr, s: text}, nil
}

// AsRangeExpr returns the range expression text. It panics if v is not a
// range_expr.
func (v Value) AsRangeExpr() string { v.mustBe(CodeRangeExpr); return v.s }

// rangeInts expands a range_expr to its integers, in INCREASING order and
// de-duplicated, which is what section 3.4.1.1.1 specifies ("combined to form a
// list of values in increasing order").
//
// Note that internal/openjd expands the same grammar in first-seen order
// instead. That divergence is its own, is pre-existing, and is deliberately not
// changed here — see the intrange package doc.
//
// The base spec's own worked table (section 3.4.1.1.1) also lists
// "1-10:4,10-15" as an error because its sub-ranges overlap. That constraint
// belongs to <IntTaskParameterDefinition>'s use of the grammar to define a
// step's task parameter space (enforced separately by
// internal/openjd/validate.go's intRangeHasOverlap) — it is not part of the
// EXPR language's range_expr type. The EXPR spec (2026-02
// Expression-Language.md) defines range_expr(s: string) -> range_expr and
// list(value: range_expr) -> list[int] with no overlap restriction at all, so
// overlapping sub-ranges here are legal and simply de-duplicate, matching
// "1-5,3-7" -> [1,2,3,4,5,6,7] below.
func rangeInts(v Value) ([]int64, error) {
	ranges, err := intrange.Parse(v.AsRangeExpr())
	if err != nil {
		// Unreachable: RangeExpr validated the text on construction.
		return nil, fmt.Errorf("invalid range expression %q: %w", v.AsRangeExpr(), err)
	}
	total := 0
	for _, r := range ranges {
		total += r.Count()
		if err := checkElementCount(total); err != nil {
			return nil, err
		}
	}
	if len(ranges) == 1 {
		return expandOneRange(ranges[0], total), nil
	}
	return expandRanges(ranges, total), nil
}

// expandOneRange expands a range expression with exactly ONE sub-range, which
// needs neither of the two things the general path below does.
//
// A single sub-range is an arithmetic progression with a non-zero step, so its
// values are strictly monotonic: no two are equal, which retires the
// de-duplication map, and they are already ordered, which retires the sort —
// they are merely in the WRONG order for a negative step, and reversing a
// slice in place is linear where sorting it is not. Both were paid for on
// every call, and every operation re-expands from scratch, so the cost
// compounds: measured on a 10,000,000-value range, one expansion went from
// 861 ms to 64 ms, and "Param.Big[:][:][:][:][:]" — which expands six times —
// from 4.06 s to 0.32 s.
//
// Correctness rests entirely on the "exactly one" test, so the general path is
// still what runs for anything else — two sub-ranges may overlap and may be
// written in any order, and only the map and the sort make section 3.4.1.1.1's
// "increasing order", de-duplicated, come out right.
func expandOneRange(r intrange.Range, total int) []int64 {
	out := make([]int64, 0, total)
	for _, n := range r.Iterate() {
		out = append(out, int64(n))
	}
	// Iterate walks in the step's own direction, and rangeInts owes its caller
	// increasing order. A single reversal is the whole difference.
	if r.Step < 0 {
		slices.Reverse(out)
	}
	return out
}

// expandRanges is the general path: several sub-ranges, which may overlap and
// may be written in any order, so the values must be de-duplicated and sorted.
func expandRanges(ranges []intrange.Range, total int) []int64 {
	seen := make(map[int]struct{}, total)
	out := make([]int64, 0, total)
	for _, r := range ranges {
		for _, n := range r.Iterate() {
			if _, dup := seen[n]; dup {
				continue
			}
			seen[n] = struct{}{}
			out = append(out, int64(n))
		}
	}
	slices.Sort(out)
	return out
}

// canonicalRange renders integers as range expression text, collapsing runs
// that share a constant delta.
//
// Section 2.1.8 makes a positive-step slice of a range_expr return a range_expr,
// and the sliced integers have no text of their own, so B2 cannot avoid this
// derivation. Sub-project C's range_expr(l: list[int]) reuses it.
//
// A run must be at least three values long to be worth collapsing: "1,4" and
// "1-4:3" describe the same pair, and the comma form is what a reader expects.
func canonicalRange(ints []int64) string {
	if len(ints) == 0 {
		return ""
	}
	var parts []string
	for i := 0; i < len(ints); {
		if i+2 < len(ints) {
			delta := ints[i+1] - ints[i]
			if delta != 0 && ints[i+2]-ints[i+1] == delta {
				j := i + 2
				for j+1 < len(ints) && ints[j+1]-ints[j] == delta {
					j++
				}
				parts = append(parts, runText(ints[i], ints[j], delta))
				i = j + 1
				continue
			}
		}
		parts = append(parts, strconv.FormatInt(ints[i], 10))
		i++
	}
	return strings.Join(parts, ",")
}

// runText renders one collapsed run, omitting a step of 1.
func runText(start, end, delta int64) string {
	if delta == 1 {
		return fmt.Sprintf("%d-%d", start, end)
	}
	return fmt.Sprintf("%d-%d:%d", start, end, delta)
}
