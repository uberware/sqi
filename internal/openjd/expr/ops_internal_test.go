// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestApplyBinary_IntArithmetic(t *testing.T) {
	tests := []struct {
		name string
		op   Op
		l, r int64
		want Value
	}{
		{"add", OpAdd, 2, 3, Int(5)},
		{"subtract", OpSub, 2, 3, Int(-1)},
		{"multiply", OpMul, 4, 3, Int(12)},
		{"divide always yields a float", OpDiv, 10, 4, Float(2.5)},
		{"divide with no remainder still yields a float", OpDiv, 10, 5, Float(2)},
		{"floor divide", OpFloorDiv, 10, 3, Int(3)},
		{"floor divide rounds toward negative infinity", OpFloorDiv, -7, 3, Int(-3)},
		{"floor divide with a negative divisor", OpFloorDiv, 7, -3, Int(-3)},
		{"modulo", OpMod, 10, 3, Int(1)},
		{"modulo takes the divisor's sign", OpMod, -7, 3, Int(2)},
		{"modulo with a negative divisor", OpMod, 7, -3, Int(-2)},
		{"power", OpPow, 2, 3, Int(8)},
		{"power of zero", OpPow, 5, 0, Int(1)},
		{"negative exponent yields a float", OpPow, 2, -3, Float(0.125)},
		{"one to a huge power terminates", OpPow, 1, math.MaxInt64, Int(1)},
		{"modulo by negative one at the int64 minimum", OpMod, math.MinInt64, -1, Int(0)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := applyBinary(tt.op, Int(tt.l), Int(tt.r))
			if err != nil {
				t.Fatalf("applyBinary(%v, %d, %d): %v", tt.op, tt.l, tt.r, err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("= %v (%s); want %v (%s)", got, got.Type, tt.want, tt.want.Type)
			}
		})
	}
}

func TestApplyBinary_IntErrors(t *testing.T) {
	tests := []struct {
		name    string
		op      Op
		l, r    int64
		wantMsg string
	}{
		{"divide by zero", OpDiv, 1, 0, "division by zero"},
		{"floor divide by zero", OpFloorDiv, 1, 0, "division by zero"},
		{"modulo by zero", OpMod, 1, 0, "modulo by zero"},
		{"add overflow", OpAdd, math.MaxInt64, 1, "integer overflow"},
		{"add overflow negative", OpAdd, math.MinInt64, -1, "integer overflow"},
		{"subtract overflow", OpSub, math.MinInt64, 1, "integer overflow"},
		{"multiply overflow", OpMul, math.MaxInt64, 2, "integer overflow"},
		{"multiply overflow negative", OpMul, math.MinInt64, -1, "integer overflow"},
		{"power overflow", OpPow, 2, 64, "integer overflow"},
		{"power overflow with a huge exponent", OpPow, 2, math.MaxInt64, "integer overflow"},
		{"zero to a negative power", OpPow, 0, -1, "negative power"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := applyBinary(tt.op, Int(tt.l), Int(tt.r))
			if err == nil {
				t.Fatalf("applyBinary(%v, %d, %d) = nil error; want an error", tt.op, tt.l, tt.r)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %q; want it to contain %q", err.Error(), tt.wantMsg)
			}
		})
	}
}

func TestApplyUnary_Int(t *testing.T) {
	got, err := applyUnary(OpNeg, Int(5))
	if err != nil || !got.Equal(Int(-5)) {
		t.Errorf("applyUnary(OpNeg, 5) = %v, %v; want -5, nil", got, err)
	}
	got, err = applyUnary(OpPos, Int(5))
	if err != nil || !got.Equal(Int(5)) {
		t.Errorf("applyUnary(OpPos, 5) = %v, %v; want 5, nil", got, err)
	}
	if _, err := applyUnary(OpNeg, Int(math.MinInt64)); err == nil {
		t.Error("negating math.MinInt64 = nil error; want integer overflow")
	}
}

func TestApplyBinary_UnsupportedOperands(t *testing.T) {
	// Section 2.1.1: "when mixing int and float operands, the int is promoted
	// to float and the float overload is used." Sub-project A reported this as
	// unsupported, same-type-only dispatch having no int/float coercion; B1's
	// coercing shape match now supplies exactly that promotion.
	got, err := applyBinary(OpAdd, Int(1), Float(2.5))
	if err != nil {
		t.Fatalf("applyBinary(+, 1, 2.5): %v", err)
	}
	if !got.Equal(Float(3.5)) {
		t.Errorf("1 + 2.5 = %s; want 3.5", got)
	}
}

func TestApplyUnary_UnsupportedOperand(t *testing.T) {
	_, err := applyUnary(OpNeg, String("x"))
	if err == nil {
		t.Fatal("-'x' = nil error; want unsupported operand type")
	}
	if got := err.Error(); !strings.Contains(got, "unsupported operand type for -: string") {
		t.Errorf("error = %q; want it to name the operator and the kind", got)
	}
}

func TestFloatValue_Section134(t *testing.T) {
	tests := []struct {
		name    string
		in      float64
		want    Value
		wantErr string
	}{
		{name: "ordinary", in: 1.5, want: Float(1.5)},
		{name: "negative zero is normalized", in: math.Copysign(0, -1), want: Float(0)},
		{name: "infinity is an error", in: math.Inf(1), wantErr: "infinite"},
		{name: "negative infinity is an error", in: math.Inf(-1), wantErr: "infinite"},
		{name: "nan is an error", in: math.NaN(), wantErr: "not a number"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := floatValue(tt.in)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("floatValue(%v) error = %v; want it to contain %q", tt.in, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("floatValue(%v): %v", tt.in, err)
			}
			if !got.Equal(tt.want) || math.Signbit(got.AsFloat()) {
				t.Errorf("floatValue(%v) = %v; want %v with no sign bit", tt.in, got, tt.want)
			}
		})
	}
}

func TestApplyBinary_FloatArithmetic(t *testing.T) {
	tests := []struct {
		name string
		op   Op
		l, r float64
		want Value
	}{
		{"add", OpAdd, 1.5, 2.25, Float(3.75)},
		{"subtract", OpSub, 1.5, 2.25, Float(-0.75)},
		{"multiply", OpMul, 1.5, 2.0, Float(3)},
		{"divide", OpDiv, 7.5, 2.5, Float(3)},
		{"floor divide yields an int", OpFloorDiv, 7.5, 2.5, Int(3)},
		{"floor divide rounds toward negative infinity", OpFloorDiv, -7.0, 3.0, Int(-3)},
		{"modulo takes the divisor's sign", OpMod, -7.0, 3.0, Float(2)},
		{"power", OpPow, 2.0, 3.0, Float(8)},
		{"fractional power of a positive base", OpPow, 4.0, 0.5, Float(2)},
		// The expected value is a literal, not Float(0.1 + 0.2): Go folds that
		// constant expression exactly at compile time and would give 0.3,
		// whereas the runtime float64 addition under test gives this.
		{"floating point is not exact", OpAdd, 0.1, 0.2, Float(0.30000000000000004)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := applyBinary(tt.op, Float(tt.l), Float(tt.r))
			if err != nil {
				t.Fatalf("applyBinary(%v, %v, %v): %v", tt.op, tt.l, tt.r, err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("= %v (%s); want %v (%s)", got, got.Type, tt.want, tt.want.Type)
			}
		})
	}
}

func TestApplyBinary_FloatErrors(t *testing.T) {
	tests := []struct {
		name    string
		op      Op
		l, r    float64
		wantMsg string
	}{
		{"divide by zero", OpDiv, 1, 0, "division by zero"},
		{"floor divide by zero", OpFloorDiv, 1, 0, "division by zero"},
		{"modulo by zero", OpMod, 1, 0, "modulo by zero"},
		{"zero to a negative power", OpPow, 0, -1, "negative power"},
		{"negative base to a fractional power", OpPow, -2, 0.5, "fractional power"},
		{"overflow to infinity", OpMul, 1e300, 1e300, "infinite"},
		{"floor divide whose true quotient exceeds int64 range", OpFloorDiv, 1e300, 1.0, "overflow"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := applyBinary(tt.op, Float(tt.l), Float(tt.r)); err == nil {
				t.Fatalf("applyBinary(%v, %v, %v) = nil error; want %q", tt.op, tt.l, tt.r, tt.wantMsg)
			} else if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %q; want it to contain %q", err.Error(), tt.wantMsg)
			}
		})
	}
}

func TestApplyBinary_StringOperators(t *testing.T) {
	tests := []struct {
		name string
		op   Op
		l, r string
		want Value
	}{
		{"concatenate", OpAdd, "a", "b", String("ab")},
		{"contains", OpIn, "ell", "hello", Bool(true)},
		{"does not contain", OpIn, "xyz", "hello", Bool(false)},
		{"not contains", OpNotIn, "xyz", "hello", Bool(true)},
		{"not contains when present", OpNotIn, "ell", "hello", Bool(false)},
		{"empty substring is always contained", OpIn, "", "hello", Bool(true)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := applyBinary(tt.op, String(tt.l), String(tt.r))
			if err != nil {
				t.Fatalf("applyBinary: %v", err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("= %v; want %v", got, tt.want)
			}
		})
	}
}

func TestApplyBinary_StringRepetitionNowBounded(t *testing.T) {
	// String repetition was deferred until limits.go's size bound existed to
	// cap an unbounded repeat count; this task implements it now that the
	// bound exists. Confirm both that a modest repeat succeeds and that it is
	// not exempt from the bound.
	got, err := applyBinary(OpMul, String("x"), Int(3))
	if err != nil {
		t.Fatalf("applyBinary: %v", err)
	}
	if want := "xxx"; got.AsStr() != want {
		t.Errorf("= %q; want %q", got.AsStr(), want)
	}
	if _, err := applyBinary(OpMul, String("x"), Int(100_000_000)); err == nil {
		t.Error("'x' * 100000000 succeeded; want the size bound to reject it")
	}
}

func TestApplyUnary_FloatAndNot(t *testing.T) {
	tests := []struct {
		name string
		op   Op
		in   Value
		want Value
	}{
		{"negate a float", OpNeg, Float(1.5), Float(-1.5)},
		{"negating zero does not produce negative zero", OpNeg, Float(0), Float(0)},
		{"unary plus on a float", OpPos, Float(1.5), Float(1.5)},
		{"not true", OpNot, Bool(true), Bool(false)},
		{"not false", OpNot, Bool(false), Bool(true)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := applyUnary(tt.op, tt.in)
			if err != nil {
				t.Fatalf("applyUnary: %v", err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("= %v; want %v", got, tt.want)
			}
		})
	}
}

func TestApplyUnary_NotRequiresBool(t *testing.T) {
	// Section 2.1.6: "not" remains strictly boolean even though "and"/"or"
	// accept any operand.
	for _, v := range []Value{Int(1), String(""), Null(), Float(0)} {
		if _, err := applyUnary(OpNot, v); err == nil {
			t.Errorf("not %v succeeded; want unsupported operand type", v.Type)
		}
	}
}

func TestApplyBinary_Ordering(t *testing.T) {
	tests := []struct {
		name string
		op   Op
		l, r Value
		want bool
	}{
		{"int less than", OpLt, Int(1), Int(2), true},
		{"int not less than", OpLt, Int(2), Int(1), false},
		{"int greater than", OpGt, Int(2), Int(1), true},
		{"int less or equal at the boundary", OpLe, Int(2), Int(2), true},
		{"int greater or equal at the boundary", OpGe, Int(2), Int(2), true},
		{"float less than", OpLt, Float(1.5), Float(2.5), true},
		{"string orders lexicographically", OpLt, String("abc"), String("abd"), true},
		{"string prefix orders first", OpLt, String("ab"), String("abc"), true},
		{"false is less than true", OpLt, Bool(false), Bool(true), true},
		{"true is not less than false", OpLt, Bool(true), Bool(false), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := applyBinary(tt.op, tt.l, tt.r)
			if err != nil {
				t.Fatalf("applyBinary: %v", err)
			}
			if !got.Equal(Bool(tt.want)) {
				t.Errorf("= %v; want %v", got, Bool(tt.want))
			}
		})
	}
}

func TestApplyBinary_OrderingIsSameTypeOnly(t *testing.T) {
	// Section 2.1.4: "Ordering operators ... T1 and T2 may differ for
	// compatible pairs (int/float and string/path); comparing other cross-type
	// pairs is an error." Cost-ranked shape matching supplies BOTH named pairs
	// from the plain same-type shapes in orderingShapes, with no dedicated
	// cross-type shape needed: int -> float and path -> string are both
	// coercibleConditional's named compatible pairs, so argCost admits them at
	// a widening cost into the (float, float) and (string, string) shapes
	// respectively. Every other cross-type pair stays an error.
	promoted := []struct{ l, r Value }{
		{Int(1), Float(2.5)},
		{Float(1.5), Int(2)},
		{Value{Type: TPath, s: "abc"}, String("z")},
		{String("z"), Value{Type: TPath, s: "abc"}},
	}
	for _, tt := range promoted {
		if _, err := applyBinary(OpLt, tt.l, tt.r); err != nil {
			t.Errorf("%s < %s: %v; want the named compatible pair to be permitted", tt.l.Type, tt.r.Type, err)
		}
	}

	rejected := []struct{ l, r Value }{
		{String("a"), Int(1)},
		{Bool(true), Int(1)},
		{Null(), Null()},
	}
	for _, tt := range rejected {
		if _, err := applyBinary(OpLt, tt.l, tt.r); err == nil {
			t.Errorf("%s < %s succeeded; want unsupported operand types", tt.l.Type, tt.r.Type)
		}
	}
}

func TestValuesEqual_Section125(t *testing.T) {
	tests := []struct {
		name string
		l, r Value
		want bool
	}{
		{"equal ints", Int(5), Int(5), true},
		{"unequal ints", Int(5), Int(6), false},
		{"equal floats", Float(1.5), Float(1.5), true},
		{"int equals an exactly equal float", Int(5), Float(5), true},
		{"float equals an exactly equal int", Float(5), Int(5), true},
		{"int does not equal a fractional float", Int(5), Float(5.5), false},
		{
			"large int is not confused by float precision",
			Int(9007199254740993), Float(9007199254740992), false,
		},
		{"equal strings", String("a"), String("a"), true},
		{"string never equals a number", String("5"), Int(5), false},
		{"number never equals a string", Float(5), String("5"), false},
		{"bool never equals a number", Bool(true), Int(1), false},
		{"number never equals a bool", Int(1), Bool(true), false},
		{"equal bools", Bool(true), Bool(true), true},
		{"unequal bools", Bool(true), Bool(false), false},
		{"null equals null", Null(), Null(), true},
		{"null never equals a value", Null(), Int(0), false},
		{"a value never equals null", String(""), Null(), false},
		// string vs path (section 1.2.5): the path converts to string for the
		// comparison, in both operand orders.
		{"string equals a path with the same text", String("/a/b"), Value{Type: TPath, s: "/a/b"}, true},
		{"path equals a string with the same text", Value{Type: TPath, s: "/a/b"}, String("/a/b"), true},
		{"string does not equal a differently-spelled path", String("/a/b"), Value{Type: TPath, s: "/a/c"}, false},
		// path still compares unequal to a number: no cross-type rule reaches
		// across path and int/float, only string and path.
		{"path never equals a number", Value{Type: TPath, s: "5"}, Int(5), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := valuesEqual(tt.l, tt.r); got != tt.want {
				t.Errorf("valuesEqual(%v, %v) = %v; want %v", tt.l, tt.r, got, tt.want)
			}
			// != must always be the exact negation.
			ne, err := applyBinary(OpNe, tt.l, tt.r)
			if err != nil {
				t.Fatalf("applyBinary(OpNe): %v", err)
			}
			if !ne.Equal(Bool(!tt.want)) {
				t.Errorf("!= gave %v; want %v", ne, Bool(!tt.want))
			}
		})
	}
}

func TestApplyBinary_EqualityIsNeverUnsupported(t *testing.T) {
	// Unlike ordering, == and != are defined for every pair of types, so they
	// must never report "unsupported operand types". The pairwise check is
	// driven off sampleValues below, which enumerates the package's scalar
	// Codes directly, so a future scalar Code added there cannot silently
	// drop out of this loop.
	all := sampleValues(t)
	for _, l := range all {
		for _, r := range all {
			if _, err := applyBinary(OpEq, l, r); err != nil {
				t.Errorf("%s == %s returned %v; equality is total", l.Type, r.Type, err)
			}
		}
	}
}

// TestApplyBinary_EqualitySelfEquality asserts that a value of every scalar
// Code equals itself. A missing row in numericOrStringEqual (or valuesEqual)
// falls through to "return false" instead of an "unsupported operand types"
// error, so a future scalar Code added without extending that logic would
// otherwise compare unequal to itself with no loud signal. Driving the loop
// from sampleValues, rather than a hardcoded list, means that case fails this
// test instead.
func TestApplyBinary_EqualitySelfEquality(t *testing.T) {
	for _, v := range sampleValues(t) {
		out, err := applyBinary(OpEq, v, v)
		if err != nil {
			t.Errorf("%s == %s returned error %v; equality is total", v.Type, v.Type, err)
			continue
		}
		if !out.AsBool() {
			t.Errorf("a %s value did not equal itself", v.Type)
		}
	}
}

// sampleValues returns one representative Value per scalar Code, so tests
// driven from it exercise every scalar type Value can carry.
func sampleValues(t *testing.T) []Value {
	t.Helper()
	// CodeRangeExpr is now present: range_expr expansion exists (rangeInts,
	// rangeexpr.go) and listOrRangeEqual (ops.go) decides range_expr vs
	// range_expr equality by comparing text first, then falling back to
	// expanding both sides — which is what makes this sample equal itself.
	scalarCodes := []Code{CodeNull, CodeBool, CodeInt, CodeFloat, CodeString, CodePath, CodeRangeExpr}
	samples := map[Code]Value{
		CodeNull:   Null(),
		CodeBool:   Bool(true),
		CodeInt:    Int(1),
		CodeFloat:  Float(1.5),
		CodeString: String("x"),
		// The package exports no path constructor — section 1.2.3's string->path
		// coercion is the only route to one, and is how a real path value is
		// built at the evaluation boundary.
		CodePath:      mustCoerce(t, String("/a/b"), TPath),
		CodeRangeExpr: mustRangeExpr(t, "1-3"),
	}
	values := make([]Value, 0, len(scalarCodes))
	for _, c := range scalarCodes {
		v, ok := samples[c]
		if !ok {
			// A new scalar Code was added to scalarCodes without a paired sample
			// here. Fail loudly rather than silently leaving it out of the
			// self-equality check this test exists to provide.
			t.Fatalf("no sample Value registered for Code %s (%d); add one to samples in sampleValues", c, c)
		}
		values = append(values, v)
	}
	return values
}

// mustCoerce builds a value of a type with no exported constructor by running
// the coercion that produces it, failing the test if that coercion is ever
// removed.
func mustCoerce(t *testing.T, v Value, target Type) Value {
	t.Helper()
	out, err := coerce(v, target)
	if err != nil {
		t.Fatalf("coerce(%v, %s) failed: %v", v, target, err)
	}
	return out
}

// mustRangeExpr builds a range_expr sample value, failing the test if the
// text is ever rejected.
func mustRangeExpr(t *testing.T, text string) Value {
	t.Helper()
	v, err := RangeExpr(text)
	if err != nil {
		t.Fatalf("RangeExpr(%q) failed: %v", text, err)
	}
	return v
}

func TestBinaryShapes_DeclaredReturnTypes(t *testing.T) {
	// The declared Ret is what makes type checking possible, so it is asserted
	// directly rather than inferred from a computed result. Three of these are
	// counterintuitive and all three are the spec's own signatures (section
	// 2.1.1): int / int is a float, float // float is an int, and int ** int is
	// a union because the exponent's sign decides.
	tests := []struct {
		name  string
		op    Op
		left  Type
		right Type
		want  string
	}{
		{"int plus int", OpAdd, TInt, TInt, "int"},
		{"float plus float", OpAdd, TFloat, TFloat, "float"},
		{"string plus string", OpAdd, TString, TString, "string"},
		{"int divided by int is a float", OpDiv, TInt, TInt, "float"},
		{"float divided by float", OpDiv, TFloat, TFloat, "float"},
		{"int floor divided by int", OpFloorDiv, TInt, TInt, "int"},
		{"float floor divided by float is an int", OpFloorDiv, TFloat, TFloat, "int"},
		{"int modulo int", OpMod, TInt, TInt, "int"},
		{"float modulo float", OpMod, TFloat, TFloat, "float"},
		{"int to an int power is a union", OpPow, TInt, TInt, "float | int"},
		{"float to a float power", OpPow, TFloat, TFloat, "float"},
		{"int less than int", OpLt, TInt, TInt, "bool"},
		{"string in string", OpIn, TString, TString, "bool"},
		{"string not in string", OpNotIn, TString, TString, "bool"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, b, ok := matchShapes(binaryShapes[tt.op], []Type{tt.left, tt.right})
			if !ok {
				t.Fatalf("no shape for %s with %s and %s", tt.op, tt.left, tt.right)
			}
			if got := substitute(s.Ret, b).String(); got != tt.want {
				t.Errorf("Ret = %q; want %q", got, tt.want)
			}
		})
	}
}

func TestUnaryShapes_DeclaredReturnTypes(t *testing.T) {
	tests := []struct {
		name string
		op   Op
		arg  Type
		want string
	}{
		{"negate an int", OpNeg, TInt, "int"},
		{"negate a float", OpNeg, TFloat, "float"},
		{"positive int", OpPos, TInt, "int"},
		{"positive float", OpPos, TFloat, "float"},
		{"not a bool", OpNot, TBool, "bool"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, b, ok := matchShapes(unaryShapes[tt.op], []Type{tt.arg})
			if !ok {
				t.Fatalf("no shape for %s with %s", tt.op, tt.arg)
			}
			if got := substitute(s.Ret, b).String(); got != tt.want {
				t.Errorf("Ret = %q; want %q", got, tt.want)
			}
		})
	}
}

func TestApplyBinary_PromotesAMixedNumericPair(t *testing.T) {
	// Section 2.1.1: mixing int and float promotes the int and uses the float
	// overload. Sub-project A reported this as unsupported; cost-ranked shape
	// matching now handles it, and this is the single most visible behavior
	// change in B1. Each operator is exercised in both operand orders.
	tests := []struct {
		name string
		op   Op
		l, r Value
		want Value
	}{
		{"int plus float", OpAdd, Int(1), Float(2.5), Float(3.5)},
		{"float plus int", OpAdd, Float(2.5), Int(1), Float(3.5)},
		{"int minus float", OpSub, Int(1), Float(0.5), Float(0.5)},
		{"float minus int", OpSub, Float(2.5), Int(1), Float(1.5)},
		{"int times float", OpMul, Int(2), Float(1.5), Float(3)},
		{"float times int", OpMul, Float(1.5), Int(2), Float(3)},
		{"int divided by float", OpDiv, Int(3), Float(2), Float(1.5)},
		{"float divided by int", OpDiv, Float(3), Int(2), Float(1.5)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := applyBinary(tt.op, tt.l, tt.r)
			if err != nil {
				t.Fatalf("applyBinary: %v", err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("= %s; want %s", got, tt.want)
			}
		})
	}
}

func TestApplyBinary_MixedComparison(t *testing.T) {
	// Section 2.1.4 permits int/float ordering, which cost-ranked shape
	// matching supplies by promoting the int and choosing the float shape.
	got, err := applyBinary(OpLt, Int(1), Float(2.5))
	if err != nil {
		t.Fatalf("applyBinary: %v", err)
	}
	if !got.Equal(Bool(true)) {
		t.Errorf("1 < 2.5 = %s; want true", got)
	}
}

func TestApplyBinary_StillRejectsWhatHasNoShape(t *testing.T) {
	tests := []struct {
		name string
		op   Op
		l, r Value
	}{
		{"string plus int", OpAdd, String("a"), Int(1)},
		{"bool plus bool", OpAdd, Bool(true), Bool(true)},
		{"null plus int", OpAdd, Null(), Int(1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := applyBinary(tt.op, tt.l, tt.r)
			if err == nil {
				t.Fatalf("applyBinary(%s, %s, %s) = nil error; want an error", tt.op, tt.l, tt.r)
			}
			if !strings.Contains(err.Error(), "unsupported operand types") {
				t.Errorf("error = %q; want it to report unsupported operand types", err.Error())
			}
		})
	}
}

func TestApplyBinary_PropagatesAPlaceholder(t *testing.T) {
	tests := []struct {
		name string
		op   Op
		l, r Value
		want string // the resulting type, rendered
	}{
		{"placeholder plus int", OpAdd, Unresolved(TInt), Int(1), "unresolved[int]"},
		{"int plus placeholder", OpAdd, Int(1), Unresolved(TInt), "unresolved[int]"},
		{"both placeholders", OpAdd, Unresolved(TInt), Unresolved(TInt), "unresolved[int]"},
		{"placeholder floats", OpAdd, Unresolved(TFloat), Unresolved(TFloat), "unresolved[float]"},
		{"placeholder strings concatenate", OpAdd, Unresolved(TString), String("x"), "unresolved[string]"},
		// The declared return type is what answers this, so int / int is a float
		// even though nothing was divided.
		{"placeholder division is a float", OpDiv, Unresolved(TInt), Int(2), "unresolved[float]"},
		{"placeholder floor division of floats is an int", OpFloorDiv, Unresolved(TFloat), Float(2), "unresolved[int]"},
		// int ** int declares float | int, so the placeholder carries the union.
		{"placeholder power is a union", OpPow, Unresolved(TInt), Int(2), "unresolved[float | int]"},
		{"placeholder comparison is a bool", OpLt, Unresolved(TInt), Int(1), "unresolved[bool]"},
		// Section 2.1.1's promotion applies to a placeholder too.
		{"placeholder int promoted against a float", OpAdd, Unresolved(TInt), Float(1.5), "unresolved[float]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := applyBinary(tt.op, tt.l, tt.r)
			if err != nil {
				t.Fatalf("applyBinary: %v", err)
			}
			if !got.IsUnresolved() {
				t.Fatalf("result is %s; want a placeholder", got.Type)
			}
			if got.Type.String() != tt.want {
				t.Errorf("type = %q; want %q", got.Type.String(), tt.want)
			}
		})
	}
}

func TestApplyBinary_PlaceholderStillTypeChecks(t *testing.T) {
	// A placeholder does not make everything succeed: its constraint still has
	// to select a shape. This is what catches "Param.Name + 5" at check time,
	// before any parameter value exists.
	tests := []struct {
		name string
		l, r Value
	}{
		{"placeholder string plus int", Unresolved(TString), Int(5)},
		{"int plus placeholder string", Int(5), Unresolved(TString)},
		{"placeholder bool plus placeholder bool", Unresolved(TBool), Unresolved(TBool)},
		{"placeholder list plus int", Unresolved(ListOf(TInt)), Int(1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := applyBinary(OpAdd, tt.l, tt.r); err == nil {
				t.Error("applyBinary = nil error; want unsupported operand types")
			}
		})
	}
}

func TestApplyUnary_PropagatesAPlaceholder(t *testing.T) {
	tests := []struct {
		name string
		op   Op
		v    Value
		want string
	}{
		{"negate a placeholder int", OpNeg, Unresolved(TInt), "unresolved[int]"},
		{"negate a placeholder float", OpNeg, Unresolved(TFloat), "unresolved[float]"},
		{"not a placeholder bool", OpNot, Unresolved(TBool), "unresolved[bool]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := applyUnary(tt.op, tt.v)
			if err != nil {
				t.Fatalf("applyUnary: %v", err)
			}
			if got.Type.String() != tt.want {
				t.Errorf("type = %q; want %q", got.Type.String(), tt.want)
			}
		})
	}
	if _, err := applyUnary(OpNot, Unresolved(TInt)); err == nil {
		t.Error("not on a placeholder int = nil error; want unsupported operand type")
	}
}

func TestApplyBinary_EqualityWithAPlaceholder(t *testing.T) {
	// Equality is total across types, so it never reports unsupported — but with
	// a placeholder it cannot report a concrete answer either. A bare false would
	// be a wrong answer: the values may be equal at runtime.
	for _, op := range []Op{OpEq, OpNe} {
		for _, pair := range [][2]Value{
			{Unresolved(TInt), Int(1)},
			{Int(1), Unresolved(TInt)},
			{Unresolved(TString), Int(1)},
			{Unresolved(TInt), Unresolved(TInt)},
		} {
			t.Run(op.String()+" "+pair[0].Type.String()+" "+pair[1].Type.String(), func(t *testing.T) {
				got, err := applyBinary(op, pair[0], pair[1])
				if err != nil {
					t.Fatalf("applyBinary: %v", err)
				}
				if want := "unresolved[bool]"; got.Type.String() != want {
					t.Errorf("type = %q; want %q", got.Type.String(), want)
				}
			})
		}
	}
}

func TestApplyBinary_PlaceholderChains(t *testing.T) {
	// A placeholder result keeps propagating, so a longer expression does not
	// fail at the second operator.
	first, err := applyBinary(OpAdd, Unresolved(TInt), Int(1))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := applyBinary(OpMul, first, Int(2))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if want := "unresolved[int]"; second.Type.String() != want {
		t.Errorf("type = %q; want %q", second.Type.String(), want)
	}
}

func TestListOperators(t *testing.T) {
	rng, err := RangeExpr("1-3")
	if err != nil {
		t.Fatalf("RangeExpr: %v", err)
	}
	syms := MapSymbols{"Param.Range": rng}
	tests := []struct {
		src      string
		want     string
		wantType string
	}{
		{"[1, 2] + [3, 4]", "[1, 2, 3, 4]", "list[int]"},
		{"[1] + [2.0]", "[1.0, 2.0]", "list[float]"},
		{"[] + [1]", "[1]", "list[int]"},
		{"[1] + []", "[1]", "list[int]"},
		{"[['a']] + [['b']]", "[[a], [b]]", "list[list[string]]"},
		{"[0] * 3", "[0, 0, 0]", "list[int]"},
		{"[1, 2] * 2", "[1, 2, 1, 2]", "list[int]"},
		{"[1] * 0", "[]", "list[int]"},
		{"'ab' * 3", "ababab", "string"},
		{"'x' * 0", "", "string"},
		{"2 in [1, 2, 3]", "true", "bool"},
		{"9 in [1, 2, 3]", "false", "bool"},
		{"9 not in [1, 2, 3]", "true", "bool"},
		{"2.0 in [1, 2, 3]", "true", "bool"},
		{"Param.Range + [4]", "[1, 2, 3, 4]", "list[int]"},
		{"[0] + Param.Range", "[0, 1, 2, 3]", "list[int]"},
		{"Param.Range + Param.Range", "[1, 2, 3, 1, 2, 3]", "list[int]"},
		{"2 in Param.Range", "true", "bool"},
		{"9 not in Param.Range", "true", "bool"},
		{"'n=' + Param.Range", "n=1-3", "string"},
		{"Param.Range + '!'", "1-3!", "string"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, syms, TAny)
			if err != nil {
				t.Fatalf("Eval(%q): %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %q, want %q", tc.src, got, tc.want)
			}
			if got := v.Type.String(); got != tc.wantType {
				t.Errorf("Eval(%q) type = %s, want %s", tc.src, got, tc.wantType)
			}
		})
	}
}

func TestListOperators_Errors(t *testing.T) {
	tests := []struct {
		src      string
		wantSubs string
	}{
		{"['a'] + [1]", "incompatible"},
		{"[1] * 'a'", "unsupported operand types"},
		{"[1] + 1", "unsupported operand types"},
		{"[0] * 100000000", "too large"},
		{"'x' * 100000000", "too large"},
		// String CONCATENATION was unbounded while repetition was not, so a
		// chain of individually-legal repetitions walked straight past
		// maxStringBytes. Each term here is well inside the bound on its own;
		// only the sum is over it, which is exactly what concatStrings now
		// checks.
		{"'xxxxxxxxxx' * 900000 + 'xxxxxxxxxx' * 900000", "too large"},
		// The empty-list literal's provisional type-variable binding is
		// replaceable only by a type that has a list layer wherever the binding
		// did. Without that restriction "[] < [1]"'s fix would also make this
		// match, and it would then fail inside coerce() instead of being
		// reported as an unsupported operand pair.
		{"[[]] < [1]", "unsupported operand types"},
		// The rest of the fence around that exception, in both operand orders.
		// Widening it far enough to order "[[1]] < [[]]" must not turn any of
		// these into a match: only a binding (or an argument) that bottoms out
		// at nulltype under matching list layers is replaceable, and none of
		// these does.
		{"['a'] < [1]", "unsupported operand types"},
		{"[1] < ['a']", "unsupported operand types"},
		{"[1] < [[]]", "unsupported operand types"},
		{"[] < 1", "unsupported operand types"},
		{"1 < []", "unsupported operand types"},
		// Section 2.1.4's compatible pairs ARE now applied elementwise (see
		// orderingUnified, shape.go, and TestListOrdering_CompatiblePairs
		// below), which widens what list ordering admits — so every row above
		// is also the fence around that widening. int/float and string/path
		// unify; a genuinely mismatched element pair, at any nesting depth,
		// still has no shape to match.
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			_, err := Eval(tc.src, nil, TAny)
			if err == nil {
				t.Fatalf("Eval(%q) = nil error, want one mentioning %q", tc.src, tc.wantSubs)
			}
			if !strings.Contains(err.Error(), tc.wantSubs) {
				t.Fatalf("Eval(%q) error = %q, want it to mention %q", tc.src, err.Error(), tc.wantSubs)
			}
		})
	}
}

// TestRangeExpr_TextCoercionIsAddOnly pins an adjudication, so that the next
// reader sees it was decided rather than defaulted: section 1.2.3's
// range_expr -> string coercion is granted to the operators whose tables name a
// range_expr row, and to no others.
//
// "Param.Range * 2" used to evaluate to the string "1-101-10" — the range
// coerced to its own text and repeated — which is spec-legal only if that
// coercion fires during operator overload selection wherever a string
// parameter appears. It does not: section 2.1.2 writes out
// __add__(string, range_expr) and __add__(range_expr, string) as explicit rows,
// and section 2.1.3 writes out three more __add__ rows plus
// __contains__(range_expr, int), which would all be redundant if a range_expr
// reached a string parameter on its own. Neither table gives __mul__ any
// range_expr row, nor gives "in" a string/range_expr one.
//
// Both halves are tested together because they are one ruling: the same
// promoteNoRangeText annotation (shape.go) produces both, and the "+" rows that
// the spec DOES name must keep working, or the ruling has been over-applied.
// The reference implementation independently rejects both rejected forms
// ("Cannot use '*' operator with range_expr and int", "Cannot use 'in'
// operator with range_expr and string") — corroboration, not the reason.
func TestRangeExpr_TextCoercionIsAddOnly(t *testing.T) {
	syms := MapSymbols{"Param.Range": mustRangeExpr(t, "1-10")}
	rejected := []string{
		// Section 2.1.2's __mul__(string, int) must not swallow a range_expr.
		"Param.Range * 2",
		// A non-positive count took the same route and produced "".
		"Param.Range * 0",
		// Section 2.1.2's __contains__(string, string) must not either: this
		// asked whether "1" is a SUBSTRING of "1-10", which it is, rather than
		// the membership question section 2.1.3 defines.
		"'1' in Param.Range",
		"'1' not in Param.Range",
		"Param.Range in '1-10'",
	}
	for _, src := range rejected {
		t.Run("rejected: "+src, func(t *testing.T) {
			_, err := Eval(src, syms, TAny)
			if err == nil {
				t.Fatalf("Eval(%q) = nil error, want an unsupported-operand-types error", src)
			}
			if !strings.Contains(err.Error(), "unsupported operand types") {
				t.Fatalf("Eval(%q) error = %q, want it to mention %q", src, err.Error(), "unsupported operand types")
			}
		})
	}
	accepted := []struct {
		src  string
		want string
	}{
		// The rows section 2.1.2 names explicitly, both operand orders.
		{"Param.Range + '!'", "1-10!"},
		{"'n=' + Param.Range", "n=1-10"},
		// Section 2.1.3's own range_expr rows, untouched by the restriction.
		{"2 in Param.Range", "true"},
		{"99 not in Param.Range", "true"},
		{"Param.Range + [11]", "[1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11]"},
		// A range_expr coerced to a string TARGET is section 1.2.3's rule
		// working normally; only overload selection is restricted.
		{"Param.Range", "1-10"},
	}
	for _, tc := range accepted {
		t.Run("accepted: "+tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, syms, TAny)
			if err != nil {
				t.Fatalf("Eval(%q): %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Fatalf("Eval(%q) = %q, want %q", tc.src, got, tc.want)
			}
		})
	}
}

// TestListOperators_RepeatOverflow pins the class of bug a Critical review
// found in repeatList/repeatString: computing unitSize*n and checking the
// PRODUCT — rather than checking the operands first — lets int64
// multiplication overflow and wrap, silently defeating the bound. Every case
// here is chosen so the WRAPPED product would slip past a naive "product >
// max" check (landing on a negative number, or on a small positive one),
// while the guard that actually runs (checkRepeat, limits.go) rejects it by
// comparing the operands before ever forming that product — so each case
// here returns instantly with a clean errTooLarge rather than panicking or
// hanging.
//
// Deliberately NOT included: a list wrap-to-ZERO case (the string one below,
// "wraps to zero", has a list analog: "([0] * 1048576) * 17592186044416").
// Unlike every case actually here, that one is not safe to run against a
// reverted fix: repeatList's own loop ("for range n { append }") has no
// built-in guard the way strings.Repeat and make()'s capacity check do, so
// on a revert it does not panic — a second re-review confirmed by actually
// running it that it grows past 13GB resident and is still climbing after 15
// seconds, with no panic and no termination. A test that OOM-kills the
// process instead of failing cleanly when the thing it guards regresses is
// worse than no test at all — it turns a caught regression into an outage,
// and invites someone to delete it rather than read it. TestCheckRepeat_
// OverflowSafe (limits_internal_test.go) already covers this exact wrap-to-
// zero arithmetic directly against checkRepeat, which cannot allocate at
// all since it never calls the operator; do not re-add an end-to-end list
// version of it for symmetry.
func TestListOperators_RepeatOverflow(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		// 2 * 9223372036854775807 (math.MaxInt64) overflows int64 and wraps
		// to a NEGATIVE number. Safe on a revert: make([]Value, 0, total)
		// panics on the negative capacity.
		{"list: wraps to negative", "[0, 0] * 9223372036854775807"},
		{"string: wraps to negative", "'xx' * 9223372036854775807"},
		// 1048576 (2^20) * 17592186044416 (2^44) is exactly 2^64, which
		// wraps to ZERO — the smallest possible positive-check bypass. Safe
		// on a revert for the STRING case specifically: strings.Repeat has
		// its own internal overflow guard and panics rather than looping.
		// The inner "* 1048576" is itself well within the limit and builds
		// instantly.
		{"string: wraps to zero", "('x' * 1048576) * 17592186044416"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Eval(tc.src, nil, TAny)
			if err == nil {
				t.Fatalf("Eval(%q) = nil error, want errTooLarge", tc.src)
			}
			if !strings.Contains(err.Error(), "too large") {
				t.Fatalf("Eval(%q) error = %q, want it to mention %q", tc.src, err.Error(), "too large")
			}
		})
	}
}

func TestListOperators_Unresolved(t *testing.T) {
	syms := MapSymbols{"Param.Items": Unresolved(ListOf(TInt)), "Param.N": Unresolved(TInt)}
	tests := []struct {
		src      string
		wantType string
	}{
		{"Param.Items + [1]", "unresolved[list[int]]"},
		{"Param.Items * 3", "unresolved[list[int]]"},
		{"[1] * Param.N", "unresolved[list[int]]"},
		{"1 in Param.Items", "unresolved[bool]"},
		// Concatenation's declared result must not depend on operand ORDER.
		// It did: the shape's Ret was list[T] with T bound from the left
		// operand, so an empty left operand answered list[nulltype] — no
		// runtime value was wrong, because concatLists recomputes the element
		// type, but that path never runs when an operand has no value, which
		// is precisely when the declared type is all there is. Indexed as
		// well, since the element type is what the mistyping loses.
		{"[] + Param.Items", "unresolved[list[int]]"},
		{"Param.Items + []", "unresolved[list[int]]"},
		{"([] + Param.Items)[0]", "unresolved[int]"},
		{"(Param.Items + [])[0]", "unresolved[int]"},
		// The same unification, now with a type to FIND rather than adopt:
		// section 2.1.3's common type of int and float is float, in either
		// order, and substituting the left binding into Ret could not say so.
		{"Param.Items + [2.0]", "unresolved[list[float]]"},
		{"[2.0] + Param.Items", "unresolved[list[float]]"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, syms, TAny)
			if err != nil {
				t.Fatalf("Eval(%q): %v", tc.src, err)
			}
			if got := v.Type.String(); got != tc.wantType {
				t.Fatalf("Eval(%q) type = %s, want %s", tc.src, got, tc.wantType)
			}
		})
	}
}

func TestListEqualityAndOrdering(t *testing.T) {
	rng, err := RangeExpr("1-3")
	if err != nil {
		t.Fatalf("RangeExpr: %v", err)
	}
	syms := MapSymbols{"Param.Range": rng}
	tests := []struct {
		src  string
		want string
	}{
		// Section 1.2.5: list equality is recursive.
		{"[1, 2] == [1, 2]", "true"},
		{"[1, 2] == [1, 3]", "false"},
		{"[1, 2] == [1]", "false"},
		{"[[1]] == [[1]]", "true"},
		{"[1, 2] != [1]", "true"},
		{"[1] == 1", "false"},   // scalar vs list is always unequal
		{"[1] == 'a'", "false"}, // other cross-type comparisons are unequal
		// Section 1.2.5: a range_expr is expanded and compared elementwise.
		{"Param.Range == [1, 2, 3]", "true"},
		{"[1, 2, 3] == Param.Range", "true"},
		{"Param.Range == [1, 2]", "false"},
		// Section 1.2.5: list ordering is lexicographic, and a shorter list
		// that is otherwise equal is less.
		{"[1, 2] < [1, 3]", "true"},
		{"[1, 2] < [1, 2, 0]", "true"},
		{"[2] > [1, 9]", "true"},
		{"[1, 2] <= [1, 2]", "true"},
		{"[1, 2] >= [1, 2]", "true"},
		{"['a'] < ['b']", "true"},
		// Section 1.2.5's "if all compared elements are equal, the shorter list
		// is considered less than the longer one", with the shorter list EMPTY —
		// which this table had no case for at all, and which was order-dependent
		// as a result: "[1] < []" was already false, while "[] < [1]" errored
		// out at shape matching because the empty literal's list[nulltype] had
		// pinned the shared type variable to nulltype before int arrived. The
		// nested row is the same defect one level down, where the provisional
		// binding is list[nulltype] rather than nulltype.
		//
		// Both directions of both depths are listed, because fixing one
		// direction is what caused the second defect: the nested case worked
		// with the empty list on the LEFT and errored with it on the right,
		// since the exception was written for an empty BINDING meeting a real
		// argument and not for the reverse. A row for one direction alone
		// cannot see that.
		{"[] < [1]", "true"},
		{"[] <= [1]", "true"},
		{"[[]] < [[1]]", "true"},
		{"[[1]] < [[]]", "false"},
		{"[[1]] > [[]]", "true"},
		{"[[]] <= [[1]]", "true"},
		{"[[[1]]] < [[[]]]", "false"},
		{"[1] < []", "false"},
		{"[1] > []", "true"},
		{"[] < []", "false"},
		{"[] <= []", "true"},
		{"[] == []", "true"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, syms, TAny)
			if err != nil {
				t.Fatalf("Eval(%q): %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Fatalf("Eval(%q) = %s, want %s", tc.src, got, tc.want)
			}
		})
	}
}

// TestListOrdering_CompatiblePairs pins the second adjudication of this wave:
// section 2.1.4's compatible pairs (int/float and string/path) apply
// ELEMENTWISE to list ordering, so "[1] < [1.0]" is false rather than the
// "unsupported operand types" error it used to be.
//
// The reasoning is in orderingUnified (shape.go) and rests on composing two
// spec sections: 1.2.5 defines list ordering elementwise and says nothing about
// the elements' types, and 2.1.4 permits an ordering operator's operands to
// differ for exactly those two pairs. The error was an artifact of the shape's
// single shared type variable, which requires exact equality by construction.
//
// Every case was also queried against the reference implementation. It agrees
// on every int/float row and on the two same-type path rows. It does NOT agree
// on the MIXED path/string rows: it answers false for both
// "[path('/a')] < ['/b']" and "['/b'] < [path('/a')]", which cannot both be
// right — "/a" and "/b" are unequal, so one order must be true. Section 1.2.5
// says the path is converted to string for the comparison and section 2.1.4
// names string/path as a compatible pair, which makes "/a" < "/b" true; sqi
// answers that, and the reference is wrong. These rows are not in the oracle
// corpus (path has no literal syntax here and the corpus supplies no symbols),
// so there is no baseline entry to add — it is recorded here instead.
//
// The same-type path rows found a real gap while this was being written:
// list[path] against list[path] matches the ordering shape EXACTLY, so nothing
// coerced its elements and compareValues had no path row to compare them with.
// That is fixed here too (ops.go), since section 2.1.4 lists path as orderable.
func TestListOrdering_CompatiblePairs(t *testing.T) {
	syms := MapSymbols{
		"Param.Dir":   Value{Type: TPath, s: "/a"},
		"Param.Later": Value{Type: TPath, s: "/b"},
	}
	tests := []struct {
		src  string
		want string
	}{
		// int/float, both operand orders and all four operators.
		{"[1] < [1.0]", "false"},
		{"[1.0] < [1]", "false"},
		{"[1] <= [1.0]", "true"},
		{"[1] > [1.0]", "false"},
		{"[1] >= [1.0]", "true"},
		// The unified element type is what decides, not the left operand's:
		// both sides become list[float] and 2.0 < 3.0 settles it.
		{"[1, 2] < [1.0, 3.0]", "true"},
		{"[1.0, 3.0] > [1, 2]", "true"},
		// One level down. unifyElemPair recurses, so list[list[int]] and
		// list[list[float]] unify the same way section 1.2.6 rule 4 says a
		// literal's would.
		{"[[1]] < [[1.0]]", "false"},
		{"[[1]] <= [[1.0]]", "true"},
		{"[[1, 2]] < [[1.0, 3.0]]", "true"},
		// string/path, section 2.1.4's other compatible pair, in both orders.
		{"[Param.Dir] < ['/b']", "true"},
		{"['/b'] < [Param.Dir]", "false"},
		{"[Param.Dir] < [Param.Later]", "true"},
		{"[[Param.Dir]] < [['/b']]", "true"},
		// The empty-list rule still composes with all of it: an empty operand
		// adopts the other side's element type, whatever that turned out to be.
		{"[] < [1.0]", "true"},
		{"[1.0] < []", "false"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, syms, TAny)
			if err != nil {
				t.Fatalf("Eval(%q): %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Fatalf("Eval(%q) = %s, want %s", tc.src, got, tc.want)
			}
		})
	}
}

// TestRangeExprEquality_SelfEquality pins the fix for a previously PARKED
// bug (see listOrRangeEqual, ops.go): a range_expr must equal itself even
// though two range_exprs are not producible from EXPR source syntax alone —
// RangeExpr is an exported constructor, so a caller's symbol table can hand
// the evaluator two range_expr operands directly, and Param.R == Param.R was
// false before this fix. It also exercises the differently-spelled-but-
// equal path (two range_exprs with different text that expand to the same
// integers) and a genuine inequality.
func TestRangeExprEquality_SelfEquality(t *testing.T) {
	a := mustRangeExpr(t, "1-3")
	sameText := mustRangeExpr(t, "1-3")
	sameInts := mustRangeExpr(t, "1,2,3")
	different := mustRangeExpr(t, "1-4")
	tests := []struct {
		name string
		l, r Value
		want bool
	}{
		{"a range_expr equals itself", a, a, true},
		{"two range_exprs with identical text are equal", a, sameText, true},
		{"two range_exprs with different text but the same integers are equal", a, sameInts, true},
		{"two range_exprs with different integers are unequal", a, different, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := valuesEqual(tt.l, tt.r); got != tt.want {
				t.Errorf("valuesEqual(%v, %v) = %v; want %v", tt.l, tt.r, got, tt.want)
			}
		})
	}
}

// TestPathOperators covers RFC 0006 section 2.1.5.
//
// The absolute-right-operand row is pathlib's rule and is easy to miss: a
// relative child is appended, but an ABSOLUTE child replaces the left operand
// entirely.
func TestPathOperators(t *testing.T) {
	tests := []struct{ src, want, wantType string }{
		{`path('/a/b') / 'c'`, "/a/b/c", "path"},
		{`path('/a/b') / path('c')`, "/a/b/c", "path"},
		{`path('/a/b') / '/c'`, "/c", "path"},
		{`path('/a/b/') / 'c'`, "/a/b/c", "path"},
		{`path('s3://bucket/dir/') / 'file'`, "s3://bucket/dir/file", "path"},
		{`path('/a/b') + 'x'`, "/a/bx", "path"},
		{`path('/a') / 'b' + '_c.png'`, "/a/b_c.png", "path"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %q, want %q", tc.src, got, tc.want)
			}
			if got := v.Type.String(); got != tc.wantType {
				t.Errorf("Eval(%q) typed %s, want %s", tc.src, got, tc.wantType)
			}
		})
	}
}

// TestPathPlus_BeatsTheCoercedStringRow is the guard on a behavior CHANGE.
//
// Before this task, "P + 'x'" evaluated to the STRING "/a/bx", because
// __add__(string, string) accepts a path by coercion. RFC 0006 requires a path.
// A test asserting only the text would pass on the old behavior, so this
// asserts the type — and asserts it through a symbol as well as a literal,
// since a symbol is how a real template supplies a path.
func TestPathPlus_BeatsTheCoercedStringRow(t *testing.T) {
	syms := MapSymbols{"Param.Dir": Path("/a/b", PathPOSIX)}
	for _, src := range []string{`path('/a/b') + 'x'`, `Param.Dir + 'x'`} {
		v, err := Eval(src, syms, TAny)
		if err != nil {
			t.Fatalf("Eval(%q) failed: %v", src, err)
		}
		if got := v.Type.String(); got != "path" {
			t.Errorf("Eval(%q) typed %s, want path — the coerced string row won", src, got)
		}
	}
}

// TestPathOperators_POSIXEdges pins the shapes the brief's happy path does not
// reach, every one of them probed against the reference implementation first.
//
// The result of a join is a path VALUE, and every path value in this package is
// normalized on construction (Value.Path re-parses), so the join normalizes too:
// "P / ”", "P / '.'" and "P / 'c/'" all collapse. CPython agrees
// (PurePosixPath('/a/b') / '.' is PurePosixPath('/a/b')). The reference
// implementation at openjd-model 0.11.1 does NOT — it renders "/a/b/",
// "/a/b/." and "/a/b/c/" respectively, while simultaneously reporting the
// SAME joined value's .parts and .name as if it had normalized. That is a
// reference defect (its own two views of one value disagree), and RFC 0006
// says this family matches pathlib, so these follow pathlib. See
// task-9-report.md.
func TestPathOperators_POSIXEdges(t *testing.T) {
	tests := []struct{ src, want, wantType string }{
		{`path('/a/b') / ''`, "/a/b", "path"},
		{`path('/a/b') / '.'`, "/a/b", "path"},
		{`path('/a/b') / '..'`, "/a/b/..", "path"},
		{`path('/a/b') / 'c/d'`, "/a/b/c/d", "path"},
		{`path('/a/b') / 'c/'`, "/a/b/c", "path"},
		{`path('/a') / 'b' / 'c' / 'd'`, "/a/b/c/d", "path"},
		{`path('a/b') / 'c'`, "a/b/c", "path"},
		{`path('') / 'b'`, "b", "path"},
		{`path('.') / 'b'`, "b", "path"},
		{`path('..') / 'c'`, "../c", "path"},
		// A "//" root is absolute to CPython and to the reference, so the
		// child replaces the parent. Three or more slashes collapse to one.
		{`path('/a') / '//b'`, "//b", "path"},
		{`path('/a') / '///b'`, "/b", "path"},
		{`path('/a/b') + ''`, "/a/b", "path"},
		{`path('/a/b') + '/c'`, "/a/b/c", "path"},
		{`path('/') + 'x'`, "/x", "path"},
		{`path('/a/b') + 'x' + 'y'`, "/a/bxy", "path"},
		// A path on the RIGHT of "+" has no row of its own in RFC 0006's
		// table, so it reaches __add__(path, string) by the section 1.2.3
		// path -> string coercion, at a lower cost than the (string, string)
		// row: the result is a PATH. The reference answers the same text as a
		// string, having picked the (string, string) row instead.
		{`path('/a/b') + path('c')`, "/a/bc", "path"},
		// A string on the LEFT keeps the (string, string) row, since
		// string -> path is not a promotion the matcher will make.
		{`'x' + path('/a')`, "x/a", "string"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %q, want %q", tc.src, got, tc.want)
			}
			if got := v.Type.String(); got != tc.wantType {
				t.Errorf("Eval(%q) typed %s, want %s", tc.src, got, tc.wantType)
			}
		})
	}
}

// TestPathOperators_URI pins the one join rule RFC 0006 states outright and
// that no filesystem flavor can exercise: "A trailing slash on the left
// operand is consumed by the join". Only a URI can carry one this far — both
// filesystem flavors drop a trailing separator during parsing — and the
// reference consumes a whole trailing RUN of them, not just one.
func TestPathOperators_URI(t *testing.T) {
	tests := []struct{ src, want, wantType string }{
		{`path('s3://b/d') / 'f'`, "s3://b/d/f", "path"},
		{`path('s3://b/d/') / 'f'`, "s3://b/d/f", "path"},
		{`path('s3://b/d//') / 'f'`, "s3://b/d/f", "path"},
		{`path('s3://b/d///') / 'f'`, "s3://b/d/f", "path"},
		// Interior empty components are opaque and survive, which is the
		// whole point of the URI flavor.
		{`path('s3://b/d//x') / 'f'`, "s3://b/d//x/f", "path"},
		// UNRESOLVED — the specification and the reference disagree here, and
		// this pins the SPECIFICATION's reading. Section 2.1.5 says "for URI
		// paths, joining a relative child is equivalent to
		// path(p.parts + child.parts)": the child is a PATH, parsed under the
		// evaluator's flavor, so its own trailing separator and its own
		// repeated separators are normalized away before it ever becomes URI
		// components. The reference joins the child's TEXT instead and answers
		// "s3://b/d/a/" and "s3://b/d/a//b" — and unlike its other
		// non-normalizing join answers this is NOT the same defect, since a
		// URI parse would preserve those separators even if the reference
		// re-parsed its own result. Flagged in task-9-report.md for the final
		// review; reversing the ruling means parsing a URI parent's child as
		// URI segments rather than as a path.
		{`path('s3://b/d/') / 'a/'`, "s3://b/d/a", "path"},
		{`path('s3://b/d/') / 'a//b'`, "s3://b/d/a/b", "path"},
		{`path('s3://b/d/') + 'x'`, "s3://b/d/x", "path"},
		// A URI is always absolute, so it replaces whatever is on the left.
		{`path('/a/b') / 's3://x/y'`, "s3://x/y", "path"},
		{`path('/a/b') / path('s3://x/y')`, "s3://x/y", "path"},
		// ... and an absolute filesystem child replaces a URI parent.
		{`path('s3://b/d') / '/abs'`, "/abs", "path"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %q, want %q", tc.src, got, tc.want)
			}
			if got := v.Type.String(); got != tc.wantType {
				t.Errorf("Eval(%q) typed %s, want %s", tc.src, got, tc.wantType)
			}
		})
	}
}

// TestPathOperators_Windows is the flavor the reference implementation cannot
// answer for — its path family is POSIX-only — so every expectation here is
// CPython's, read from PureWindowsPath on the machine this was written on,
// which is the same source parseWindows and splitRootWindows were ported from.
//
// The four cases that matter are the ones a rule keyed on is_absolute() alone
// gets wrong, because a Windows child can anchor itself WITHOUT being
// absolute:
//   - "/x" has a root but no drive, so it keeps the parent's drive;
//   - "D:b" has a drive but no root, and a DIFFERENT drive discards the parent;
//   - "C:b" on the same drive is an ordinary relative child;
//   - "c:b" is the same drive spelled differently, and the child's spelling wins.
func TestPathOperators_Windows(t *testing.T) {
	tests := []struct{ src, want string }{
		{`path('C:/a') / 'b'`, `C:\a\b`},
		{`path('C:/a') / '/x'`, `C:\x`},
		{`path('C:/a') / 'D:b'`, `D:b`},
		{`path('C:/a') / 'C:b'`, `C:\a\b`},
		{`path('C:/a') / 'c:b'`, `c:\a\b`},
		{`path('C:/a') / 'C:/x'`, `C:\x`},
		{`path('C:') / 'b'`, `C:b`},
		{`path('C:/a') / '//host/share'`, `\\host\share\`},
		{`path('C:/a') / '//host/share/x'`, `\\host\share\x`},
		{`path('//srv/share') / 'b'`, `\\srv\share\b`},
		// A BARE UNC server, with no share — the anchor shape the original
		// table omitted, and the one that broke. Its drive ("\\srv") carries
		// no separator of its own, so the components glued straight onto the
		// server name and "path('//nas') / 'renders'" silently addressed a
		// DIFFERENT host ("\\nasrenders"). ntpath.join's final block inserts
		// the separator; see pathJoin. The trailing separator CPython leaves
		// on two of these is not a typo: "\\srv\b" re-parses as a
		// server+share pair, which pathlib credits with a root.
		{`path('//nas') / 'renders' / 'shot01'`, `\\nas\renders\shot01`},
		{`path('//srv') / 'b'`, `\\srv\b\`},
		{`path('//srv') / 'x/y'`, `\\srv\x\y`},
		{`path('//./dev') / 'b'`, `\\.\dev\b`},
		{`path('//?./name') / 'b'`, `\\?.\name\b`},
		// A bare "\\" anchor already ENDS in a separator, so the same block
		// must not add a second one.
		{`path('//') / 'x'`, `\\x`},
		{`path('//srv/share') / '/x'`, `\\srv\share\x`},
		{`path('a') / 'b'`, `a\b`},
		{`path('C:/a') / '..'`, `C:\a\..`},
		{`path('C:/a') / ''`, `C:\a`},
		{`path('C:/a') + 'x'`, `C:\ax`},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, MapSymbols{}, TAny, WithPathFormat(PathWindows))
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %q, want %q", tc.src, got, tc.want)
			}
			if got := v.Type.String(); got != "path" {
				t.Errorf("Eval(%q) typed %s, want path", tc.src, got)
			}
		})
	}
}

// TestPathJoin_URIAuthorityIsNeverInheritedAsADrive pins the guard
// anchorParts's own doc comment names as the thing that "must never happen",
// and which nothing exercised until the final fix wave: a URI's
// scheme+authority is reported as a ROOT, never as a drive, because pathJoin
// lets a rootless child INHERIT its parent's drive.
//
// Dropping the "p.isURI ||" clause stays green everywhere else. Under
// PathWindows, path('C://b') / 'x' then runs the authority "C://b" through
// splitRootWindows, which reads "C:" as a drive, and the join answers "C:/x" —
// the bucket gone, replaced by a drive letter that came from the scheme.
//
// The only way to reach a URI whose authority is ALSO drive-shaped is the
// one-letter scheme the specification's own grammar admits
// (^[a-zA-Z][a-zA-Z0-9+.-]*:// — a star, not a plus). That behavior is settled
// and stays; see splitURI. The point here is that the settled behavior is what
// makes the guard reachable at all, so it is exactly the case worth pinning.
func TestPathJoin_URIAuthorityIsNeverInheritedAsADrive(t *testing.T) {
	tests := []struct {
		src    string
		flavor PathFormat
		want   string
	}{
		{`path('C://b') / 'x'`, PathWindows, "C://b/x"},
		{`path('C://b') / 'x'`, PathPOSIX, "C://b/x"},
		{`path('C://b/d') / 'x'`, PathWindows, "C://b/d/x"},
		// A multi-letter scheme reaches the same arm and always did — kept so
		// the drive-shaped row above is visibly the SAME rule and not a
		// special case.
		{`path('s3://b') / 'x'`, PathWindows, "s3://b/x"},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%s under %v", tc.src, tc.flavor), func(t *testing.T) {
			v, err := Eval(tc.src, MapSymbols{}, TAny, WithPathFormat(tc.flavor))
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) under %v = %q, want %q", tc.src, tc.flavor, got, tc.want)
			}
		})
	}
}

// TestPathOperators_Unsupported pins the operand pairs RFC 0006's table does
// NOT list. Each one is an error in the reference implementation too, and each
// stays an error only because string -> path and int -> string are not
// promotions the shape matcher will make to SELECT an overload.
func TestPathOperators_Unsupported(t *testing.T) {
	for _, src := range []string{
		`'/a' / path('b')`,
		`1 / path('a')`,
		`path('a') / 1`,
		`path('a') + 1`,
	} {
		t.Run(src, func(t *testing.T) {
			if _, err := Eval(src, MapSymbols{}, TAny); err == nil {
				t.Fatalf("Eval(%q) succeeded, want an unsupported-operand error", src)
			}
		})
	}
}

// TestPathOperators_Unresolved checks the path a template takes before its
// parameter values exist: no implementation runs, so the result type is read
// off the matched shape.
func TestPathOperators_Unresolved(t *testing.T) {
	syms := MapSymbols{"Param.Dir": Unresolved(TPath)}
	for _, src := range []string{`Param.Dir / 'c'`, `Param.Dir + 'x'`} {
		v, err := Eval(src, syms, TAny)
		if err != nil {
			t.Fatalf("Eval(%q) failed: %v", src, err)
		}
		if !v.IsUnresolved() {
			t.Fatalf("Eval(%q) = %v, want an unresolved value", src, v)
		}
		c, ok := unresolvedConstraint(v.Type)
		if !ok || c.String() != "path" {
			t.Errorf("Eval(%q) constrained to %v (ok=%v), want path", src, c, ok)
		}
	}
}
