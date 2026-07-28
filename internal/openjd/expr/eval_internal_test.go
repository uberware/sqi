// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"strings"
	"testing"
)

func evalSrc(t *testing.T, src string, syms Symbols) (Value, error) {
	t.Helper()
	e, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	return e.Eval(syms)
}

func TestEval_Literals(t *testing.T) {
	tests := []struct {
		src  string
		want Value
	}{
		{"42", Int(42)},
		{"0xFF_FF", Int(65535)},
		{"3.5", Float(3.5)},
		{"1e3", Float(1000)},
		{`'hi'`, String("hi")},
		{`r'a\nb'`, String(`a\nb`)},
		{"True", Bool(true)},
		{"false", Bool(false)},
		{"None", Null()},
		{"null", Null()},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			got, err := evalSrc(t, tt.src, nil)
			if err != nil {
				t.Fatalf("Eval: %v", err)
			}
			if got != tt.want {
				t.Errorf("= %v (%s); want %v (%s)", got, got.Kind, tt.want, tt.want.Kind)
			}
		})
	}
}

func TestEval_Names(t *testing.T) {
	syms := MapSymbols{
		"Param.X":          Int(10),
		"Task.Param.Frame": Int(7),
		"Param.if":         String("keyword attribute"),
	}
	tests := []struct {
		src  string
		want Value
	}{
		{"Param.X", Int(10)},
		{"Task.Param.Frame", Int(7)},
		{"Param.if", String("keyword attribute")},
		{"Param.X + 3", Int(13)},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			got, err := evalSrc(t, tt.src, syms)
			if err != nil {
				t.Fatalf("Eval: %v", err)
			}
			if got != tt.want {
				t.Errorf("= %v; want %v", got, tt.want)
			}
		})
	}
}

func TestEval_UnknownSymbol(t *testing.T) {
	_, err := evalSrc(t, "1 + Param.DoesNotExist", MapSymbols{})
	if err == nil {
		t.Fatal("Eval = nil error; want unknown symbol")
	}
	if !strings.Contains(err.Error(), `unknown symbol "Param.DoesNotExist"`) {
		t.Errorf("error = %q; want it to name the symbol", err.Error())
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error is %T; want *Error", err)
	}
	if e.Offset != 4 {
		t.Errorf("Offset = %d; want 4 (the start of the name)", e.Offset)
	}
}

func TestEval_NilSymbolsIsAnEmptyTable(t *testing.T) {
	// A nil Symbols must behave as an empty table, not panic: callers that
	// evaluate a constant expression should not have to build one.
	if _, err := evalSrc(t, "Param.X", nil); err == nil {
		t.Error("Eval with nil symbols = nil error; want unknown symbol")
	}
	if v, err := evalSrc(t, "1 + 1", nil); err != nil || v != Int(2) {
		t.Errorf("Eval(1 + 1, nil) = %v, %v; want 2, nil", v, err)
	}
}

func TestEval_ErrorBlamesTheOperator(t *testing.T) {
	// The offset must point at the operator that failed, not at the start of
	// the expression. This is the whole reason offsets ride on tree nodes.
	_, err := evalSrc(t, "Param.A + Param.B", MapSymbols{
		"Param.A": String("x"),
		"Param.B": Int(1),
	})
	if err == nil {
		t.Fatal("Eval = nil error; want unsupported operand types")
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error is %T; want *Error", err)
	}
	if e.Offset != 8 {
		t.Errorf("Offset = %d; want 8 (the \"+\")", e.Offset)
	}
	if !strings.Contains(e.Error(), "unsupported operand types for +: string and int") {
		t.Errorf("error = %q; want it to name the operator and both kinds", e.Error())
	}
}

func TestEval_ErrorUnwrapsToTheCause(t *testing.T) {
	_, err := evalSrc(t, "1 / 0", nil)
	if err == nil {
		t.Fatal("Eval = nil error; want division by zero")
	}
	if !errors.Is(err, errDivideByZero) {
		t.Errorf("errors.Is(err, errDivideByZero) = false; err = %v", err)
	}
}

func TestEval_Arithmetic(t *testing.T) {
	tests := []struct {
		src  string
		want Value
	}{
		{"1 + 2 * 3", Int(7)},
		{"(1 + 2) * 3", Int(9)},
		{"10 / 4", Float(2.5)},
		{"-7 // 3", Int(-3)},
		{"-7 % 3", Int(2)},
		{"2 ** 10", Int(1024)},
		{"2 ** -2", Float(0.25)},
		{"-2 ** 2", Int(-4)},
		{"--5", Int(5)},
		{`'a' + 'b'`, String("ab")},
		{"not true", Bool(false)},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			got, err := evalSrc(t, tt.src, nil)
			if err != nil {
				t.Fatalf("Eval: %v", err)
			}
			if got != tt.want {
				t.Errorf("= %v (%s); want %v (%s)", got, got.Kind, tt.want, tt.want.Kind)
			}
		})
	}
}
