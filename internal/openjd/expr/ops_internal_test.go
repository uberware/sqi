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
			if got != tt.want {
				t.Errorf("= %v (%s); want %v (%s)", got, got.Kind, tt.want, tt.want.Kind)
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
	if err != nil || got != Int(-5) {
		t.Errorf("applyUnary(OpNeg, 5) = %v, %v; want -5, nil", got, err)
	}
	got, err = applyUnary(OpPos, Int(5))
	if err != nil || got != Int(5) {
		t.Errorf("applyUnary(OpPos, 5) = %v, %v; want 5, nil", got, err)
	}
	if _, err := applyUnary(OpNeg, Int(math.MinInt64)); err == nil {
		t.Error("negating math.MinInt64 = nil error; want integer overflow")
	}
}

func TestApplyBinary_UnsupportedOperands(t *testing.T) {
	// A missing table row IS sub-project A's same-type-only rule. Adding
	// int/float coercion is sub-project B's job; until then this must fail.
	_, err := applyBinary(OpAdd, Int(1), Float(2.5))
	if err == nil {
		t.Fatal("1 + 2.5 = nil error; want unsupported operand types")
	}
	if got := err.Error(); !strings.Contains(got, "unsupported operand types for +: int and float") {
		t.Errorf("error = %q; want it to name the operator and both kinds", got)
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
			if got != tt.want || math.Signbit(got.AsFloat()) {
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
			if got != tt.want {
				t.Errorf("= %v (%s); want %v (%s)", got, got.Kind, tt.want, tt.want.Kind)
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
			if got != tt.want {
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
			if got != tt.want {
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
			t.Errorf("not %v succeeded; want unsupported operand type", v.Kind)
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
			if got != Bool(tt.want) {
				t.Errorf("= %v; want %v", got, Bool(tt.want))
			}
		})
	}
}

func TestApplyBinary_OrderingIsSameTypeOnly(t *testing.T) {
	// Section 2.1.4 permits int/float and string/path cross-pairs, but both
	// are implicit coercion, which is sub-project B's. Until then, an error.
	for _, tt := range []struct{ l, r Value }{
		{Int(1), Float(2.5)},
		{Float(1.5), Int(2)},
		{String("a"), Int(1)},
		{Bool(true), Int(1)},
		{Null(), Null()},
	} {
		if _, err := applyBinary(OpLt, tt.l, tt.r); err == nil {
			t.Errorf("%s < %s succeeded; want unsupported operand types", tt.l.Kind, tt.r.Kind)
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
			if ne != Bool(!tt.want) {
				t.Errorf("!= gave %v; want %v", ne, Bool(!tt.want))
			}
		})
	}
}

func TestApplyBinary_EqualityIsNeverUnsupported(t *testing.T) {
	// Unlike ordering, == and != are defined for every pair of types, so they
	// must never report "unsupported operand types". The pairwise check is
	// driven off sampleValues below, which is itself keyed from kindNames —
	// the package's real kind table in value.go — so a future Kind added
	// there cannot silently drop out of this loop.
	all := sampleValues(t)
	for _, l := range all {
		for _, r := range all {
			if _, err := applyBinary(OpEq, l, r); err != nil {
				t.Errorf("%s == %s returned %v; equality is total", l.Kind, r.Kind, err)
			}
		}
	}
}

// TestApplyBinary_EqualitySelfEquality asserts that a value of every kind
// equals itself. A missing row in numericOrStringEqual (or valuesEqual) falls
// through to "return false" instead of an "unsupported operand types" error,
// so a future Kind added without extending that logic would otherwise
// compare unequal to itself with no loud signal. Driving the loop from
// sampleValues, rather than a hardcoded list, means that case fails this test
// instead.
func TestApplyBinary_EqualitySelfEquality(t *testing.T) {
	for _, v := range sampleValues(t) {
		out, err := applyBinary(OpEq, v, v)
		if err != nil {
			t.Errorf("%s == %s returned error %v; equality is total", v.Kind, v.Kind, err)
			continue
		}
		if !out.AsBool() {
			t.Errorf("a %s value did not equal itself", v.Kind)
		}
	}
}

// sampleValues returns one representative Value per Kind in kindNames, the
// package's real kind table (value.go), so tests driven from it cannot drift
// from the actual kind set as sub-projects B and C add kinds.
func sampleValues(t *testing.T) []Value {
	t.Helper()
	samples := map[Kind]Value{
		KindNull:   Null(),
		KindBool:   Bool(true),
		KindInt:    Int(1),
		KindFloat:  Float(1.5),
		KindString: String("x"),
	}
	values := make([]Value, 0, len(kindNames))
	for k := range kindNames {
		v, ok := samples[k]
		if !ok {
			// A new Kind was added to kindNames without a paired sample here.
			// Fail loudly rather than silently leaving it out of the
			// self-equality check this test exists to provide.
			t.Fatalf("no sample Value registered for Kind %s (%d); add one to samples in sampleValues", k, k)
		}
		values = append(values, v)
	}
	return values
}
