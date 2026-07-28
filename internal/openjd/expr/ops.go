// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

var (
	errIntOverflow  = errors.New("integer overflow")
	errDivideByZero = errors.New("division by zero")
	errModuloByZero = errors.New("modulo by zero")
	errZeroNegPower = errors.New("zero cannot be raised to a negative power")
	errInfinite     = errors.New("the result is infinite")
	errNotANumber   = errors.New("the result is not a number")
	errNegFracPower = errors.New("a negative number cannot be raised to a fractional power")
)

// opKey selects one binary operator implementation.
//
// Operator behavior is a table rather than a switch on purpose. "+" alone has
// ten signatures across the full EXPR spec — integers, floats, text, paths,
// lists, ranges and combinations — and sub-project A implements only the
// same-type ones. A switch would force sub-projects B and C to rewrite it;
// a table lets them add rows.
//
// A missing key is reported as "unsupported operand types", which is also
// precisely A's same-type-only restriction. The mechanism and the restriction
// are therefore one thing, not two that can drift out of step.
type opKey struct {
	op          Op
	left, right Kind
}

// binaryFunc implements one operator signature. The operands are guaranteed to
// have the kinds their key names, so an implementation may use the payload
// accessors without checking.
type binaryFunc func(l, r Value) (Value, error)

// binaryOps is the dispatch table. Sub-project B adds rows such as
// {OpAdd, KindPath, KindString}; sub-project C adds none, since functions are
// registered separately.
var binaryOps = map[opKey]binaryFunc{
	{OpAdd, KindInt, KindInt}:      intBinary(addInt),
	{OpSub, KindInt, KindInt}:      intBinary(subInt),
	{OpMul, KindInt, KindInt}:      intBinary(mulInt),
	{OpFloorDiv, KindInt, KindInt}: intBinary(floorDivInt),
	{OpMod, KindInt, KindInt}:      intBinary(modInt),
	{OpDiv, KindInt, KindInt}:      divInts,
	{OpPow, KindInt, KindInt}:      powInts,

	{OpAdd, KindFloat, KindFloat}:      floatBinary(func(a, b float64) float64 { return a + b }),
	{OpSub, KindFloat, KindFloat}:      floatBinary(func(a, b float64) float64 { return a - b }),
	{OpMul, KindFloat, KindFloat}:      floatBinary(func(a, b float64) float64 { return a * b }),
	{OpDiv, KindFloat, KindFloat}:      divFloats,
	{OpFloorDiv, KindFloat, KindFloat}: floorDivFloats,
	{OpMod, KindFloat, KindFloat}:      modFloats,
	{OpPow, KindFloat, KindFloat}:      powFloats,

	{OpAdd, KindString, KindString}:   concatStrings,
	{OpIn, KindString, KindString}:    containsString,
	{OpNotIn, KindString, KindString}: notContainsString,
}

// unaryKey selects one prefix operator implementation.
type unaryKey struct {
	op   Op
	kind Kind
}

type unaryFunc func(v Value) (Value, error)

var unaryOps = map[unaryKey]unaryFunc{
	{OpNeg, KindInt}: negInt,
	{OpPos, KindInt}: identity,

	{OpNeg, KindFloat}: negFloat,
	{OpPos, KindFloat}: identity,
	{OpNot, KindBool}:  notBool,
}

// applyBinary dispatches a binary operator, or reports that no signature
// matches the operand kinds. Errors carry no position; the evaluator wraps
// them with the offset of the operator that failed.
func applyBinary(op Op, l, r Value) (Value, error) {
	fn, ok := binaryOps[opKey{op, l.Kind, r.Kind}]
	if !ok {
		return Value{}, fmt.Errorf("unsupported operand types for %s: %s and %s", op, l.Kind, r.Kind)
	}
	return fn(l, r)
}

// applyUnary dispatches a prefix operator.
func applyUnary(op Op, v Value) (Value, error) {
	fn, ok := unaryOps[unaryKey{op, v.Kind}]
	if !ok {
		return Value{}, fmt.Errorf("unsupported operand type for %s: %s", op, v.Kind)
	}
	return fn(v)
}

func identity(v Value) (Value, error) { return v, nil }

// intBinary adapts an int64 operation to the table's signature.
func intBinary(f func(a, b int64) (int64, error)) binaryFunc {
	return func(l, r Value) (Value, error) {
		out, err := f(l.AsInt(), r.AsInt())
		if err != nil {
			return Value{}, err
		}
		return Int(out), nil
	}
}

// floatValue wraps a computed float, applying spec section 1.3.4: the language
// produces no negative zero, no infinity and no NaN. Every float-producing
// operation must return through here.
func floatValue(f float64) (Value, error) {
	switch {
	case math.IsNaN(f):
		return Value{}, errNotANumber
	case math.IsInf(f, 0):
		return Value{}, errInfinite
	case f == 0:
		// True for -0.0 as well; assigning the literal normalizes the sign.
		return Float(0), nil
	}
	return Float(f), nil
}

func addInt(a, b int64) (int64, error) {
	sum := a + b
	if (a > 0 && b > 0 && sum < 0) || (a < 0 && b < 0 && sum >= 0) {
		return 0, errIntOverflow
	}
	return sum, nil
}

func subInt(a, b int64) (int64, error) {
	diff := a - b
	if (b < 0 && diff < a) || (b > 0 && diff > a) {
		return 0, errIntOverflow
	}
	return diff, nil
}

func mulInt(a, b int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	// Checked before the division below, which would itself panic on
	// math.MinInt64 / -1.
	if (a == math.MinInt64 && b == -1) || (b == math.MinInt64 && a == -1) {
		return 0, errIntOverflow
	}
	product := a * b
	if product/b != a {
		return 0, errIntOverflow
	}
	return product, nil
}

func negInt(v Value) (Value, error) {
	i := v.AsInt()
	if i == math.MinInt64 {
		return Value{}, errIntOverflow
	}
	return Int(-i), nil
}

// floorDivInt implements "//" with floored division (spec section 2.1.1),
// rounding toward negative infinity. Go's "/" truncates toward zero, so a
// negative result with a non-zero remainder is one too high.
func floorDivInt(a, b int64) (int64, error) {
	if b == 0 {
		return 0, errDivideByZero
	}
	if a == math.MinInt64 && b == -1 {
		return 0, errIntOverflow
	}
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q, nil
}

// modInt implements "%" with floored semantics, so the result takes the
// divisor's sign: -7 % 3 is 2, not -1.
func modInt(a, b int64) (int64, error) {
	if b == 0 {
		return 0, errModuloByZero
	}
	if a == math.MinInt64 && b == -1 {
		return 0, nil
	}
	r := a % b
	if r != 0 && (r < 0) != (b < 0) {
		r += b
	}
	return r, nil
}

// divInts implements __truediv__(int, int) -> float: "/" is always float
// division, even when the operands divide exactly.
func divInts(l, r Value) (Value, error) {
	b := r.AsInt()
	if b == 0 {
		return Value{}, errDivideByZero
	}
	return floatValue(float64(l.AsInt()) / float64(b))
}

// powInts implements __pow__(int, int) -> int | float. The result is an int
// for a non-negative exponent and a float for a negative one, so 2 ** 3 is 8
// and 2 ** -3 is 0.125.
func powInts(l, r Value) (Value, error) {
	base, exp := l.AsInt(), r.AsInt()
	if exp < 0 {
		if base == 0 {
			return Value{}, errZeroNegPower
		}
		return floatValue(math.Pow(float64(base), float64(exp)))
	}
	out, err := powIntPositive(base, exp)
	if err != nil {
		return Value{}, err
	}
	return Int(out), nil
}

// powIntPositive raises base to a non-negative exponent by repeated squaring.
//
// The algorithm matters: a loop over the exponent would not terminate for
// "2 ** 9223372036854775807", whereas squaring overflows within about six
// iterations and reports it immediately. Bases of 0, 1 and -1 never overflow
// and terminate in at most 63 iterations.
func powIntPositive(base, exp int64) (int64, error) {
	result, factor, e := int64(1), base, exp
	for e > 0 {
		if e&1 == 1 {
			var err error
			if result, err = mulInt(result, factor); err != nil {
				return 0, err
			}
		}
		e >>= 1
		if e == 0 {
			break
		}
		var err error
		if factor, err = mulInt(factor, factor); err != nil {
			return 0, err
		}
	}
	return result, nil
}

// floatBinary adapts a total float64 operation to the table's signature. The
// result passes through floatValue, so section 1.3.4's no-infinity, no-NaN
// and no-negative-zero rules apply to every float-producing operator.
func floatBinary(f func(a, b float64) float64) binaryFunc {
	return func(l, r Value) (Value, error) {
		return floatValue(f(l.AsFloat(), r.AsFloat()))
	}
}

func divFloats(l, r Value) (Value, error) {
	b := r.AsFloat()
	if b == 0 {
		return Value{}, errDivideByZero
	}
	return floatValue(l.AsFloat() / b)
}

// floorDivFloats implements __floordiv__(float, float) -> int. The int return
// is what the spec's signature table specifies; it is not a slip.
func floorDivFloats(l, r Value) (Value, error) {
	b := r.AsFloat()
	if b == 0 {
		return Value{}, errDivideByZero
	}
	q := math.Floor(l.AsFloat() / b)
	// float64(math.MaxInt64) is exactly 2^63, one past the last representable
	// int64, so the upper bound is a >= test rather than a > test.
	if math.IsNaN(q) || q < math.MinInt64 || q >= float64(math.MaxInt64) {
		return Value{}, errIntOverflow
	}
	return Int(int64(q)), nil
}

// modFloats implements "%" on floats with floored semantics, so the result
// takes the divisor's sign: -7.0 % 3.0 is 2.0. math.Mod truncates.
func modFloats(l, r Value) (Value, error) {
	b := r.AsFloat()
	if b == 0 {
		return Value{}, errModuloByZero
	}
	rem := math.Mod(l.AsFloat(), b)
	if rem != 0 && (rem < 0) != (b < 0) {
		rem += b
	}
	return floatValue(rem)
}

func powFloats(l, r Value) (Value, error) {
	base, exp := l.AsFloat(), r.AsFloat()
	if base == 0 && exp < 0 {
		return Value{}, errZeroNegPower
	}
	if base < 0 && exp != math.Trunc(exp) {
		return Value{}, errNegFracPower
	}
	return floatValue(math.Pow(base, exp))
}

func concatStrings(l, r Value) (Value, error) {
	return String(l.AsStr() + r.AsStr()), nil
}

// containsString implements __contains__(a, b) for "b in a". The expression's
// LEFT operand is the needle and the right is the haystack, which is the
// reverse of the signature's parameter order.
func containsString(l, r Value) (Value, error) {
	return Bool(strings.Contains(r.AsStr(), l.AsStr())), nil
}

func notContainsString(l, r Value) (Value, error) {
	return Bool(!strings.Contains(r.AsStr(), l.AsStr())), nil
}

func negFloat(v Value) (Value, error) { return floatValue(-v.AsFloat()) }

func notBool(v Value) (Value, error) { return Bool(!v.AsBool()), nil }
