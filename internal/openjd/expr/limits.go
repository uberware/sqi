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
// It always applies and is not configurable, exactly like maxRangeValues in
// internal/openjd/range.go, and for the same reason: this package is reachable
// from POST /api/v1/jobs through template evaluation, so "[0] * 100000000" must
// produce an error rather than a multi-gigabyte allocation.
//
// The specification's own limits, section 1.3.9's memory-bounded evaluation
// and section 1.3.10's operation-bounded evaluation, are WithMemoryLimit and
// WithOperationLimit, enforced by meter.go, and are configurable. This floor
// stays underneath them and is not raised by either: it is a safety property,
// not a conformance one, and it is what still fires on a large enough single
// operation even under a generous WithMemoryLimit (see doc.go's bounded
// evaluation bullet for an example).
const maxElements = 10_000_000

// maxStringBytes is the same bound for a produced string, in bytes, since
// "'x' * 100000000" costs memory by length rather than by element count.
//
// Like maxElements, this sits underneath the configurable WithMemoryLimit
// rather than being superseded by it: raising the memory limit does
// not raise this ceiling, so "'a' * 20000000" still fails here with errTooLarge
// regardless of what WithMemoryLimit allows.
const maxStringBytes = 10_000_000

// maxParseDepth is the third bound, and the first of the two that apply before
// any value exists: how deep the recursive-descent parser (parser.go) may nest.
// (maxSourceBytes below is the other, and applies earlier still — before a
// single token is read.)
//
// The two bounds above cap what one operation allocates. This one caps the Go
// stack the parser itself consumes, which is a strictly worse failure mode:
// exhausting it is a runtime.throw ("fatal error: stack overflow"), not a
// panic, so recover() cannot catch it and the whole process dies — including
// sqi-server, which reaches this package from POST /api/v1/jobs. Without the
// guard, a list literal nested 200,000 deep, or 4,000,000 stacked "not"
// operators, kills the test binary outright.
//
// It counts grammar-descent frames, not source brackets: one level of source
// nesting costs several units, because the guard has to sit on every function
// that takes part in a recursion cycle rather than only on the outermost one.
// Those cycles are parseConditional (the entry production, reached again
// through a list literal, a parenthesis, a subscript, and through its own
// right-associative else branch), parseNot, parseUnary, parsePower — which
// recurses into parseUnary for its exponent, and back — and parsePostfix. The
// full enumeration, with the proof that no cycle escapes it, is in parser.go's
// enter. So the effective limit on source nesting is a fraction of this number
// (one bracket costs three frames), still two orders of magnitude beyond any
// real template, and two below the depth at which the stack gives out.
const maxParseDepth = 500

// maxEvalDepth is maxParseDepth's counterpart for evaluation: how many nested
// evalNode frames one expression may use.
//
// A separate bound is needed because the parse guard cannot see this hazard at
// all. The parser builds a left-associative run — "1 + 1 + 1 + …", "true or true
// or …" — in a loop (parseBinaryLevel, parseLogicalLevel), so an arbitrarily
// long chain costs it no recursion and passes maxParseDepth untouched. The tree
// it produces is left-deep, and evalBinary/evalLogical descend into the left
// operand recursively, one Go frame per operator. Unbounded, a chain of a few
// hundred thousand operators overflows the stack with "fatal error: stack
// overflow", the same uncatchable runtime.throw maxParseDepth exists to
// prevent, reached by a different road.
//
// 10,000 nested evaluations is orders of magnitude past any expression a
// template author writes (source nesting is capped far below it by
// maxParseDepth; only a flat chain of 10,000 operators in one expression can
// reach it at all), and well below the stack-overflow depth, which leaves room
// for a platform with a smaller stack or deeper frames.
//
// No parseable expression reaches it, given maxSourceBytes below: a flat chain
// costs at least two source bytes an operator, so 10,000 bytes buys about
// 5,000 levels, half of this. It is kept anyway: it is the floor that catches
// a deep tree however it arrives, and Parse is not the only way one can be
// built (TestEval_RecursionDepthIsBounded constructs the tree directly, which
// is what keeps the guard tested); removing a guard because an outer guard
// hides it would bring the hazard back the moment the outer bound moves.
const maxEvalDepth = 10_000

// maxSourceBytes is the fifth bound, and the only one of the five that applies
// before the parser has read a single token: how many bytes of source text one
// expression may be.
//
// It covers what the other four cannot. maxElements and maxStringBytes bound
// what one evaluation allocates; the specification's own configurable limits
// (meter.go's memory and operation budgets), the cumulative template-wide
// budget and the wall-clock deadline are all metered from inside an evaluation
// too — meter.charge and meter.reserve are the only places any of them is
// consulted. Parsing happens strictly before all of that: tokenize builds the
// whole token slice and the parser the whole tree, and nothing in that path
// checks how much it is spending. maxParseDepth does not help, because it
// bounds recursive-descent stack frames: a flat left-associative chain such as
// "1+1+1+…" is read by parseBinaryLevel in a loop, consumes no recursion at
// all, and passes it (maxEvalDepth's comment above says the same about the
// tree that chain produces).
//
// Without this bound, a single multi-megabyte "1+1+1+…" expression parses
// successfully into hundreds of megabytes of live heap, and a 4 MiB request
// body (what POST /api/v1/jobs accepts, anonymously when auth is off) costs
// that much per in-flight request. An expired deadline does not help: the
// parse runs to completion before anything looks at the clock.
//
// The bound is on source length rather than on node count, which is the
// weaker of the two in precision and the stronger in every other respect. A
// node-count bound would be more exact — it would charge a nested list literal
// more than the same bytes of whitespace — but it has to be threaded through
// the lexer and every parse production to be checked as the tree grows, and a
// bound the tokenizer itself can still outrun (the token slice for megabytes of
// "+1" is already enormous before one node exists) is not the bound this hazard
// needs.
// A length check is O(1), reads no input, is shape-independent, and fires
// before a single byte is allocated. It bounds peak memory directly because
// expressions are parsed one at a time and discarded: a let: binding retains
// values, not trees, so the peak tree a template can hold is one expression's
// worth. At this limit that peak is about 1 MB and the transient allocation a
// few MB, an amplification applied to 10 KB instead of to the whole 4 MiB
// body.
//
// It always applies and is not configurable, for the same reason as the four
// bounds above: it is a safety property rather than a conformance one, and
// nothing an operator can set in openjd.expr_* raises or lowers it. It sits
// underneath all of them.
//
// The value is 10,000 bytes, over 100x the largest expression in real
// templates: across every {{ }} body and let: binding in the vendored
// conformance fixtures, the official samples and sqi's reference presets, the
// longest is 99 bytes, a four-line comprehension.
// TestParse_AcceptsTheLargestRealisticExpressions parses the five largest of
// them verbatim and asserts the headroom, so an attempt to tighten this toward
// real-world sizes fails a test rather than a submission.
//
// As noted at maxEvalDepth: at this value no parseable expression can build a
// tree deep enough to reach that bound, since a chain costs at least two source
// bytes a level. maxEvalDepth stays anyway, for the reasons its own comment
// gives.
const maxSourceBytes = 10_000

// errTooLarge is wrapped by every bound failure so callers can match it.
var errTooLarge = errors.New("the result is too large")

// checkElementCount reports whether a result of n elements is within the bound.
// It is called with an arithmetic count, before any allocation — a check made
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

// checkSourceBytes is checkElementCount for an expression's source length, in
// bytes, and it is the one bound in this file that is checked before any input
// is read rather than before an allocation is made. See maxSourceBytes.
func checkSourceBytes(n int) error {
	if n > maxSourceBytes {
		return fmt.Errorf("%w: the expression is %d bytes of source, which exceeds the limit of %d",
			errTooLarge, n, maxSourceBytes)
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

// checkRepeat bounds a repetition — unitSize copied n times — against limit,
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
