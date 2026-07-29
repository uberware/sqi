// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"fmt"
)

// maxElements is a hard safety bound on how many elements one operation may
// materialize: a list repetition, a range expansion, a concatenation or a
// slice.
//
// It ALWAYS applies and is not configurable, exactly like maxRangeValues in
// internal/openjd/range.go, and for the same reason: this package becomes
// reachable from POST /api/v1/jobs when sub-project E wires it into template
// evaluation, so "[0] * 100000000" must produce an error rather than a
// multi-gigabyte allocation.
//
// The specification's OWN limits — section 1.3.9's memory-bounded evaluation and
// section 1.3.10's operation-bounded evaluation — are sub-project E's and are
// configurable. This floor stays underneath them: it is a safety property, not a
// conformance one.
const maxElements = 10_000_000

// maxStringBytes is the same bound for a produced string, in bytes, since
// "'x' * 100000000" costs memory by length rather than by element count.
const maxStringBytes = 10_000_000

// maxParseDepth is the third bound, and the only one that applies before any
// value exists: how deep the recursive-descent parser (parser.go) may nest.
//
// The two bounds above cap what one operation ALLOCATES. This one caps the Go
// STACK the parser itself consumes, which is a strictly worse failure mode:
// exhausting it is a runtime.throw ("fatal error: stack overflow"), not a
// panic, so recover() cannot catch it and the whole process dies — including
// sqi-server, once sub-project E makes this package reachable from
// POST /api/v1/jobs. Measured before the guard existed: a list literal nested
// 200,000 deep, or 4,000,000 stacked "not" operators, killed the test binary
// outright.
//
// It counts GRAMMAR-DESCENT frames, not source brackets: one level of source
// nesting costs several units, because the guard has to sit on every function
// that takes part in a recursion cycle rather than only on the outermost one.
// Those cycles are parseConditional (the entry production, reached again
// through a list literal, a parenthesis, a subscript, and through its own
// right-associative else branch), parseNot, parseUnary, parsePower — which
// recurses into parseUnary for its exponent, and back — and parsePostfix. The
// full enumeration, with the proof that no cycle escapes it, is in parser.go's
// enter. So the effective limit on source nesting is a fraction of this number
// — one bracket costs three frames — still two orders of magnitude beyond any
// real template, and two below the depth at which the stack actually gives out.
const maxParseDepth = 500

// maxEvalDepth is maxParseDepth's counterpart for EVALUATION: how many nested
// evalNode frames one expression may use.
//
// A separate bound is needed because the parse guard cannot see this hazard at
// all. The parser builds a left-associative run — "1 + 1 + 1 + …", "true or true
// or …" — in a LOOP (parseBinaryLevel, parseLogicalLevel), so an arbitrarily
// long chain costs it no recursion and passes maxParseDepth untouched. The TREE
// it produces is left-deep, and evalBinary/evalLogical descend into the left
// operand recursively, one Go frame per operator. Measured on the machine this
// was written on, with the guard bypassed: "1 + 1 + …" evaluated at 400,000
// operators and died at 500,000 with "fatal error: stack overflow", "true or
// true or …" at 500,000 and 600,000 — the same uncatchable runtime.throw
// maxParseDepth exists to prevent, reached by a different road.
//
// The value is chosen between those two facts. 10,000 nested evaluations is
// orders of magnitude past any expression a template author writes (source
// nesting is capped far below it by maxParseDepth; only a flat chain of 10,000
// operators in one expression can reach it at all), and forty times below the
// lowest measured crash point, which leaves room for a platform with a smaller
// stack or a deeper frame than the one measured.
const maxEvalDepth = 10_000

// errTooLarge is wrapped by every bound failure so callers can match it.
var errTooLarge = errors.New("the result is too large")

// checkElementCount reports whether a result of n elements is within the bound.
// It is called with an ARITHMETIC count, before any allocation — a check made
// after allocating would be no protection at all.
//
// A negative n means the caller's own count overflowed, which is far past the
// bound rather than under it.
func checkElementCount(n int) error {
	if n < 0 || n > maxElements {
		return fmt.Errorf("%w: %d elements exceeds the limit of %d", errTooLarge, n, maxElements)
	}
	return nil
}

// checkStringBytes is checkElementCount for a produced string's byte length.
func checkStringBytes(n int) error {
	if n < 0 || n > maxStringBytes {
		return fmt.Errorf("%w: %d bytes exceeds the limit of %d", errTooLarge, n, maxStringBytes)
	}
	return nil
}

// checkRepeat bounds a REPETITION — unitSize copied n times — against limit,
// and returns the safe total. It exists because unitSize*n is exactly the
// quantity being bounded, so computing that product first and checking the
// result afterward is not a check at all: for a large enough unitSize and n,
// the product overflows int64 and wraps — possibly to a small positive
// number that sails straight past the limit check while the caller then
// loops on the raw, un-wrapped n, or to a negative number that panics an
// allocation sized from it. Comparing n against limit/unitSize instead never
// forms that product: limit and unitSize both fit comfortably within int64,
// so the division itself cannot overflow, and n > limit/unitSize implies
// n*unitSize > limit for any unitSize, n > 0 — which is what makes the
// multiplication below provably safe once this check has passed.
//
// A non-positive unitSize or n needs no bound — repeating nothing, or
// repeating something zero or fewer times, is always the empty result — and
// reports so via a zero total rather than an error.
func checkRepeat(unitSize int, n int64, limit int) (int64, error) {
	if unitSize <= 0 || n <= 0 {
		return 0, nil
	}
	if n > int64(limit)/int64(unitSize) {
		return 0, fmt.Errorf("%w: repeating %d elements %d times exceeds the limit of %d", errTooLarge, unitSize, n, limit)
	}
	return int64(unitSize) * n, nil
}
