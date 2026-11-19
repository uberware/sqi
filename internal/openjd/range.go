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
//
// Its text carries NO "openjd: " prefix and does not name the range expression
// again: both call sites wrap it in "openjd: range expression %q: " already, so
// a prefix here produced "openjd: range expression \"1-2000000000\": openjd:
// range expression expands to too many values". Every other error reaching those
// same call sites (from internal/openjd/intrange) is likewise a bare phrase.
var errRangeTooLarge = errors.New("expands to too many values")

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

// intRangeHasOverlap reports whether two elements of an OpenJD INT/CHUNK[INT]
// range expression overlap. Template Schemas §3.4.1.1.1 measures overlap on an
// element's SPAN — the interval from the smallest to the largest value in its
// set, not its written endpoints — and two elements overlap when their spans
// share an integer, even when their sets share no value: "1-9:2,2-10:2" is an
// error, while "1-10:4,10-15" is not, since the span of "1-10:4" ends at 9.
// [parseIntRangeExpr] silently de-duplicates for expansion, so this is a
// separate detection used only by validation.
//
// The spans are computed arithmetically by intrange.Overlaps, so the integers
// are never materialized and arbitrarily large ranges are safe.
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

	return intrange.Overlaps(ranges), nil
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
