// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"cmp"
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

// binaryFunc is the shape of an operator implementation a Shape wraps. The
// operands are guaranteed to have been coerced to the Shape's declared
// parameter types before an implementation runs, so it may use the payload
// accessors without checking.
type binaryFunc func(l, r Value) (Value, error)

// identity is OpPos's implementation: unary "+" changes nothing.
func identity(v Value) (Value, error) { return v, nil }

// shapeBinary adapts a two-operand implementation to a Shape's argument slice.
func shapeBinary(f func(l, r Value) (Value, error)) func(args []Value) (Value, error) {
	return func(args []Value) (Value, error) { return f(args[0], args[1]) }
}

// shapeUnary adapts a one-operand implementation to a Shape's argument slice.
func shapeUnary(f func(v Value) (Value, error)) func(args []Value) (Value, error) {
	return func(args []Value) (Value, error) { return f(args[0]) }
}

// binaryShapes lists every accepted signature of each binary operator, in the
// spec's own terms (sections 2.1.1, 2.1.2, 2.1.4).
//
// A shape declares what it returns as well as what it takes, which is what lets
// an operand with no value still yield a typed result. Ordering within a list
// mostly does not affect which shape wins: matchShapes (shape.go) ranks every
// candidate by cost — an exact match cheaper than a coercing one — and the
// lowest total wins regardless of list position. The one place position still
// matters is an exact tie, which keeps the earliest shape; no operator table
// here relies on that, since no operator lists two shapes of equal cost for
// the same argument types.
//
// Sub-projects B2 and C add shapes here and to their own function registry; a
// list with no matching shape is reported as "unsupported operand types", which
// is the same single mechanism sub-project A used.
var binaryShapes = map[Op][]Shape{
	OpAdd: {
		{Params: []Type{TInt, TInt}, Ret: TInt, Fn: shapeBinary(intBinary(addInt))},
		{Params: []Type{TFloat, TFloat}, Ret: TFloat, Fn: shapeBinary(floatBinary(func(a, b float64) float64 { return a + b }))},
		{Params: []Type{TString, TString}, Ret: TString, Fn: shapeBinary(concatStrings)},
	},
	OpSub: {
		{Params: []Type{TInt, TInt}, Ret: TInt, Fn: shapeBinary(intBinary(subInt))},
		{Params: []Type{TFloat, TFloat}, Ret: TFloat, Fn: shapeBinary(floatBinary(func(a, b float64) float64 { return a - b }))},
	},
	OpMul: {
		{Params: []Type{TInt, TInt}, Ret: TInt, Fn: shapeBinary(intBinary(mulInt))},
		{Params: []Type{TFloat, TFloat}, Ret: TFloat, Fn: shapeBinary(floatBinary(func(a, b float64) float64 { return a * b }))},
	},
	// Dividing two ints yields a float — the spec's __truediv__(int, int) -> float.
	OpDiv: {
		{Params: []Type{TInt, TInt}, Ret: TFloat, Fn: shapeBinary(divInts)},
		{Params: []Type{TFloat, TFloat}, Ret: TFloat, Fn: shapeBinary(divFloats)},
	},
	// Floor-dividing two floats yields an int — __floordiv__(float, float) -> int.
	OpFloorDiv: {
		{Params: []Type{TInt, TInt}, Ret: TInt, Fn: shapeBinary(intBinary(floorDivInt))},
		{Params: []Type{TFloat, TFloat}, Ret: TInt, Fn: shapeBinary(floorDivFloats)},
	},
	OpMod: {
		{Params: []Type{TInt, TInt}, Ret: TInt, Fn: shapeBinary(intBinary(modInt))},
		{Params: []Type{TFloat, TFloat}, Ret: TFloat, Fn: shapeBinary(modFloats)},
	},
	// int ** int is int for a non-negative exponent and float for a negative one,
	// so its declared return is the union the spec writes: float | int.
	OpPow: {
		{Params: []Type{TInt, TInt}, Ret: UnionOf(TInt, TFloat), Fn: shapeBinary(powInts)},
		{Params: []Type{TFloat, TFloat}, Ret: TFloat, Fn: shapeBinary(powFloats)},
	},

	OpIn:    {{Params: []Type{TString, TString}, Ret: TBool, Fn: shapeBinary(containsString)}},
	OpNotIn: {{Params: []Type{TString, TString}, Ret: TBool, Fn: shapeBinary(notContainsString)}},

	OpLt: orderingShapes(OpLt),
	OpGt: orderingShapes(OpGt),
	OpLe: orderingShapes(OpLe),
	OpGe: orderingShapes(OpGe),
}

// orderingShapes builds the four same-type ordering signatures for one
// comparison operator. Section 2.1.4 also permits int/float and string/path
// cross-pairs; both reach one of these same-type shapes by the ordinary
// promotion argCost (shape.go) already gives every operator — int -> float
// and path -> string are both coercibleConditional's named compatible pairs —
// with no dedicated cross-type shape of their own.
func orderingShapes(op Op) []Shape {
	return []Shape{
		{Params: []Type{TInt, TInt}, Ret: TBool, Fn: shapeBinary(ordering(op, compareInts))},
		{Params: []Type{TFloat, TFloat}, Ret: TBool, Fn: shapeBinary(ordering(op, compareFloats))},
		{Params: []Type{TString, TString}, Ret: TBool, Fn: shapeBinary(ordering(op, compareStrings))},
		{Params: []Type{TBool, TBool}, Ret: TBool, Fn: shapeBinary(ordering(op, compareBools))},
	}
}

// unaryShapes lists every accepted signature of each prefix operator.
var unaryShapes = map[Op][]Shape{
	OpNeg: {
		{Params: []Type{TInt}, Ret: TInt, Fn: shapeUnary(negInt)},
		{Params: []Type{TFloat}, Ret: TFloat, Fn: shapeUnary(negFloat)},
	},
	OpPos: {
		{Params: []Type{TInt}, Ret: TInt, Fn: shapeUnary(identity)},
		{Params: []Type{TFloat}, Ret: TFloat, Fn: shapeUnary(identity)},
	},
	OpNot: {
		{Params: []Type{TBool}, Ret: TBool, Fn: shapeUnary(notBool)},
	},
}

// applyBinary dispatches a binary operator, or reports that no signature accepts
// the operand types. Errors carry no position; the evaluator wraps them with the
// offset of the operator that failed.
func applyBinary(op Op, l, r Value) (Value, error) {
	switch op {
	case OpEq, OpNe:
		// Equality is total across types (section 1.2.5), so it is never
		// unsupported — but with a placeholder it has no concrete answer
		// either. A bare false would be wrong: the values may be equal at
		// runtime.
		if l.IsUnresolved() || r.IsUnresolved() {
			return Unresolved(TBool), nil
		}
		if op == OpEq {
			return Bool(valuesEqual(l, r)), nil
		}
		return Bool(!valuesEqual(l, r)), nil
	}

	s, b, ok := matchShapes(binaryShapes[op], []Type{l.Type, r.Type})
	if !ok {
		return Value{}, fmt.Errorf("unsupported operand types for %s: %s and %s", op, l.Type, r.Type)
	}
	if l.IsUnresolved() || r.IsUnresolved() {
		return unresolvedResult(s, b), nil
	}
	return callShape(s, b, []Value{l, r})
}

// applyUnary dispatches a prefix operator.
func applyUnary(op Op, v Value) (Value, error) {
	s, b, ok := matchShapes(unaryShapes[op], []Type{v.Type})
	if !ok {
		return Value{}, fmt.Errorf("unsupported operand type for %s: %s", op, v.Type)
	}
	if v.IsUnresolved() {
		return unresolvedResult(s, b), nil
	}
	return callShape(s, b, []Value{v})
}

// unresolvedResult is the whole reason a Shape declares a return type.
//
// When an operand has no value there is nothing to execute, so the result is
// read off the matched shape instead: substitute the bound type variables into
// the declared Ret and wrap it as a placeholder. Note that the shape was still
// SELECTED by the operand types, so a type error is still caught here — this
// path makes an expression checkable, not unconditionally valid.
func unresolvedResult(s Shape, b bindings) Value {
	return Unresolved(substitute(s.Ret, b))
}

// callShape coerces the arguments to the shape's declared parameter types and
// runs it. The shape may have been selected at a widening cost rather than an
// exact one, so an argument can still need converting — that is where section
// 2.1.1's int-to-float promotion actually happens.
func callShape(s Shape, b bindings, args []Value) (Value, error) {
	for i := range args {
		want := substitute(s.Params[i], b)
		if args[i].Type.Equal(want) {
			continue
		}
		converted, err := coerce(args[i], want)
		if err != nil {
			return Value{}, err
		}
		args[i] = converted
	}
	return s.Fn(args)
}

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

// ordering turns a three-way comparison into the binaryFunc for one ordering
// operator. Each ordering shape it backs is declared same-type (int/int,
// float/float, string/string, bool/bool); section 2.1.4's compatible
// cross-pairs — int/float and string/path — reach a same-type shape by
// promotion during shape matching (shape.go's argCost), not by an extra row
// here. A cross-type pair that is not one of those two compatible pairs still
// has no shape to promote into, so it is correctly reported as unsupported.
func ordering(op Op, compare func(l, r Value) int) binaryFunc {
	return func(l, r Value) (Value, error) {
		c := compare(l, r)
		switch op {
		case OpLt:
			return Bool(c < 0), nil
		case OpGt:
			return Bool(c > 0), nil
		case OpLe:
			return Bool(c <= 0), nil
		case OpGe:
			return Bool(c >= 0), nil
		}
		return Value{}, fmt.Errorf("%s is not an ordering operator", op)
	}
}

func compareInts(l, r Value) int { return cmp.Compare(l.AsInt(), r.AsInt()) }

func compareFloats(l, r Value) int { return cmp.Compare(l.AsFloat(), r.AsFloat()) }

func compareStrings(l, r Value) int { return strings.Compare(l.AsStr(), r.AsStr()) }

// compareBools orders False before True (spec section 2.1.4).
func compareBools(l, r Value) int { return cmp.Compare(boolRank(l.AsBool()), boolRank(r.AsBool())) }

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// valuesEqual implements cross-type equality (spec section 1.2.5):
//
//   - int vs float compare numerically, so 5 == 5.0
//   - string vs path: the path converts to string for the comparison
//   - bool vs any non-bool is always unequal, so true == 1 is false
//   - string vs a number is always unequal, so "5" == 5 is false
//   - null equals only null
//   - every other cross-type pair is unequal
//
// The list vs range_expr rule is NOT implemented: expanding a range_expr for
// comparison needs list values, which is sub-project B2's, same as every
// other list operation.
//
// The null and bool cases are split out from numericOrStringEqual below
// purely to keep this function's cyclomatic complexity under the repo's
// lint threshold; the combined behavior is unchanged.
func valuesEqual(l, r Value) bool {
	switch {
	case l.Type.Code == CodeNull || r.Type.Code == CodeNull:
		return l.Type.Code == r.Type.Code
	case l.Type.Code == CodeBool || r.Type.Code == CodeBool:
		return l.Type.Code == r.Type.Code && l.AsBool() == r.AsBool()
	}
	return numericOrStringEqual(l, r)
}

// numericOrStringEqual handles the int/float/string cases of valuesEqual.
// Callers must have already excluded null and bool operands.
func numericOrStringEqual(l, r Value) bool {
	switch {
	case l.Type.Code == CodeInt && r.Type.Code == CodeInt:
		return l.AsInt() == r.AsInt()
	case l.Type.Code == CodeFloat && r.Type.Code == CodeFloat:
		return l.AsFloat() == r.AsFloat()
	case l.Type.Code == CodeInt && r.Type.Code == CodeFloat:
		return intEqualsFloat(l.AsInt(), r.AsFloat())
	case l.Type.Code == CodeFloat && r.Type.Code == CodeInt:
		return intEqualsFloat(r.AsInt(), l.AsFloat())
	case l.Type.Code == CodeString && r.Type.Code == CodeString:
		return l.AsStr() == r.AsStr()
	case l.Type.Code == CodePath || r.Type.Code == CodePath:
		return pathEqual(l, r)
	}
	// PARKED, for whichever sub-project implements range_expr expansion: two
	// range_exprs land here and compare unequal, including to themselves.
	// Comparing their string payloads is NOT obviously right — section 1.2.5
	// expands a range_expr when it meets a list, which would make '1-3' equal
	// '1,2,3', so a payload comparison would contradict the expanded one. Decide
	// it alongside expansion, then add the row and CodeRangeExpr to
	// sampleValues' scalarCodes in ops_internal_test.go. Unreachable through
	// Eval today: nothing produces two range_expr operands.
	return false
}

// pathEqual handles the equality rows where at least one operand is a path.
// Split out of numericOrStringEqual for the same reason null and bool are split
// out of valuesEqual: to keep each function under the repo's complexity
// threshold. Callers must have already excluded null and bool operands.
//
// A path compares equal to a string with the same text, which is section
// 1.2.5's rule that the path converts to string for the comparison, and equal
// to a path with the same text, which is not that section's business — 1.2.5
// covers CROSS-type comparison — but is required for a path to equal itself.
// Anything else is a cross-type pair 1.2.5 makes unequal.
//
// The path's payload is read directly (v.s) rather than through AsStr, which
// panics on anything but CodeString.
func pathEqual(l, r Value) bool {
	if l.Type.Code != CodeString && l.Type.Code != CodePath {
		return false
	}
	if r.Type.Code != CodeString && r.Type.Code != CodePath {
		return false
	}
	return l.s == r.s
}

// intEqualsFloat compares an int64 to a float64 exactly. Converting the int to
// float64 and comparing would lose precision above 2^53, making distinct
// integers look equal.
func intEqualsFloat(i int64, f float64) bool {
	if f != math.Trunc(f) {
		return false
	}
	// float64(math.MaxInt64) is exactly 2^63, one past the last representable
	// int64, so the upper bound is a >= test.
	if f < math.MinInt64 || f >= float64(math.MaxInt64) {
		return false
	}
	return int64(f) == i
}
