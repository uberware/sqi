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
