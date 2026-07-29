// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
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

func TestApplyBinary_StringRepetitionIsDeferred(t *testing.T) {
	// Deliberately absent until sub-project E supplies the operation limit
	// that makes an unbounded repeat count safe. If this ever starts passing,
	// confirm the limit landed with it.
	if _, err := applyBinary(OpMul, String("x"), Int(3)); err == nil {
		t.Error("'x' * 3 succeeded; string repetition is deferred to sub-project E")
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
	// CodeRangeExpr is deliberately absent, and its absence is the finding this
	// comment records rather than hides. Two range_exprs comparing their string
	// payloads would make '1-3' != '1,2,3', yet section 1.2.5 has a range_expr
	// EXPAND when compared against a list — so the same two values would be
	// equal once expansion exists. Which answer is right is undecided until the
	// sub-project that implements expansion decides it; adding a payload
	// comparison now would bake in the guess. valuesEqual's fallthrough makes a
	// range_expr unequal to itself in the meantime, unreachable through Eval
	// because nothing constructs two range_expr operands.
	//
	// CodePath is present precisely because it has no such ambiguity.
	scalarCodes := []Code{CodeNull, CodeBool, CodeInt, CodeFloat, CodeString, CodePath}
	samples := map[Code]Value{
		CodeNull:   Null(),
		CodeBool:   Bool(true),
		CodeInt:    Int(1),
		CodeFloat:  Float(1.5),
		CodeString: String("x"),
		// The package exports no path constructor — section 1.2.3's string->path
		// coercion is the only route to one, and is how a real path value is
		// built at the evaluation boundary.
		CodePath: mustCoerce(t, String("/a/b"), TPath),
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
		{"string times int is deferred to sub-project E", OpMul, String("ab"), Int(3)},
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
