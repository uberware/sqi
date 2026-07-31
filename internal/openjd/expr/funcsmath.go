// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"fmt"
	"math"
	"strconv"
)

// mathFuncs is RFC 0006's math-function group. Its other members are added by
// later tasks of this sub-project.
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
			return Int(int64(math.RoundToEven(args[0].AsFloat()))), nil
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
			return Int(int64(math.Floor(args[0].AsFloat()))), nil
		}},
	},
	"ceil": {
		{Params: []Type{TInt}, Ret: TInt, Fn: func(args []Value) (Value, error) {
			return args[0], nil
		}},
		{Params: []Type{TFloat}, Ret: TInt, Fn: func(args []Value) (Value, error) {
			return Int(int64(math.Ceil(args[0].AsFloat()))), nil
		}},
	},
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
	scale := math.Pow(10, float64(-ndigits))
	return Int(int64(math.RoundToEven(f/scale) * scale)), nil
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
	return Int(q * scale), nil
}
