// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "testing"

func TestValue_Constructors(t *testing.T) {
	tests := []struct {
		name string
		v    Value
		kind Kind
	}{
		{"null", Null(), KindNull},
		{"bool", Bool(true), KindBool},
		{"int", Int(42), KindInt},
		{"float", Float(3.5), KindFloat},
		{"string", String("hi"), KindString},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.v.Kind != tt.kind {
				t.Errorf("Kind = %v; want %v", tt.v.Kind, tt.kind)
			}
		})
	}
}

func TestValue_ZeroValueIsNull(t *testing.T) {
	// The zero Value must be a usable null, so a function returning
	// (Value{}, err) never hands back an incoherent value.
	var v Value
	if v.Kind != KindNull {
		t.Errorf("zero Value Kind = %v; want KindNull", v.Kind)
	}
	if !v.IsNull() {
		t.Error("zero Value IsNull() = false; want true")
	}
}

func TestValue_Accessors(t *testing.T) {
	if got := Bool(true).AsBool(); got != true {
		t.Errorf("AsBool() = %v; want true", got)
	}
	if got := Int(42).AsInt(); got != 42 {
		t.Errorf("AsInt() = %d; want 42", got)
	}
	if got := Float(3.5).AsFloat(); got != 3.5 {
		t.Errorf("AsFloat() = %v; want 3.5", got)
	}
	if got := String("hi").AsStr(); got != "hi" {
		t.Errorf("AsStr() = %q; want %q", got, "hi")
	}
}

func TestValue_AccessorOnWrongKindPanics(t *testing.T) {
	// Reading the wrong payload is a programming error in the dispatch table,
	// not a runtime condition. Failing loudly beats returning a silent zero
	// that would flow into a rendered command line.
	defer func() {
		if recover() == nil {
			t.Error("AsInt on a string value did not panic")
		}
	}()
	_ = String("hi").AsInt()
}

func TestValue_String(t *testing.T) {
	tests := []struct {
		name string
		v    Value
		want string
	}{
		{"null renders as the json alias", Null(), "null"},
		{"true", Bool(true), "true"},
		{"false", Bool(false), "false"},
		{"int", Int(-42), "-42"},
		{"float", Float(3.5), "3.5"},
		// Written as a literal, not as Float(0.1 + 0.2): Go folds that constant
		// expression at compile time with exact arithmetic, yielding 0.3.
		{"float uses the shortest representation", Float(0.30000000000000004), "0.30000000000000004"},
		{"float that is integral keeps a point", Float(4), "4.0"},
		{"large float stays in fixed notation like python", Float(1e10), "10000000000.0"},
		{"small float stays in fixed notation", Float(0.0015), "0.0015"},
		{"very large float uses exponent notation", Float(1e300), "1e+300"},
		{"very small float uses exponent notation", Float(1e-300), "1e-300"},
		{"string is rendered verbatim", String("a b"), "a b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.v.String(); got != tt.want {
				t.Errorf("String() = %q; want %q", got, tt.want)
			}
		})
	}
}

func TestKind_String(t *testing.T) {
	tests := []struct {
		k    Kind
		want string
	}{
		{KindNull, "null"},
		{KindBool, "bool"},
		{KindInt, "int"},
		{KindFloat, "float"},
		{KindString, "string"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.k.String(); got != tt.want {
				t.Errorf("String() = %q; want %q", got, tt.want)
			}
		})
	}
}
