// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"fmt"
	"math"
	"strconv"
)

// mathFuncs is RFC 0006's math-function group: abs, min, max, sum, floor,
// ceil, round. A later sub-project never edits this table — C2, C3 and C4 add
// their own groups in their own files (funcsstr.go, funcsre.go, funcsrepr.go,
// funcspath.go) and their own entry in funcs.go's mergeFuncs call.
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
		{Params: []Type{ListOf(TInt)}, Ret: TInt, Fn: func(args []Value) (Value, error) {
			return extremumInt(args[0].AsList(), true)
		}},
		{Params: []Type{ListOf(TFloat)}, Ret: TFloat, Fn: func(args []Value) (Value, error) {
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
		{Params: []Type{TRangeExpr}, Ret: TInt, Fn: func(args []Value) (Value, error) {
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
		{Params: []Type{ListOf(TInt)}, Ret: TInt, Fn: func(args []Value) (Value, error) {
			return extremumInt(args[0].AsList(), false)
		}},
		{Params: []Type{ListOf(TFloat)}, Ret: TFloat, Fn: func(args []Value) (Value, error) {
			return extremumFloat(args[0].AsList(), false)
		}},
		{Params: []Type{ListOf(TNull)}, Ret: TNoReturn, Fn: func([]Value) (Value, error) {
			return Value{}, emptyListError("max")
		}},
		{Params: []Type{TRangeExpr}, Ret: TInt, Fn: func(args []Value) (Value, error) {
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
	"sum": {
		{Params: []Type{ListOf(TNull)}, Ret: TInt, Fn: func([]Value) (Value, error) {
			return Int(0), nil
		}},
		{Params: []Type{ListOf(TInt)}, Ret: TInt, Fn: func(args []Value) (Value, error) {
			return sumInts(args[0].AsList())
		}},
		{Params: []Type{ListOf(TFloat)}, Ret: TFloat, Fn: func(args []Value) (Value, error) {
			total := 0.0
			for _, v := range args[0].AsList() {
				total += v.AsFloat()
			}
			return floatValue(total)
		}},
		{Params: []Type{TRangeExpr}, Ret: TInt, Fn: func(args []Value) (Value, error) {
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
		scale := math.Pow(10, float64(ndigits))
		rounded := math.RoundToEven(f*scale) / scale
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
			return Value{}, errIntOverflow
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
