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
// section 1.3.10's operation-bounded evaluation — landed in sub-project E1
// (meter.go's WithMemoryLimit and WithOperationLimit) and are configurable. This
// floor stays underneath them and is not raised by either: it is a safety
// property, not a conformance one, and it is what still fires on a large enough
// single operation even under a generous WithMemoryLimit — see doc.go's BOUNDED
// EVALUATION bullet for the measured example.
const maxElements = 10_000_000

// maxStringBytes is the same bound for a produced string, in bytes, since
// "'x' * 100000000" costs memory by length rather than by element count.
//
// Like maxElements, this sits underneath sub-project E1's configurable
// WithMemoryLimit (meter.go) rather than being superseded by it: raising the
// memory limit does not raise this ceiling, so "'a' * 20000000" still fails here
// with errTooLarge regardless of what WithMemoryLimit allows.
const maxStringBytes = 10_000_000

// maxParseDepth is the third bound, and the first of the two that apply before
// any value exists: how deep the recursive-descent parser (parser.go) may nest.
// (maxSourceBytes below is the other, and applies earlier still — before a
// single token is read. This paragraph said "the only one" until that bound
// existed.)
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
//
// NO PARSEABLE EXPRESSION REACHES IT ANY MORE, since maxSourceBytes below
// landed: a flat chain costs at least two source bytes an operator, so 10,000
// bytes buys about 5,000 levels — half of this. It is deliberately kept rather
// than deleted or retuned. It is the floor that catches a deep tree however it
// arrives, and Parse is not the only way one can be built (this package's own
// TestEval_RecursionDepthIsBounded now constructs the tree directly, which is
// what keeps the guard tested at all); and removing a guard because a newer,
// outer guard currently hides it is how the original hazard returns the moment
// the outer bound moves.
const maxEvalDepth = 10_000

// maxSourceBytes is the fifth bound, and the only one of the five that applies
// before the parser has read a single token: how many bytes of source text ONE
// expression may be.
//
// It closes a gap the other four left wide open. maxElements and
// maxStringBytes bound what one EVALUATION allocates; the specification's own
// configurable limits (meter.go's memory and operation budgets, sub-project
// E1), the template-wide cumulative budget (E4c) and the wall-clock deadline
// (H1) are all metered from inside an evaluation too — meter.charge and
// meter.reserve are the only places any of them is consulted. Parsing happens
// strictly BEFORE all of that: tokenize builds the whole token slice and the
// parser the whole tree, and nothing in that path ever asked how much it was
// spending. maxParseDepth does not help, because it bounds recursive-descent
// STACK FRAMES: a flat left-associative chain such as "1+1+1+…" is read by
// parseBinaryLevel in a LOOP, consumes no recursion at all, and sails past it
// — maxEvalDepth's own comment above says as much about the tree that chain
// produces.
//
// Measured on the machine this was written on, before this bound existed: a
// single 4,000,001-byte "1+1+1+…" expression parsed SUCCESSFULLY through
// Parse in 544 ms, holding 427.6 MB of live heap and churning 1,403.3 MB in
// total. Through the whole submission path a 4 MiB request body — which is
// exactly what POST /api/v1/jobs accepts, anonymously when auth is off — cost
// roughly 765 MB of peak heap per in-flight request, and six concurrent
// requests reached 4,582 MB. An already-expired deadline did not help: the
// parse ran to completion before anything looked at the clock.
//
// THE BOUND IS ON SOURCE LENGTH RATHER THAN ON NODE COUNT, which is the
// weaker of the two candidates in precision and the stronger in every other
// respect. A node-count bound would be more exact — it would charge a nested
// list literal more than the same bytes of whitespace — but it has to be
// threaded through the lexer and every parse production to be checked as the
// tree grows, and a bound the tokenizer itself can still outrun (the token
// slice for 4 MB of "+1" is already 200 MB before one node exists) is not the
// bound this hazard needs. A length check is O(1), reads no input, is
// shape-independent, and fires before a single byte is allocated. It bounds
// PEAK memory directly because expressions are parsed one at a time and
// discarded: E3's let: retains VALUES, not trees, so the peak tree a template
// can hold is one expression's worth. At this limit that peak is ~1.1 MB and
// the transient churn ~3.9 MB (measured: parsing 10,001 bytes of the same flat
// chain allocated 3.2 MB, and the worst shape tried, a 10,001-byte list
// literal, 3.7 MB) — an amplification of roughly 110x retained and 390x
// transient, applied to 10 KB instead of to the whole 4 MiB body.
//
// It ALWAYS applies and is not configurable, for the same reason as the four
// bounds above: it is a safety property rather than a conformance one, and
// nothing an operator can set in openjd.expr_* raises or lowers it. It sits
// underneath all of them.
//
// The value is 10,000 bytes, which is over 100x the largest expression that
// exists in any template sqi has ever seen. Every {{ }} body and every let:
// binding in the vendored conformance fixtures, the official samples and
// sqi's own reference presets was measured — 1,401 expressions — and the
// longest is 99 bytes, a four-line comprehension. See
// TestParse_AcceptsTheLargestRealisticExpressions, which parses the five
// largest of them verbatim and asserts the headroom, so a later attempt to
// tighten this toward real-world sizes fails a test rather than a submission.
//
// ONE KNOCK-ON, RECORDED HERE AS WELL AS AT maxEvalDepth: at this value no
// PARSEABLE expression can build a tree deep enough to reach that bound, since
// a chain costs at least two source bytes a level. maxEvalDepth stays anyway,
// for the reasons its own comment gives.
const maxSourceBytes = 10_000

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

// checkSourceBytes is checkElementCount for an expression's SOURCE length, in
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
