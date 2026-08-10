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
// This constructor is also what the language's own range_expr(string) function
// calls (sub-project C1, funcsconv.go) — the first way an expression can
// produce a range_expr with no symbol table involved. The CHUNK[INT] symbol
// remains sub-project E's, still-unimplemented, second way to get one. A
// caller's symbol table binding a name to a value built by calling this
// function directly is the third.
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

// rangeExprCount returns how many integers a range_expr expands to — the same
// count len(rangeInts(v)) would give — computed without materializing the
// expansion wherever that is possible.
//
// This is section 1.3.10 rule 2's element count for a range_expr: charging the
// cost of counting must not itself cost an expansion. Sub-project task 12
// makes len(range_expr) reuse this same helper; rangeInts stays the right
// function for every caller that genuinely needs the values, such as
// list(range_expr).
//
// A SINGLE sub-range is the case that matters for avoiding materialization,
// and the only case handled purely arithmetically: its values are strictly
// monotonic (expandOneRange's own comment — "no two are equal" — is the same
// fact stated for the expansion path), so intrange.Range.Count() needs no
// deduplication to already be exact. It is also the only shape in which a
// caller can ask for an astronomically large count — len(range_expr(
// '1-20000000')) is the motivating case — without this function allocating
// anything to answer. That is also why it is returned BEFORE checkElementCount
// runs, unlike the multi-sub-range path below: checkElementCount guards
// materialization, which this branch never does, so 20,000,000 (twice
// maxElements) is a legitimate arithmetic answer here, not a value to refuse.
// A prior revision of this function ran every sub-range's Count() through
// checkElementCount unconditionally, before branching on len(ranges) — TDD's
// own RED run for TestLenRangeExpr_DoesNotMaterialize (Task 12) caught that
// directly, refusing the motivating case this comment already named.
//
// TWO OR MORE sub-ranges may overlap — expandRanges's own comment says so
// ("may overlap ... must be de-duplicated"), and rangeInts's doc comment
// pins "1-5,3-7" at 7 distinct values, not 5+5 — and there is no general
// arithmetic shortcut to size the union of several arithmetic progressions
// with different steps short of finding the overlap itself. An earlier
// version of this function summed Count() across every sub-range
// unconditionally, which overcounts exactly this case; that was a real
// defect caught in review, not a hypothetical one. The multi-sub-range case
// IS bounded, unlike the single-range one above, because it falls back to a
// real expansion that allocates: the running total below is the SAME
// conservative pre-dedup bound rangeInts itself checks before expanding — so
// falling back to the real expansion here, reusing the ranges already
// parsed, costs no more than list(range_expr(...)) already pays for the
// identical value.
func rangeExprCount(v Value) (int, error) {
	ranges, err := intrange.Parse(v.AsRangeExpr())
	if err != nil {
		// Unreachable: RangeExpr validated the text on construction.
		return 0, fmt.Errorf("invalid range expression %q: %w", v.AsRangeExpr(), err)
	}
	if len(ranges) == 1 {
		return ranges[0].Count(), nil
	}
	total := 0
	for _, r := range ranges {
		total += r.Count()
		if err := checkElementCount(total); err != nil {
			return 0, err
		}
	}
	return len(expandRanges(ranges, total)), nil
}

// rangeExprCountBounds returns a LOWER and an UPPER bound on how many integers
// v expands to, computed purely arithmetically -- nothing is materialized, for
// any shape of input, which is the one property [rangeExprCount] cannot offer.
//
// It exists because rangeExprCount is arithmetic ONLY for a single sub-range.
// With two or more it calls expandRanges to produce the count, so every caller
// that wanted a count in order to decide whether the expansion FITS was doing
// the expansion to find out. Measured at the commit that introduced
// meter.reserve: len(range_expr("1-5000000,6000000-9000000")) answered 8000001
// after 645 ms and 687 MB while charging TWO operations, and the same
// expression reached through list(), a subscript or a comprehension paid the
// identical 687 MB before the reservation refused it. See
// reserveRangeExprExpansion, which is what this pair is for.
//
// THE UPPER BOUND is the pre-deduplication sum of every sub-range's own count,
// guarded by checkElementCount exactly as rangeInts guards the same running
// total before expanding. It is >= the true count always, and EQUAL to it
// unless sub-ranges overlap.
//
// THE LOWER BOUND is the LARGEST single sub-range's count. A single sub-range
// is an arithmetic progression with a non-zero step, so its own values are
// strictly monotonic and contain no duplicates (expandOneRange's comment
// states the same fact) -- every one of them survives deduplication, so the
// union cannot be smaller than the largest of them. It is <= the true count
// always, and again EQUAL to it when one sub-range subsumes all the others.
//
// A single sub-range collapses both bounds onto the exact count, which is why
// the common case needs no expansion and no fallback at all.
func rangeExprCountBounds(v Value) (low, high int, err error) {
	ranges, perr := intrange.Parse(v.AsRangeExpr())
	if perr != nil {
		// Unreachable: RangeExpr validated the text on construction.
		return 0, 0, fmt.Errorf("invalid range expression %q: %w", v.AsRangeExpr(), perr)
	}
	if len(ranges) == 1 {
		n := ranges[0].Count()
		return n, n, nil
	}
	for _, r := range ranges {
		n := r.Count()
		if n > low {
			low = n
		}
		high += n
		if err := checkElementCount(high); err != nil {
			return 0, 0, err
		}
	}
	return low, high, nil
}

// reserveRangeExprCount is [reserveRangeExprExpansion] for a caller that needs
// only the COUNT and never the values -- len(range_expr) is the only one.
//
// The difference is the single-sub-range case, and it is not a nicety: there,
// rangeExprCount is pure arithmetic on the parsed bounds and allocates
// nothing, so there is no work to refuse. len(range_expr('1-20000000'))
// answering 20,000,000 in constant time is a DOCUMENTED, tested property
// (funcsconv.go's len row, TestLenRangeExpr_DoesNotMaterialize) -- reserving
// 20,000,000 operations for it would reject the exact query the arithmetic
// path exists to serve, under any budget tighter than the count itself. That
// regression was caught by re-running the measurement rather than by any
// test, which is why the two helpers are now separate functions with this
// comment between them.
//
// With TWO OR MORE sub-ranges counting DOES expand, so from there on this is
// identical to reserving the expansion.
func reserveRangeExprCount(ec evalCtx, v Value) error {
	ranges, err := intrange.Parse(v.AsRangeExpr())
	if err != nil {
		// Unreachable: RangeExpr validated the text on construction.
		return fmt.Errorf("invalid range expression %q: %w", v.AsRangeExpr(), err)
	}
	if len(ranges) == 1 {
		return nil
	}
	return reserveRangeExprExpansion(ec, v)
}

// reserveRangeExprExpansion refuses, BEFORE anything is expanded, when
// expanding v could not fit the evaluation's remaining operation budget.
//
// Every site that is about to expand a range_expr should reach this first,
// through one of the three wrappers that know their own context:
// reserveRangeExprCount (len, which needs the count and not the values),
// reserveRangeExprCoercion (the section 1.2.3 range_expr -> list[int] rule)
// and reserveEqualityExpansion (listOrRangeEqual's operands). Section 1.3.10
// rule 2 charges an expansion by its element count, but the charge lands once
// the elements exist -- and for a range_expr even COUNTING them used to cost
// the expansion, so the reservation added in the previous fix round could not
// help: its own input was the thing doing the work.
//
// "EVERY SITE" IS A CLAIM, AND IT WAS WRONG ONCE. The first revision of this
// comment asserted it while four sites still expanded unreserved -- equality's
// right-hand operand, callShape's argument coercion, coerceTop's target
// coercion and evalListLit's element coercion, each of them 1.6-2.8 GB under
// the server's submission budget. Three of the four have NO charge in front of
// them at all (a coercion is not a call, and list equality is charged
// left-only by an adjudicated ruling), so no amount of care about charge sites
// would have found them; they were found by measuring every construction that
// could reach rangeInts. The enumeration, with those measurements, is
// TestReserveRangeExpr_CoercionAndEqualityDoNotExpand and
// TestReserveRangeExpr_RefusalDoesNotExpand
// (reservework_internal_test.go) -- extend those first if a new expansion
// site appears, because the claim in this paragraph is only as good as they
// are.
//
// THREE OUTCOMES, in the order they are tried, and the ordering is the whole
// design:
//
//  1. The UPPER bound fits. Accept immediately. An over-estimate that fits
//     proves the true count fits, so this can never reject wrongly, and it is
//     the path every legitimate expression takes -- at no allocation.
//
//  2. The upper bound does not fit, and neither does the LOWER bound. Refuse
//     immediately, still at no allocation. The lower bound is a real count of
//     values that must survive deduplication, so this refusal is exact, not
//     conservative. Every range_expr with NON-overlapping sub-ranges lands
//     here, including the 687 MB case above: its largest sub-range alone is
//     5,000,000 values against a 10,000-operation budget.
//
//  3. The upper bound does not fit but the lower one does. The answer genuinely
//     depends on how much the sub-ranges overlap, and there is no arithmetic
//     shortcut for the size of a union of arithmetic progressions with
//     different steps (rangeExprCount's own comment says so). ONLY here is an
//     expansion worth paying for, and it buys the exact count rather than a
//     verdict -- so an expression whose sub-ranges overlap enough to fit is
//     still accepted, and this is not a tightening of the language.
//
// THE RESIDUAL, stated rather than left to be discovered: case 3 still
// expands, and is bounded only by checkElementCount's maxElements floor. It
// needs OVERLAPPING sub-ranges whose largest member fits the budget while
// their sum does not -- e.g. a thousand copies of "1-10000" -- and such text
// is bulky enough (~8 KB per position) that the submission body cap limits how
// many positions can carry it. It is the same class of residual as the
// byte-heavy wall-clock worst case recorded on maxTemplateExprPositions
// (internal/openjd/exprcheck.go), and belongs to the same later question.
func reserveRangeExprExpansion(ec evalCtx, v Value) error {
	low, high, err := rangeExprCountBounds(v)
	if err != nil {
		return err
	}
	if err := ec.m.reserveElements(high); err == nil {
		return nil // case 1
	}
	if err := ec.m.reserveElements(low); err != nil {
		return err // case 2
	}
	n, err := rangeExprCount(v) // case 3
	if err != nil {
		return err
	}
	return ec.m.reserveElements(n)
}

// reserveRangeExprCoercion reserves the expansion that coercing v to target
// would perform, when that coercion is the range_expr -> list[int] rule of
// section 1.2.3 (coerce.go). Any other pair reserves nothing: no other
// coercion in the language materializes a collection out of a compact one.
//
// It exists because a COERCION is an expansion that no charge sits in front
// of. Three of them were found by measurement rather than by reading, all
// under submissionLimits() and all pre-existing:
//
//	range_expr("1-10000000") to a list[int] TARGET      373 ms   2768 MB
//	sorted([1] + range_expr("1-10000000"))              305 ms   2768 MB
//	[1] == range_expr("1-10000000")                      97 ms   1624 MB
//
// The first two are this function's; the third is
// reserveEqualityExpansion's, below. Each ran to completion and was caught
// only afterwards -- by the memory limit, by a charge levied on the already-
// expanded value, or in the equality case not at all, since section 1.3.10
// charges list equality against the LEFT operand only.
func reserveRangeExprCoercion(ec evalCtx, v Value, target Type) error {
	if v.Type.Code != CodeRangeExpr {
		return nil
	}
	if _, ok := listElem(target); !ok {
		return nil
	}
	return reserveRangeExprExpansion(ec, v)
}

// reserveEqualityExpansion reserves the expansion listOrRangeEqual (ops.go) is
// about to perform, WITHOUT charging for it.
//
// Section 1.3.10's rule for "list/range equality comparisons" is charged
// against the LEFT operand only -- an adjudicated ruling
// (TestOperationCount_ListEquality) that this deliberately does not touch. But
// listOrRangeEqual expands whichever side is a range_expr, and for a RIGHT-hand
// one nothing stood in front of that: "[1] == range_expr(
// "1-5000000,6000000-9000000")" allocated 1603 MB in 651 ms and returned false,
// having charged three operations. Reserving is exactly the right instrument
// here, because it refuses without pricing: the charge stays left-only and no
// operation count moves.
//
// The IDENTICAL-TEXT case reserves nothing, because listOrRangeEqual answers it
// without expanding either side (see its own doc comment). That is not a
// nicety: "range_expr('1-9000000') == range_expr('1-9000000')" is true, costs
// two parses, and fits the default operation limit -- reserving both sides
// would refuse it for work it never does.
func reserveEqualityExpansion(ec evalCtx, l, r Value) error {
	if r.Type.Code != CodeRangeExpr {
		return nil
	}
	if l.Type.Code == CodeRangeExpr && l.s == r.s {
		return nil
	}
	return reserveRangeExprExpansion(ec, r)
}

// rangeExprValues expands a range_expr to its integers as a boxed []Value,
// for the one caller that actually needs the boxed form: list(range_expr)
// (funcsconv.go) becomes a list[int], so boxing there is the RESULT, not
// throwaway work. min, max and sum's own range_expr rows (funcsmath.go) used
// to box the same way and then immediately unbox again inside their reducer —
// discarding the []Value they had just built — so those now work from
// rangeInts's []int64 directly instead of calling this at all.
func rangeExprValues(v Value) ([]Value, error) {
	ints, err := rangeInts(v)
	if err != nil {
		return nil, err
	}
	vals := make([]Value, len(ints))
	for i, n := range ints {
		vals[i] = Int(n)
	}
	return vals, nil
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
