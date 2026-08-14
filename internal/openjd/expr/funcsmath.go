// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"fmt"
	"math"
	"strconv"
)

// mathFuncs is RFC 0006's math-function group: abs, min, max, sum, floor,
// ceil, round. A later sub-project never edits this table — C2, C3 and C4 add
// their own groups in their own files (funcsstrcase.go, funcsstrfind.go,
// funcsstrsplit.go, funcsstrpad.go, funcsre.go, funcsrepr.go, funcspath.go)
// and their own entry in funcs.go's mergeFuncs call.
var mathFuncs = map[string][]Shape{
	// Section 2.2.3's round(). Three rows, and the two-argument row's return
	// type is a union because RFC 0006 makes it depend on the VALUE of ndigits:
	// int when ndigits <= 0, float when positive. That cannot be decided
	// statically, so the declared type names both and the runtime picks.
	//
	// NOTE a deliberate divergence from the reference implementation, which
	// returns 1230.0 typed float for round(1234.5, -1). RFC 0006's signature
	// table says "returns int when ndigits <= 0", and the specification
	// outranks the reference; see test/oracle/baseline.txt.
	//
	// COST (sub-project E1, Task 8): Cost{} on all three -- round takes no
	// string, path or list argument, so neither rule 2 nor rule 3 names any
	// charge here. NOT REPRODUCED: the reference's own count for the
	// (float, int) and (int, int) rows is NOT flat -- it adds ceil(ndigits/256)
	// when ndigits is POSITIVE (round(2.0,1) measures 2, round(2.0,300)
	// measures 3, round(2.0,600) measures 4) and stays flat for ndigits <= 0
	// no matter its magnitude (round(2.0,-600) still measures 2). This tracks
	// the RENDERED decimal string's length, but that string lives on
	// Value.fs (floatRendered, value.go), a field Cost's ArgBytes/ResultBytes
	// deliberately never read (value.go's own doc: "nothing propagates it" --
	// section 1.3.4's rule, not a shortcut). Reproducing the reference's count
	// would require charging bytes for a presentation-only field no other Cost
	// row in this package touches, on an argument (ndigits, an int) that rule
	// 3 does not name ("a string or path value"). Left uncharged, per the
	// standing rule that an unreproducible reference count is not chased
	// absent spec text to justify it. See cost_misc_internal_test.go's PROBE.
	"round": {
		{Params: []Type{TFloat}, Ret: TInt, Fn: func(args []Value) (Value, error) {
			n, err := floatToInt(math.RoundToEven(args[0].AsFloat()))
			if err != nil {
				return Value{}, err
			}
			return Int(n), nil
		}},
		{Params: []Type{TFloat, TInt}, Ret: UnionOf(TInt, TFloat), Fn: func(args []Value) (Value, error) {
			return roundToDigits(args[0].AsFloat(), args[1].AsInt())
		}},
		{Params: []Type{TInt, TInt}, Ret: TInt, Fn: func(args []Value) (Value, error) {
			return roundIntToDigits(args[0].AsInt(), args[1].AsInt())
		}},
	},
	// RFC 0006 writes abs, min and max with a constrained type variable
	// ("T in int, float"). Monomorphic rows express that exactly, with no new
	// machinery: argCost ranks an exact match below a widening one, so abs(-5)
	// lands on the int row and abs(-2.5) on the float row, and a constrained
	// variable would only re-derive that ranking by hand.
	//
	// COST (sub-project E1, Task 8): Cost{} on both rows -- abs takes a single
	// scalar and does no list or string/path work; confirmed flat at 1 (rule 1
	// only) in the reference for both int and float, isolated from unary
	// minus's own separate charge (abs(-1) measures 2, but "-1" alone already
	// measures 1 -- abs's own share is exactly 1).
	"abs": {
		{Params: []Type{TInt}, Ret: TInt, Fn: func(args []Value) (Value, error) {
			n := args[0].AsInt()
			if n == math.MinInt64 {
				// -MinInt64 is not representable, and Go's unary minus returns
				// MinInt64 again rather than trapping. Section 2.1.1 already
				// reports this condition for arithmetic; abs joins it.
				return Value{}, errIntOverflow
			}
			if n < 0 {
				return Int(-n), nil
			}
			return Int(n), nil
		}},
		{Params: []Type{TFloat}, Ret: TFloat, Fn: func(args []Value) (Value, error) {
			return floatValue(math.Abs(args[0].AsFloat()))
		}},
	},
	// floor and ceil both return int from EITHER argument type, which is RFC
	// 0006's signature and not an oversight: their whole purpose is the
	// destructive conversion int() refuses to perform.
	//
	// COST (sub-project E1, Task 8): Cost{} on all four rows (floor and ceil
	// together) -- same reasoning as abs: a single scalar in, a single scalar
	// out, confirmed flat at 1 in the reference for both argument types.
	"floor": {
		{Params: []Type{TInt}, Ret: TInt, Fn: func(args []Value) (Value, error) {
			return args[0], nil
		}},
		{Params: []Type{TFloat}, Ret: TInt, Fn: func(args []Value) (Value, error) {
			n, err := floatToInt(math.Floor(args[0].AsFloat()))
			if err != nil {
				return Value{}, err
			}
			return Int(n), nil
		}},
	},
	"ceil": {
		{Params: []Type{TInt}, Ret: TInt, Fn: func(args []Value) (Value, error) {
			return args[0], nil
		}},
		{Params: []Type{TFloat}, Ret: TInt, Fn: func(args []Value) (Value, error) {
			n, err := floatToInt(math.Ceil(args[0].AsFloat()))
			if err != nil {
				return Value{}, err
			}
			return Int(n), nil
		}},
	},
	// COST (sub-project E1, Task 8), min and max together: rule 2 names both
	// by name ("built-in functions that iterate lists such as ... min(),
	// max() ..."). The two LIST rows (ListOf(TInt), ListOf(TFloat)) declare
	// Cost{ArgElements: {0}}, confirmed scaling in the reference
	// (min([3,1,2]) measures 4 = 1 call + 3 elements).
	//
	// The FIXED-ARITY (T,T) and (T,T,T) rows, and the list[nulltype] and
	// range_expr rows, are ALL Cost{} -- two separate DIVERGENCES from the
	// reference, neither reproducible with the Cost mechanism as built:
	//
	//  1. The reference's own count for the SCALAR 2-arg and 3-arg rows is
	//     NOT flat: min(1,2) measures 3 and min(1,2,3) measures 4 -- exactly
	//     1 (the call) plus N (the argument COUNT), the identical formula the
	//     list rows use, as if the reference internally treats every
	//     min/max call as "iterate a slice of N values" regardless of
	//     whether the N came from unpacking a list argument or from N
	//     separate positional arguments. RFC 0006 declares min(T,T) and
	//     min(T,T,T) as distinct, FIXED-ARITY overloads from min(list[T]) --
	//     neither takes a list value at all -- and rule 2's own text requires
	//     "iterates through every element of A LIST" for the charge to apply.
	//     Reproducing the reference's count here would need a NEW Cost
	//     mechanism (an unconditional per-Shape charge equal to len(Params),
	//     unrelated to any argument's VALUE), which every other charge in
	//     this package keys off instead, and which is outside this task's
	//     brief. It also would not serve section 1.3.10's own stated purpose:
	//     the arity here is fixed by the overload (2 or 3), so there is no
	//     template-author-controlled quantity to bound.
	//
	//  2. CORRECTION (final whole-branch review, sub-project E1). The
	//     range_expr rows below are now Cost{ArgElements: {0}}. An earlier
	//     revision left them uncharged, arguing: "The range_expr row's
	//     reference count does NOT scale with the range's size
	//     (min(range_expr('1-5')) and min(range_expr('1-100000')) both
	//     measure 3), matching the EXACT shape of the range_expr divergence
	//     Task 5 already found and left uncharged for the 'in'/'not in'
	//     operator's own range_expr row (ops.go's OpIn/OpNotIn,
	//     TInt/TRangeExpr) ... it is not rule 2 (nothing scales, so nothing
	//     is being 'iterated' in the charged sense) ... This task follows
	//     that precedent rather than re-litigating it."
	//
	//     The measurements are accurate; the inference from them is not.
	//     "Nothing scales" was a fact about the REFERENCE, and this package's
	//     standing rule subordinates the reference to the specification --
	//     so the precedent being followed was an unadjudicated one, and
	//     following it propagated a single unexamined ruling into a second
	//     table. Rule 2's own test is what decides it, and sqi's own Fn
	//     bodies below fail it plainly: both call rangeInts(args[0]), which
	//     expands the range in full, with no early exit and no arithmetic
	//     shortcut from the endpoints. The reference may well find its
	//     answer without touching each value; sqi provably does touch each
	//     one. The inconsistency was visible inside this very file, too --
	//     sum's range_expr row (below) already charged Cost{ArgElements: {0}}
	//     for a byte-identical rangeInts body. Measured before the fix,
	//     min(Param.R) and max(Param.R) with R = range_expr("1-1000000")
	//     each charged 1 operation while expanding a million integers.
	//     ops.go's OpIn/OpNotIn range_expr rows were corrected in the same
	//     pass, so the cited precedent now points the other way. Each is a
	//     new count divergence from the reference, baselined in
	//     test/oracle/baseline-ops.txt; the flat +1 residual described above
	//     is unchanged and remains the reference's own.
	//
	// See cost_misc_internal_test.go's PROBE for the transcribed measurements
	// of both divergences, and TestMinMax_EmptyList_SelectsNoReturnRow
	// (below the tables) for why the list[nulltype] row's own Cost{} needs no
	// separate justification: it never reaches a success path to charge
	// anything from.
	"min": {
		{Params: []Type{TInt, TInt}, Ret: TInt, Fn: func(args []Value) (Value, error) {
			return extremumInt(args, true)
		}},
		{Params: []Type{TFloat, TFloat}, Ret: TFloat, Fn: func(args []Value) (Value, error) {
			return extremumFloat(args, true)
		}},
		{Params: []Type{TInt, TInt, TInt}, Ret: TInt, Fn: func(args []Value) (Value, error) {
			return extremumInt(args, true)
		}},
		{Params: []Type{TFloat, TFloat, TFloat}, Ret: TFloat, Fn: func(args []Value) (Value, error) {
			return extremumFloat(args, true)
		}},
		{Params: []Type{ListOf(TInt)}, Ret: TInt, Cost: Cost{ArgElements: []int{0}}, Fn: func(args []Value) (Value, error) {
			return extremumInt(args[0].AsList(), true)
		}},
		{Params: []Type{ListOf(TFloat)}, Ret: TFloat, Cost: Cost{ArgElements: []int{0}}, Fn: func(args []Value) (Value, error) {
			return extremumFloat(args[0].AsList(), true)
		}},
		// The empty literal's own row. list[nulltype] matches it EXACTLY (cost
		// 0, via shape.go's argCostList), while the list[int] row above only
		// reaches the empty argument by section 1.2.6 rule 6's widening (cost
		// 1) — so this row wins outright ON COST, not by tying and falling
		// back to registration order, and RFC 0006's wording is what the user
		// sees. A prior revision of argCostList scored every concrete element
		// type — including a param whose own element was CodeNull — as the
		// same cost-1 widen, which tied this row with list[int]/list[float]
		// and let the earlier-registered list[int] row win the tie instead;
		// the fix in shape.go's argCostList carves out list[nulltype] vs
		// list[nulltype] as the exact match it always should have been. See
		// TestMinMax_EmptyList_SelectsNoReturnRow, which asserts on the
		// returned Shape itself so this cannot regress silently again.
		{Params: []Type{ListOf(TNull)}, Ret: TNoReturn, Fn: func([]Value) (Value, error) {
			return Value{}, emptyListError("min")
		}},
		{Params: []Type{TRangeExpr}, Ret: TInt, Cost: Cost{ArgElements: []int{0}}, Fn: func(args []Value) (Value, error) {
			ints, err := rangeInts(args[0])
			if err != nil {
				return Value{}, err
			}
			return extremumInts(ints, true)
		}},
	},
	"max": {
		{Params: []Type{TInt, TInt}, Ret: TInt, Fn: func(args []Value) (Value, error) {
			return extremumInt(args, false)
		}},
		{Params: []Type{TFloat, TFloat}, Ret: TFloat, Fn: func(args []Value) (Value, error) {
			return extremumFloat(args, false)
		}},
		{Params: []Type{TInt, TInt, TInt}, Ret: TInt, Fn: func(args []Value) (Value, error) {
			return extremumInt(args, false)
		}},
		{Params: []Type{TFloat, TFloat, TFloat}, Ret: TFloat, Fn: func(args []Value) (Value, error) {
			return extremumFloat(args, false)
		}},
		{Params: []Type{ListOf(TInt)}, Ret: TInt, Cost: Cost{ArgElements: []int{0}}, Fn: func(args []Value) (Value, error) {
			return extremumInt(args[0].AsList(), false)
		}},
		{Params: []Type{ListOf(TFloat)}, Ret: TFloat, Cost: Cost{ArgElements: []int{0}}, Fn: func(args []Value) (Value, error) {
			return extremumFloat(args[0].AsList(), false)
		}},
		{Params: []Type{ListOf(TNull)}, Ret: TNoReturn, Fn: func([]Value) (Value, error) {
			return Value{}, emptyListError("max")
		}},
		{Params: []Type{TRangeExpr}, Ret: TInt, Cost: Cost{ArgElements: []int{0}}, Fn: func(args []Value) (Value, error) {
			ints, err := rangeInts(args[0])
			if err != nil {
				return Value{}, err
			}
			return extremumInts(ints, false)
		}},
	},
	// sum's empty-list row returns 0 rather than erroring — RFC 0006 says so
	// explicitly, and it is the mathematically empty sum. That is the one place
	// sum and min/max part company on the same argument.
	//
	// This row wins against list[int]/list[float] below ON COST (shape.go's
	// argCostList scores list[nulltype] vs list[nulltype] as an exact match,
	// cost 0, against those rows' cost-1 widen), not because it happens to be
	// registered first — TestSum_EmptyListIsOrderIndependent pins that
	// reordering these rows does not change the result, which is what a
	// position-dependent tie would have let happen.
	//
	// COST (sub-project E1, Task 8): rule 2 names sum() by name. The two list
	// rows declare Cost{ArgElements: {0}} (list[nulltype] is Cost{}: its
	// element count is always zero, so there is nothing to charge). The
	// range_expr row ALSO declares Cost{ArgElements: {0}}, and here it costs
	// no divergence at all: the reference is confirmed SCALING on this one
	// (sum(range_expr("1-100")) measures 102, sum(range_expr("1-1000"))
	// measures 1002, both exactly 1 + N) and sqi reproduces it exactly, since
	// range_expr's own construction already charges nothing (funcsconv.go),
	// leaving sum's own share at 1 (the call) + N (elementCount, which
	// resolves a range_expr via rangeExprCount, ops.go's elementCount).
	//
	// The reference does NOT scale on min/max's identical range_expr row --
	// apparently finding its answer arithmetically from the range's
	// endpoints. That is a difference in the reference's implementations,
	// not a license for sqi to differ in its own: sqi's min, max and sum
	// range_expr bodies all call rangeInts and all expand in full, so all
	// three charge ArgElements{0}. min/max's rows were brought into line in
	// the final whole-branch review and now diverge from the reference where
	// this row agrees with it; see min's own COST comment above for that
	// correction.
	"sum": {
		{Params: []Type{ListOf(TNull)}, Ret: TInt, Fn: func([]Value) (Value, error) {
			return Int(0), nil
		}},
		{Params: []Type{ListOf(TInt)}, Ret: TInt, Cost: Cost{ArgElements: []int{0}}, Fn: func(args []Value) (Value, error) {
			return sumInts(args[0].AsList())
		}},
		{Params: []Type{ListOf(TFloat)}, Ret: TFloat, Cost: Cost{ArgElements: []int{0}}, Fn: func(args []Value) (Value, error) {
			total := 0.0
			for _, v := range args[0].AsList() {
				total += v.AsFloat()
			}
			return floatValue(total)
		}},
		{Params: []Type{TRangeExpr}, Ret: TInt, Cost: Cost{ArgElements: []int{0}}, Fn: func(args []Value) (Value, error) {
			ints, err := rangeInts(args[0])
			if err != nil {
				return Value{}, err
			}
			return sumInt64s(ints)
		}},
	},
}

// floatToInt narrows a float64 to an int64, guarding the one case Go leaves
// implementation-defined: converting a value outside int64's representable
// range. Measured on this same source, "int64(f)" for such an f returns
// math.MaxInt64 on arm64 and math.MinInt64 on amd64 — a silently wrong answer
// that differs by build architecture rather than erroring, where section
// 2.1.1 requires an integer overflow to be reported. abs's int row (above)
// already guards its own single unrepresentable case (-MinInt64) the same
// way; this is the shared version for round, floor and ceil, which can land
// on any float value at all rather than one fixed one.
//
// Both bounds are exactly representable as float64 (2^63 and -2^63), so the
// comparison is exact. This deliberately does NOT use toInt's (coerce.go)
// "float64(i) != f" round-trip test: floor, ceil and round exist precisely to
// discard a fraction, so that test would reject the very input these
// functions are meant to accept.
func floatToInt(f float64) (int64, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) || f < -9223372036854775808.0 || f >= 9223372036854775808.0 {
		return 0, errIntOverflow
	}
	return int64(f), nil
}

// roundToDigits implements round(float, int).
//
// A positive ndigits returns a FLOAT carrying its own rendered form, because
// RFC 0006 requires round(3.5, 2) to render "3.50" — strconv's 'f' format with
// an explicit precision is exactly that text, and it is what the reference
// produces. A non-positive ndigits returns an INT, per RFC 0006's signature
// table, so there is nothing to carry.
func roundToDigits(f float64, ndigits int64) (Value, error) {
	if ndigits > 0 {
		// ndigits is an arbitrary int and the rendered form is proportional to
		// it, so this is the one unbounded path the carry opens. Bounding
		// ndigits directly avoids formatting a gigabyte of zeros in order to
		// discover it was too long.
		if ndigits > maxStringBytes {
			return Value{}, fmt.Errorf("%w: %d decimal places exceeds the limit of %d",
				errTooLarge, ndigits, maxStringBytes)
		}
		// Scaling is only usable while both the scale and the scaled value stay
		// finite. math.Pow(10, ndigits) is +Inf from ndigits 309 up, and below
		// that f*scale can still overflow on its own for a large enough f — so
		// the unusable window depends on f's magnitude, not on ndigits alone
		// (round(2.0, 308) and round(1e300, 300) both land in it).
		//
		// Past that point the rounding is the IDENTITY, and that is the answer
		// rather than an error: no float64 carries enough precision for a
		// rounding at 1e-308 resolution to change it, so f is already its own
		// round-to-even at that place. round(2.0, 307) returns 2.000…0 and
		// there is no mathematical discontinuity at 308 — only an artifact of
		// the multiply. Computing it directly is the same move the negative
		// branch below makes, for the same reason.
		rounded := f
		if scale := math.Pow(10, float64(ndigits)); !math.IsInf(scale, 0) {
			if scaled := f * scale; !math.IsInf(scaled, 0) {
				rounded = math.RoundToEven(scaled) / scale
			}
		}
		out, err := floatValue(rounded)
		if err != nil {
			return Value{}, err
		}
		return floatRendered(out.AsFloat(), strconv.FormatFloat(out.AsFloat(), 'f', int(ndigits), 64)), nil
	}
	// Built the same bounded way roundIntToDigits builds its own scale below,
	// rather than math.Pow(10, float64(-ndigits)): for a large enough
	// -ndigits (round(3.5, -400)) Pow overflows to +Inf, f/Inf is 0, and
	// 0*Inf is NaN — which int64(NaN) then narrows to 0 on arm64 but
	// math.MinInt64 on amd64, sqi's primary deployment arch, silently. The
	// loop catches the same condition before it ever produces an Inf: past
	// that point f/scale is 0 for any representable f, which is round-to-even
	// at that resolution's actual answer, computed directly rather than
	// discovered via NaN.
	scale := 1.0
	for range -ndigits {
		if scale > math.MaxFloat64/10 {
			return Int(0), nil
		}
		scale *= 10
	}
	n, err := floatToInt(math.RoundToEven(f/scale) * scale)
	if err != nil {
		return Value{}, err
	}
	return Int(n), nil
}

// roundIntBeyondScale answers round(n, -places) for a places whose scale,
// 10^places, is past int64's range — the case roundIntToDigits' accumulation
// loop bails out on.
//
// It returns the answer rather than an error, because there is one and it is
// representable. The scale exceeds MaxInt64 and so exceeds |n|, which makes the
// quotient 0; the only question is whether the remainder — all of n — reaches
// half the scale and rounds away from zero. If it does the result is ±scale,
// which is not representable and IS a genuine overflow; if it does not, the
// result is 0. roundToDigits' float branch already returns 0 for the same
// shape of question (round(3.5, -400)), and bailing out here made
// round(1234, -19) an error where round(1234.0, -19) was 0.
//
// Exactly one exponent can be ambiguous. 10^19 is the first power past
// MaxInt64, and its half — 5e18 — is representable, so it has to be compared
// against. Every larger power has a half above MaxInt64, which no n can reach,
// so those all round to 0. The bound is inclusive because a remainder exactly
// at half rounds to even, and the quotient 0 is already even.
func roundIntBeyondScale(n, places int64) (Value, error) {
	const halfOfTheFirstUnrepresentablePower = 5_000_000_000_000_000_000
	if places > 19 || (n >= -halfOfTheFirstUnrepresentablePower && n <= halfOfTheFirstUnrepresentablePower) {
		return Int(0), nil
	}
	return Value{}, errIntOverflow
}

// roundIntToDigits implements round(int, int). An int is already whole, so a
// non-negative ndigits changes nothing; a negative one rounds to that decimal
// position, half to even, without going through float64 and its precision.
func roundIntToDigits(n, ndigits int64) (Value, error) {
	if ndigits >= 0 {
		return Int(n), nil
	}
	scale := int64(1)
	for range -ndigits {
		if scale > math.MaxInt64/10 {
			return roundIntBeyondScale(n, -ndigits)
		}
		scale *= 10
	}
	q, r := n/scale, n%scale
	half := scale / 2
	switch {
	case r > half || (r == half && q%2 != 0):
		q++
	case r < -half || (r == -half && q%2 != 0):
		q--
	}
	// q*scale, not Int(q*scale) directly: the multiplication itself can leave
	// int64's range even though both the accumulation loop above and the
	// half-adjustment just before it were individually guarded — measured for
	// "round(9223372036854775807, -1)" and "round(9223372036854775806, -1)",
	// both of which silently returned the wrong, sign-flipped
	// -9223372036854775806 before this used the checked multiply.
	out, err := mulInt(q, scale)
	if err != nil {
		return Value{}, err
	}
	return Int(out), nil
}

// emptyListError is RFC 0006's wording for min() and max() over an empty list.
// It is reached two ways — the dedicated list[nulltype] row for a literal, and
// the concrete-element rows for a typed list that happens to be empty at
// runtime — and both must say the same thing.
func emptyListError(name string) error {
	return fmt.Errorf("%s() requires a non-empty list", name)
}

// extremumInt returns the smallest or largest of int values.
func extremumInt(vals []Value, wantMin bool) (Value, error) {
	if len(vals) == 0 {
		return Value{}, emptyListError(extremumName(wantMin))
	}
	best := vals[0].AsInt()
	for _, v := range vals[1:] {
		n := v.AsInt()
		if (wantMin && n < best) || (!wantMin && n > best) {
			best = n
		}
	}
	return Int(best), nil
}

// extremumInts is extremumInt for already-unboxed int64 values, used by
// min/max's range_expr rows. rangeInts (rangeexpr.go) hands back a plain
// []int64; boxing it into []Value only to immediately unbox it again inside
// extremumInt would cost a real allocation for nothing — measured at half a
// gigabyte extra on a ten-million-value range — so this operates on the slice
// rangeInts already produced instead.
func extremumInts(ints []int64, wantMin bool) (Value, error) {
	if len(ints) == 0 {
		return Value{}, emptyListError(extremumName(wantMin))
	}
	best := ints[0]
	for _, n := range ints[1:] {
		if (wantMin && n < best) || (!wantMin && n > best) {
			best = n
		}
	}
	return Int(best), nil
}

// extremumFloat is extremumInt for float values.
func extremumFloat(vals []Value, wantMin bool) (Value, error) {
	if len(vals) == 0 {
		return Value{}, emptyListError(extremumName(wantMin))
	}
	best := vals[0].AsFloat()
	for _, v := range vals[1:] {
		f := v.AsFloat()
		if (wantMin && f < best) || (!wantMin && f > best) {
			best = f
		}
	}
	return floatValue(best)
}

// extremumName names the caller for the empty-list message.
func extremumName(wantMin bool) string {
	if wantMin {
		return "min"
	}
	return "max"
}

// sumInts adds int values through section 2.1.1's checked addition, so a total
// that leaves int64 is reported rather than wrapped.
func sumInts(vals []Value) (Value, error) {
	total := int64(0)
	for _, v := range vals {
		next, err := addInt(total, v.AsInt())
		if err != nil {
			return Value{}, err
		}
		total = next
	}
	return Int(total), nil
}

// sumInt64s is sumInts for already-unboxed int64 values, for the same reason
// extremumInts exists: sum's range_expr row would otherwise box rangeInts's
// []int64 into []Value purely to hand it straight to sumInts and discard the
// boxed form.
func sumInt64s(ints []int64) (Value, error) {
	total := int64(0)
	for _, n := range ints {
		next, err := addInt(total, n)
		if err != nil {
			return Value{}, err
		}
		total = next
	}
	return Int(total), nil
}
