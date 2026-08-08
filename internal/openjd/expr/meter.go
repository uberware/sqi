// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"fmt"
)

// The specification's recommended limits (sections 1.3.9 and 1.3.10), which are
// also what the reference implementation exposes as DEFAULT_MEMORY_LIMIT and
// DEFAULT_OPERATION_LIMIT.
//
// They are DEFAULTS, applied when a caller passes no option, rather than an
// opt-in. Two reasons, both load-bearing. Unlimited evaluation is the state this
// sub-project exists to end, and an opt-in limit leaves every caller that forgets
// the option exactly where the package was. And conformance.RunExprCase evaluates
// with no options at all, so the two expr1.3.10 fixtures only burn down if a
// no-option evaluation is bounded.
const (
	defaultMemoryLimit    int64 = 100_000_000
	defaultOperationLimit int64 = 10_000_000
)

// valueHeaderBytes is the fixed per-Value cost in the section 1.3.9 accounting.
//
// Section 1.3.9 makes value sizing explicitly implementation-defined, asking
// only that implementations "try to match the actual memory usage of each value
// as closely as practical". 64 lands near the reference's own numbers, which is
// what makes the figure in an error message plausible to a human reading it:
// the reference peaks at 128 bytes for "1 + 2" (two live values) and at 100064
// for "'a' * 100000". Nothing depends on matching it -- the oracle compares
// operation counts, never memory, precisely because memory is implementation-
// defined and a divergence there could only be suppressed, never adjudicated.
const valueHeaderBytes int64 = 64

// The two error classes section 1.3.11 enumerates by name.
var (
	errMemoryLimit    = errors.New("memory limit exceeded")
	errOperationLimit = errors.New("operation limit exceeded")
)

// meter carries the section 1.3.9 and 1.3.10 budgets for ONE evaluation.
//
// It is held by POINTER on evalCtx, and that is not a style choice. evalCtx
// flows by VALUE: 30 parameter occurrences in this package take "ec evalCtx",
// and the only *evalCtx in it is Option's own signature and the two closures
// Option returns, which mutate the context BEFORE evaluation begins and never
// during it. A counter stored as a plain evalCtx field would therefore be
// incremented on a copy and discarded when the callee returned -- it would read
// near zero no matter how much work an expression did, and every test written
// against it would pass. A shared pointer is what makes the counters accumulate
// without a sweep over every signature.
type meter struct {
	mem      int64 // live bytes
	memLimit int64
	ops      int64
	opLimit  int64
}

func newMeter(memLimit, opLimit int64) *meter {
	return &meter{memLimit: memLimit, opLimit: opLimit}
}

// alloc records a newly created value and checks the memory bound.
//
// The check happens AFTER the addition, matching section 1.3.9's own step 6
// ("Add size(result) to current memory; check if current memory exceeds limit")
// and its list example ("the limit is checked as each element is added").
func (m *meter) alloc(v Value) error {
	m.mem += sizeOf(v)
	if m.mem > m.memLimit {
		return fmt.Errorf("%w: %d bytes of live values exceeds the limit of %d",
			errMemoryLimit, m.mem, m.memLimit)
	}
	return nil
}

// release records that a value has been consumed and is no longer live.
//
// It cannot fail: freeing memory never breaches a bound. It clamps at zero
// rather than going negative, so a release-discipline bug shows up as a
// zero-balance test failure with a readable state instead of as a negative
// counter that masks a later over-allocation.
func (m *meter) release(v Value) {
	m.mem -= sizeOf(v)
	if m.mem < 0 {
		m.mem = 0
	}
}

// charge adds n operations and checks the bound (section 1.3.10).
func (m *meter) charge(n int64) error {
	m.ops += n
	if m.ops > m.opLimit {
		return fmt.Errorf("%w: %d operations exceeds the limit of %d",
			errOperationLimit, m.ops, m.opLimit)
	}
	return nil
}

// chargeElements applies section 1.3.10 rule 2: iterating every element of a
// list adds the number of elements.
func (m *meter) chargeElements(n int) error { return m.charge(int64(n)) }

// chargeBytes applies section 1.3.10 rule 3: processing a string or path value
// adds its length divided by 256, rounded up.
func (m *meter) chargeBytes(s string) error { return m.charge(ceilDiv256(len(s))) }

// ceilDiv256 is rule 3's ceil(len / 256).
//
// Note that section 1.3.10's own worked example states ceil(100000/256) = 392
// and a total of 393. That arithmetic is wrong: 256*390 = 99840 leaves 160, so
// the value is 391 and the total 392. The RULE is right and the reference
// implements it correctly (it reports 392); only the specification's addition
// is off. Recorded here because a reader checking this function against the
// spec would otherwise conclude it has an off-by-one.
func ceilDiv256(n int) int64 {
	if n <= 0 {
		return 0
	}
	return int64((n + 255) / 256)
}

// sizeOf is the section 1.3.9 size of one value: a fixed header plus payload.
//
// The list case recurses, and needs no depth guard: section 1.2.1 caps list
// nesting at two levels (list[list[T]] is legal, a third is not), so the
// recursion is bounded by the type system rather than by a counter.
func sizeOf(v Value) int64 {
	switch v.Type.Code {
	case CodeString, CodePath, CodeRangeExpr:
		return valueHeaderBytes + int64(len(v.s))
	case CodeList:
		total := valueHeaderBytes
		for _, elem := range v.l {
			total += sizeOf(elem)
		}
		return total
	default:
		// int, float, bool, nulltype and unresolved carry no payload that
		// scales with input. An unresolved value carries no payload at all.
		return valueHeaderBytes
	}
}

// EvalWithMetrics evaluates src and reports the section 1.3.10 operation count
// alongside the result, mirroring the reference implementation's
// ParsedExpression.evaluate_with_metrics. It exists for the differential oracle
// (test/oracle), which cannot read a package-private counter.
//
// It reports operations only. Section 1.3.9's memory figure is deliberately not
// exposed: value sizing is implementation-defined, so a differential comparison
// of it could only be suppressed, never adjudicated.
func EvalWithMetrics(src string, syms Symbols, target Type, opts ...Option) (Value, int64, error) {
	e, err := Parse(src)
	if err != nil {
		return Value{}, 0, err
	}
	ec := newEvalCtx(src, syms, opts)
	if ec.limitErr != nil {
		return Value{}, 0, ec.limitErr
	}
	v, err := evalNode(e.root, ec, target, 0)
	if err != nil {
		return Value{}, ec.m.ops, err
	}
	out, err := coerceTop(ec, src, e.root, v, target)
	if err != nil {
		return Value{}, ec.m.ops, err
	}
	return out, ec.m.ops, nil
}

// EvalForBalanceCheck evaluates src and reports the live memory remaining
// afterward alongside the result's own size. The two must be equal: after a
// top-level evaluation nothing but the result is live.
//
// It exists for the differential oracle, which asserts the identity over the
// whole corpus rather than over hand-picked cases. That breadth is the point: a
// release bug in one rarely-exercised function is exactly what a written-out
// test set misses. It routes through coerceTop exactly like every other public
// entry point, so the invariant covers the top-level coercion too -- without
// that, this instrument would be structurally blind to a leak or a missing
// bound check in exactly the region WithMemoryLimit most needs to reach (see
// coerceTop's own doc comment).
func EvalForBalanceCheck(src string, target Type) (live, resultSize int64, err error) {
	e, perr := Parse(src)
	if perr != nil {
		return 0, 0, perr
	}
	ec := newEvalCtx(src, MapSymbols(nil), nil)
	v, eerr := evalNode(e.root, ec, target, 0)
	if eerr != nil {
		return 0, 0, eerr
	}
	out, cerr := coerceTop(ec, src, e.root, v, target)
	if cerr != nil {
		return 0, 0, cerr
	}
	return ec.m.mem, sizeOf(out), nil
}
