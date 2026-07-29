// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/uberware/sqi/internal/openjd/intrange"
)

// maxRangeValues is a hard resource-exhaustion safety bound on the number of
// integers an INT/CHUNK[INT] range expression may materialize. It is
// independent of the spec's gated per-parameter value limit
// (maxTaskParamValues): this guard ALWAYS applies — even when
// EnforceLimits=false — to keep a hostile template (e.g. "1-2000000000") from
// triggering a multi-gigabyte allocation reachable from POST /api/v1/jobs.
const maxRangeValues = 10_000_000

// errRangeTooLarge is returned (wrapped) when a range expression's arithmetic
// value count exceeds maxRangeValues, before any slice is allocated.
var errRangeTooLarge = errors.New("openjd: range expression expands to too many values")

// intRange is the shared range element type. internal/openjd is STRICTER than
// the base specification about what it accepts — see openjdRangePolicy — and
// that strictness is preserved deliberately: relaxing it would start accepting
// templates this package rejects today.
type intRange = intrange.Range

// openjdRangePolicy is this package's long-standing acceptance rule, now stated
// explicitly rather than baked into the parser: a step must be positive and a
// range must not run backwards. The specification permits both; see the
// intrange package doc.
var openjdRangePolicy = intrange.Policy{PositiveStepOnly: true, AscendingOnly: true}

// parseIntRangeExpr parses an OpenJD integer range expression string.
//
// Grammar:
//
//	<RangeExpr>  ::= <Element> (',' <Element>)*
//	<Element>    ::= <Number> | <Number>'-'<Number> | <Number>'-'<Number>':'<Step>
//	<Number>     ::= optional sign + decimal digits
//	<Step>       ::= non-zero positive decimal integer
//
// Example inputs: "1", "1-100", "1-100:2", "1,5,10", "1-10,20-30", "0-100:5,200".
//
// Before materializing, the arithmetic value count is summed across all
// elements; if it exceeds [maxRangeValues] an error is returned WITHOUT
// allocating the result slice. Valid (in-bounds) expressions expand exactly as
// before, preserving de-duplication and first-seen ordering.
func parseIntRangeExpr(expr string) ([]int, error) {
	ranges, err := parseIntRangeElements(expr)
	if err != nil {
		return nil, err
	}

	// Resource-exhaustion guard: sum arithmetic counts before allocating.
	total, ok := sumRangeCounts(ranges)
	if !ok {
		return nil, fmt.Errorf("openjd: range expression %q: %w (limit %d)", expr, errRangeTooLarge, maxRangeValues)
	}

	seen := make(map[int]struct{}, total)
	result := make([]int, 0, total)
	for _, r := range ranges {
		for _, v := range r.Iterate() {
			if _, dup := seen[v]; !dup {
				seen[v] = struct{}{}
				result = append(result, v)
			}
		}
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("openjd: range expression %q produced no values", expr)
	}
	return result, nil
}

// intRangeExprCount returns the number of values an INT/CHUNK[INT] range
// expression expands to, computed arithmetically (sum of each element's
// [intRange.count]) WITHOUT materializing the integers. ok is false when the
// expression is unparseable or its arithmetic count exceeds [maxRangeValues]
// (the latter is reported by the structural parse check, so callers avoid
// double-reporting). The summed count may exceed the de-duplicated count when
// sub-ranges overlap; overlap is flagged separately by [intRangeHasOverlap].
func intRangeExprCount(expr string) (count int, ok bool) {
	ranges, err := parseIntRangeElements(expr)
	if err != nil {
		return 0, false
	}
	return sumRangeCounts(ranges)
}

// sumRangeCounts sums the arithmetic value counts of every sub-range, returning
// ok=false (and total 0) as soon as any single range — or the running total —
// exceeds maxRangeValues. It never materializes the integers, so it is the
// allocation-free guard shared by expansion, counting, and validation.
func sumRangeCounts(ranges []intRange) (total int, ok bool) {
	for _, r := range ranges {
		c := r.Count()
		if c > maxRangeValues {
			return 0, false
		}
		total += c
		if total > maxRangeValues {
			return 0, false
		}
	}
	return total, true
}

// validateIntRangeExpr reports the first error an INT/CHUNK[INT] range
// expression would raise during expansion — a parse error, the
// resource-exhaustion guard, or an expression that yields no values — WITHOUT
// materializing the value slice. It mirrors the errors of [parseIntRangeExpr]
// so submit-time validation can run cheaply even for large in-bounds ranges.
func validateIntRangeExpr(expr string) error {
	ranges, err := parseIntRangeElements(expr)
	if err != nil {
		return err
	}
	total, ok := sumRangeCounts(ranges)
	if !ok {
		return fmt.Errorf("openjd: range expression %q: %w (limit %d)", expr, errRangeTooLarge, maxRangeValues)
	}
	if total == 0 {
		return fmt.Errorf("openjd: range expression %q produced no values", expr)
	}
	return nil
}

// intRangeHasOverlap reports whether an OpenJD INT/CHUNK[INT] range expression
// produces the same integer from more than one element (i.e. overlapping
// sub-ranges or duplicate values). The OpenJD jobtemplate-2023-09 spec forbids
// overlapping sub-ranges; [parseIntRangeExpr] silently de-duplicates for
// expansion, so this is a separate detection used only by validation.
//
// Detection is performed by pairwise arithmetic intersection of the sub-ranges'
// arithmetic progressions (start, end, step) via the CRT — it never
// materializes the integers, so it is safe for arbitrarily large ranges. Two
// stepped ranges that share an interval but no common value (e.g. "1-10:2" and
// "2-10:2") are correctly reported as non-overlapping.
//
// Expressions containing a format string ("{{") cannot be counted before
// resolution and report no overlap. A parse error is returned to the caller,
// which may ignore it (the structural parse check reports it separately).
func intRangeHasOverlap(expr string) (overlap bool, err error) {
	if strings.Contains(expr, "{{") {
		return false, nil
	}

	ranges, perr := parseIntRangeElements(expr)
	if perr != nil {
		return false, perr
	}

	for i := range ranges {
		for j := i + 1; j < len(ranges); j++ {
			if rangesIntersect(ranges[i], ranges[j]) {
				return true, nil
			}
		}
	}
	return false, nil
}

// rangesIntersect reports whether two stepped integer ranges share at least one
// value. It treats each range as an arithmetic progression and solves for a
// common term using the Chinese Remainder Theorem; no integers are
// materialized.
func rangesIntersect(a, b intRange) bool {
	if a.Step <= 0 {
		a.Step = 1
	}
	if b.Step <= 0 {
		b.Step = 1
	}
	if a.Start > a.End || b.Start > b.End {
		return false
	}

	lo := max(a.Start, b.Start)
	hi := min(a.End, b.End)
	if lo > hi {
		return false // disjoint intervals
	}

	// Solve v ≡ a.Start (mod a.Step) and v ≡ b.Start (mod b.Step).
	g := gcd(a.Step, b.Step)
	diff := b.Start - a.Start
	if diff%g != 0 {
		return false // congruences are incompatible: no common value
	}

	lcm := a.Step / g * b.Step
	m := b.Step / g
	inv := modInverse((a.Step/g)%m, m)
	t := mod(mod(diff/g, m)*inv, m)
	x0 := a.Start + a.Step*t // a particular common solution, mod lcm

	// Smallest v ≥ lo with v ≡ x0 (mod lcm).
	v := lo + mod(x0-lo, lcm)
	return v <= hi
}

// mod returns a mod m normalized to [0, m).
func mod(a, m int) int {
	r := a % m
	if r < 0 {
		r += m
	}
	return r
}

// gcd returns the (non-negative) greatest common divisor of a and b.
func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		a = -a
	}
	return a
}

// modInverse returns the modular multiplicative inverse of a modulo m, assuming
// gcd(a, m) == 1. For m == 1 the inverse is 0 by convention.
func modInverse(a, m int) int {
	if m == 1 {
		return 0
	}
	_, x, _ := extGCD(mod(a, m), m)
	return mod(x, m)
}

// extGCD returns g = gcd(a, b) and Bézout coefficients x, y with a*x + b*y = g.
func extGCD(a, b int) (g, x, y int) {
	if b == 0 {
		return a, 1, 0
	}
	g, x1, y1 := extGCD(b, a%b)
	return g, y1, x1 - (a/b)*y1
}

// parseIntRangeElements parses an INT range expression into its sub-ranges
// WITHOUT materializing the integers, applying openjdRangePolicy. Empty
// comma-separated parts are skipped.
//
// Two special cases preserve error strings that predate intrange and are
// pinned by TestParseAndValidateIntRangeExpr_ErrorStringsUnchanged: a wholly
// empty (or whitespace-only) expression reports "range expression is empty"
// directly, and an expression that is non-empty but whose parts are ALL empty
// (e.g. ",,") reports no error here at all — intrange.ParseWithPolicy
// collapses both cases to the same "range expression is empty" error, but
// this package's callers (parseIntRangeExpr, validateIntRangeExpr) tell the
// two apart themselves, reporting "produced no values" for the latter.
func parseIntRangeElements(expr string) ([]intRange, error) {
	if strings.TrimSpace(expr) == "" {
		return nil, errors.New("openjd: range expression is empty")
	}
	ranges, err := intrange.ParseWithPolicy(expr, openjdRangePolicy)
	if err != nil {
		if err.Error() == "range expression is empty" {
			return nil, nil
		}
		return nil, fmt.Errorf("openjd: range expression %q: %w", expr, err)
	}
	return ranges, nil
}
