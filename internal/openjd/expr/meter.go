// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"fmt"
	"math"
	"time"
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
	// deadline, when non-zero, is an absolute time after which evaluation
	// stops. It is a BACKSTOP, not a budget: it exists because section
	// 1.3.10 prices 256 bytes at one operation, so byte-heavy work is nearly
	// free in operations and expensive in seconds. Nothing else in this
	// package measures time.
	deadline time.Time
	// now is the clock, injectable for tests. Nil means time.Now.
	now func() time.Time
	// sinceCheck counts charges since the last clock read.
	sinceCheck int64
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

// deadlineCheckInterval is how many charges pass between clock reads.
//
// Reading the clock on every charge would tax the hot path of every expression
// for no benefit; 1024 charges is far below any budget worth interrupting and
// far above the cost of a time.Now(). It counts charge CALLS, not the
// operations those calls charge for -- see checkDeadline, where that
// distinction turns out to be the whole reason the first charge is sampled too.
const deadlineCheckInterval = 1024

// charge adds n operations and checks the bound (section 1.3.10), plus the
// wall-clock backstop when one is set.
//
// THE OPERATION CHECK RUNS FIRST, AND THE ORDER IS DELIBERATE. A charge that
// breaches both bounds reports the OPERATION limit, because that verdict is
// deterministic -- the same template on the same input breaches it every time,
// so it is a property of the template and callers may report it as one (422).
// A deadline breach depends on how loaded the machine was and says only that
// the server gave up (503). Reporting the deterministic failure in preference
// to the non-deterministic one is what keeps an invalid template from being
// reported as a transient server condition on a busy host.
//
// The zero-deadline short-circuit is INLINE here rather than left to
// checkDeadline, so the no-deadline path really does cost one comparison:
// checkDeadline holds an fmt.Errorf and an indirect call and does not inline,
// so tail-calling it would make every charge in the package pay for a frame it
// has no use for.
func (m *meter) charge(n int64) error {
	m.ops += n
	if m.ops > m.opLimit {
		return fmt.Errorf("%w: %d operations exceeds the limit of %d",
			errOperationLimit, m.ops, m.opLimit)
	}
	if m.deadline.IsZero() {
		return nil
	}
	return m.checkDeadline()
}

// checkDeadline reports ErrDeadlineExceeded once the wall-clock backstop has
// passed, SAMPLING the clock on the first charge and every
// deadlineCheckInterval charges thereafter.
//
// The sampling is the point, not an optimization to be tidied away: charge is
// the hottest function in the package, and a time.Now() on every call taxes
// every expression whether or not it is anywhere near a deadline. It is pinned
// by TestDeadline_ClockIsSampledNotReadOnEveryCharge, which exists precisely
// so a "simplification" to an unconditional read fails a test instead of
// passing one.
//
// The FIRST charge is checked as well as every interval thereafter, and that is
// not an off-by-one nicety -- MEASURED, the interval on its own is blind to
// exactly the workload this backstop exists for. Section 1.3.10 prices 256
// bytes at one operation, so byte-heavy work is charged in a HANDFUL OF BULK
// CALLS rather than in many small ones, and this counter counts CALLS.
// '("x" * 900000).title()' -- the ~57 ms, ~7,000-operation case the backstop
// was designed against -- made fewer than deadlineCheckInterval charge calls
// and so never read the clock at all. Neither did "1 + 1", "len([1,2,3])" or a
// 50-element comprehension.
//
// Checking at sinceCheck == 1 makes an expired deadline stop the very NEXT
// evaluation whatever its size. That is what makes this a bound on a REQUEST
// rather than on one large expression: a template is walked position by
// position, each position a separate evaluation with a FRESH meter, so without
// the first-charge check a template built entirely from small expressions could
// run arbitrarily far past an expired deadline while every meter sat below the
// interval forever.
//
// WHAT THE SAMPLING WINDOW ACTUALLY COSTS, stated exactly because an earlier
// revision of this comment understated it as "one oversized string build". A
// sampled check cannot interrupt work already in flight, and the unchecked
// window is not one bulk operation but up to deadlineCheckInterval-1 = 1023
// charge calls, EACH of which may itself be a bulk operation. The honest
// guarantee is therefore "the deadline plus at most one evaluation's remaining
// OPERATION budget" -- the call count cannot outrun that budget in practice,
// because a charge that prices bulk work adds at least one operation to it (a
// dispatched call is charged rule 1 by callShape before its work begins).
// At the server's SubmissionOperations default that is <= 10,000
// charges, and at MaxExprSubmissionOperations <= 100,000 (see
// internal/openjd/exprlimits.go). meter.reserve narrows the worst of that
// window: it is called with a count in hand immediately before every LARGE
// materialization, and checks the deadline unconditionally there, so the
// biggest single construction in the window never starts rather than being
// caught after it finishes.
func (m *meter) checkDeadline() error {
	if m.deadline.IsZero() {
		return nil
	}
	m.sinceCheck++
	if m.sinceCheck != 1 && m.sinceCheck < deadlineCheckInterval {
		return nil
	}
	if m.sinceCheck >= deadlineCheckInterval {
		// Reset to 1, not 0: at 0 the next charge would increment to 1 and
		// re-enter through the first-charge branch, reading the clock on two
		// consecutive charges every interval. Harmless, but it would make this
		// comment's "every deadlineCheckInterval charges" untrue.
		m.sinceCheck = 1
	}
	return m.deadlinePassed()
}

// deadlinePassed reads the clock -- through m.now when a test injected one --
// and reports ErrDeadlineExceeded if the deadline is behind it.
//
// The message names the bound that was breached and by how much. It must not
// restate ErrDeadlineExceeded's own text: the two are concatenated by %w, and
// this is surfaced verbatim in an HTTP response body.
func (m *meter) deadlinePassed() error {
	now := time.Now
	if m.now != nil {
		now = m.now
	}
	t := now()
	if t.After(m.deadline) {
		return fmt.Errorf("%w by %s", ErrDeadlineExceeded, t.Sub(m.deadline))
	}
	return nil
}

// chargeElements applies section 1.3.10 rule 2: iterating every element of a
// list adds the number of elements.
func (m *meter) chargeElements(n int) error { return m.charge(int64(n)) }

// chargeBytes applies section 1.3.10 rule 3: processing a string or path value
// adds its length divided by 256, rounded up.
func (m *meter) chargeBytes(s string) error { return m.charge(ceilDiv256(len(s))) }

// reserve reports whether n MORE operations would breach the bound, WITHOUT
// adding them to the running total.
//
// It exists because [meter.charge] is inherently a report AFTER the fact, and
// for a BULK operation that is not a bound at all. The charge for a produced
// collection (Cost.ResultElements, Cost.ResultBytes -- chargeResult in ops.go)
// necessarily runs once the value exists, so "[0] * 10000000" under a
// 10,000-operation limit used to materialize ten million elements (1.1 GB, 108
// ms measured) and only THEN report 10,000,001 operations against a limit of
// 10,000 -- a thousandfold overshoot of the very number the error names. The
// only thing that actually stopped it was limits.go's fixed maxElements floor,
// three orders of magnitude higher.
//
// The distinction from charge is deliberate and is what keeps this invisible to
// every SUCCESSFUL evaluation: reserve(n) fails exactly when a later charge(n)
// would have failed, and adds nothing of its own, so an evaluation that
// completes is charged precisely what it was charged before this existed. That
// is what makes the differential oracle (test/oracle) the instrument for this
// change -- its operation counts are compared only on cases whose values agree,
// i.e. only on evaluations that succeeded, and not one of them may move.
//
// Call it at a site that has an ARITHMETIC count in hand and has not yet
// allocated -- the same discipline limits.go's checkElementCount already
// follows, and usually the very next line after it. See repeatList (ops.go),
// rangeList (funcslist.go), padWidth (funcsstrpad.go), and -- for the one
// value whose count is NOT free to obtain -- reserveRangeExprExpansion
// (rangeexpr.go), which every range_expr site goes through.
//
// "ARITHMETIC count" is the load-bearing word, and getting it wrong is how
// this mechanism was defeated once already. An earlier revision of this
// comment said "elementCount answers arithmetically for a range_expr
// (rangeExprCount)". It does not: rangeExprCount is arithmetic only for a
// SINGLE sub-range, and with two or more it expands the whole range to count
// it -- so a reservation fed by it materialized 687 MB in order to decide
// that 687 MB was unaffordable, and on the success path expanded twice.
// A reservation whose own input does the work it exists to avert is not a
// reservation. Use rangeExprCountBounds (rangeexpr.go) for that value.
//
// IT ALSO CHECKS THE WALL-CLOCK DEADLINE, UNCONDITIONALLY -- no sampling, no
// counter, just the zero-deadline short-circuit that costs an evaluation with
// no deadline one comparison. That is affordable precisely because of what
// this function is for: it is called only where a large materialization is
// about to begin, with the count already in hand, and MEASURED it fires orders
// of magnitude less often than charge does -- 2 calls against 100,003 charges
// for '[x * 2 for x in range(100000)]', 1 against 4 for
// '("x" * 900000).title()', 1 against 6 for
// 'sorted([1] + range_expr("1-100000"))', and 0 for "1 + 1" or "len([1,2,3])".
// Nothing measured reached double figures. Checking here converts "the largest
// construction in the sampling window runs to completion and is caught on the
// charge after it" into "it never starts", which is exactly the workload the
// backstop exists for -- see checkDeadline for what the window still costs.
//
// A CALLER THAT READS THIS ERROR AS A VERDICT ABOUT SIZE must now separate the
// two: reserveRangeExprExpansion (rangeexpr.go) treats a reserve failure as
// "the count does not fit" and falls through to a cheaper probe, which for a
// deadline error would be both wrong and, when the fallback reserves zero,
// silently non-refusing. It checks errors.Is(err, ErrDeadlineExceeded) for
// exactly that reason. It is the only such caller; every other one propagates.
func (m *meter) reserve(n int64) error {
	if n > 0 && m.ops+n > m.opLimit {
		return fmt.Errorf("%w: %d operations exceeds the limit of %d",
			errOperationLimit, m.ops+n, m.opLimit)
	}
	if m.deadline.IsZero() {
		return nil
	}
	return m.deadlinePassed()
}

// reserveElements is [meter.reserve] for a rule-2 element count that is known
// before the elements are built.
func (m *meter) reserveElements(n int) error { return m.reserve(int64(n)) }

// reserveBytes is [meter.reserve] for a rule-3 byte length that is known before
// the string is built. It takes the LENGTH rather than the string itself --
// which is the whole point, since the string does not exist yet.
func (m *meter) reserveBytes(n int64) error {
	if n <= 0 {
		return nil
	}
	return m.reserve((n + 255) / 256)
}

// unmeteredCtx returns an evalCtx whose meter can refuse nothing.
//
// It exists for exactly ONE caller -- paddedNumber (pathnumber.go), which
// renders with_number's zero padding through zfillString from outside any
// evaluation's own budget, and whose width is already bounded to
// maxNumberPadding (32) characters by the caller above it, so there is
// nothing here for a budget to bound.
//
// Do not reach for it anywhere else. A zero-value evalCtx would carry a NIL
// meter and panic on the first charge, which is why this returns a real one;
// but a real evaluation path routed through this context is unbounded by
// construction, which is the state sub-project E1 exists to have ended.
func unmeteredCtx() evalCtx {
	return evalCtx{m: newMeter(math.MaxInt64, math.MaxInt64)}
}

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

// SizeOf reports the section 1.3.9 size, in bytes, that this package charges
// for holding v live -- the same figure [meter.alloc] adds to an evaluation's
// memory budget.
//
// It exists for callers that RETAIN values across evaluations, which the
// per-Eval meter by construction cannot bound: each Eval starts with a fresh
// budget, so a caller that keeps N results alive is exposed to N times the
// per-Eval limit no matter how tight that limit is. internal/worker/fmtres'
// let: evaluator is the case this was added for (an EXPR let: block binds up
// to 50 values into one symbol table and holds them for the whole task); the
// server-side checker discards every result, which is exactly why a per-Eval
// cap sufficed there.
//
// The figure is implementation-defined by section 1.3.9's own admission
// ("try to match the actual memory usage of each value as closely as
// practical"), so treat it as a budgeting unit, not as a Go heap
// measurement.
func SizeOf(v Value) int64 { return sizeOf(v) }

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
//
// WHAT THE CORPUS-WIDE SWEEP DOES NOT REACH, stated because "over the whole
// corpus" reads broader than it delivers: this function takes no Symbols and
// passes MapSymbols(nil), and test/oracle/corpus.txt contains NO SYMBOL
// REFERENCES AT ALL. So no corpus case ever produces an unresolved value,
// and the sweep touches none of the roughly thirteen release sites on the
// unresolved paths (evalIndex's and evalSlice's unresolved branches,
// sliceComponent, condResult, applyBinary's and callFunction's
// unresolved-operand returns, and their siblings) and no symbol-backed
// receiver. That is a COVERAGE gap, not a known correctness one: the final
// whole-branch review probed 67 symbol-bearing and unresolved-path
// expressions by hand and every one balanced exactly. Closing it properly
// needs a corpus that binds symbols, which is sub-project E's to add when
// template integration gives it a reason to.
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
