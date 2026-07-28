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
