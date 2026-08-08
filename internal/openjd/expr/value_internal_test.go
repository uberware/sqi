// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "testing"

func TestValue_Constructors(t *testing.T) {
	tests := []struct {
		name string
		v    Value
		code Code
	}{
		{"null", Null(), CodeNull},
		{"bool", Bool(true), CodeBool},
		{"int", Int(42), CodeInt},
		{"float", Float(3.5), CodeFloat},
		{"string", String("hi"), CodeString},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.v.Type.Code != tt.code {
				t.Errorf("Type = %v; want code %v", tt.v.Type, tt.code)
			}
		})
	}
}

func TestValue_ZeroValueIsNull(t *testing.T) {
	// The zero Value must be a usable null, so a function returning
	// (Value{}, err) never hands back an incoherent value.
	var v Value
	if v.Type.Code != CodeNull {
		t.Errorf("zero Value Type = %v; want CodeNull", v.Type)
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

func TestValue_AccessorOnWrongCodePanics(t *testing.T) {
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

func TestValue_CarriesItsType(t *testing.T) {
	tests := []struct {
		name string
		v    Value
		want string
	}{
		{"null", Null(), "nulltype"},
		{"bool", Bool(true), "bool"},
		{"int", Int(7), "int"},
		{"float", Float(1.5), "float"},
		{"string", String("x"), "string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.v.Type.String(); got != tt.want {
				t.Errorf("Type = %q; want %q", got, tt.want)
			}
		})
	}
}

func TestValue_Equal(t *testing.T) {
	tests := []struct {
		name string
		a, b Value
		want bool
	}{
		{"same int", Int(1), Int(1), true},
		{"different int", Int(1), Int(2), false},
		{"int against float of the same magnitude is not Equal", Int(1), Float(1), false},
		{"same string", String("a"), String("a"), true},
		{"different string", String("a"), String("b"), false},
		{"same bool", Bool(true), Bool(true), true},
		{"different bool", Bool(true), Bool(false), false},
		{"null equals null", Null(), Null(), true},
		{"null against int", Null(), Int(0), false},
		{"same unresolved constraint", Unresolved(TInt), Unresolved(TInt), true},
		{"different unresolved constraint", Unresolved(TInt), Unresolved(TString), false},
		{"unresolved against a concrete value of that type", Unresolved(TInt), Int(1), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.Equal(tt.b); got != tt.want {
				t.Errorf("%s.Equal(%s) = %v; want %v", tt.a.Type, tt.b.Type, got, tt.want)
			}
			if got := tt.b.Equal(tt.a); got != tt.want {
				t.Errorf("Equal is not symmetric for %s and %s", tt.a.Type, tt.b.Type)
			}
		})
	}
}

func TestUnresolved(t *testing.T) {
	tests := []struct {
		name       string
		constraint Type
		wantType   string
	}{
		{"int", TInt, "unresolved[int]"},
		{"any", TAny, "unresolved"},
		{"list", ListOf(TPath), "unresolved[list[path]]"},
		{"union", UnionOf(TInt, TString), "unresolved[int | string]"},
		{"already unresolved flattens", UnresolvedOf(TInt), "unresolved[int]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := Unresolved(tt.constraint)
			if got := v.Type.String(); got != tt.wantType {
				t.Errorf("Type = %q; want %q", got, tt.wantType)
			}
			if !v.IsUnresolved() {
				t.Error("IsUnresolved() = false; want true")
			}
		})
	}
}

func TestUnresolved_CarriesNoPayload(t *testing.T) {
	// A placeholder has a type but no value. This is what sub-project A's
	// separation of the type tag from the payload was built to allow, and it is
	// what lets B1 type-check a list-typed parameter with no list machinery.
	v := Unresolved(TInt)
	if !v.Equal(Unresolved(TInt)) {
		t.Error("two placeholders with the same constraint are not Equal")
	}
	// Reading a payload off a placeholder is a dispatch bug, and must be loud.
	defer func() {
		if recover() == nil {
			t.Error("AsInt() on a placeholder did not panic")
		}
	}()
	_ = v.AsInt()
}

func TestValue_ConcreteValuesAreNotUnresolved(t *testing.T) {
	for _, v := range []Value{Null(), Bool(false), Int(0), Float(0), String("")} {
		t.Run(v.Type.String(), func(t *testing.T) {
			if v.IsUnresolved() {
				t.Error("IsUnresolved() = true; want false")
			}
		})
	}
}

func TestList_EqualAndString(t *testing.T) {
	tests := []struct {
		name   string
		a, b   Value
		want   bool
		render string
	}{
		{
			name:   "same elements are equal",
			a:      List(TInt, []Value{Int(1), Int(2)}),
			b:      List(TInt, []Value{Int(1), Int(2)}),
			want:   true,
			render: "[1, 2]",
		},
		{
			name:   "different elements are not equal",
			a:      List(TInt, []Value{Int(1), Int(2)}),
			b:      List(TInt, []Value{Int(1), Int(3)}),
			want:   false,
			render: "[1, 2]",
		},
		{
			name:   "different lengths are not equal",
			a:      List(TInt, []Value{Int(1)}),
			b:      List(TInt, []Value{Int(1), Int(2)}),
			want:   false,
			render: "[1]",
		},
		{
			name:   "empty list",
			a:      List(TNull, nil),
			b:      List(TNull, nil),
			want:   true,
			render: "[]",
		},
		{
			name:   "nested lists compare elementwise",
			a:      List(ListOf(TInt), []Value{List(TInt, []Value{Int(1)})}),
			b:      List(ListOf(TInt), []Value{List(TInt, []Value{Int(1)})}),
			want:   true,
			render: "[[1]]",
		},
		{
			name:   "strings render quoted",
			a:      List(TString, []Value{String("a"), String("b")}),
			b:      List(TString, []Value{String("a"), String("b")}),
			want:   true,
			render: `["a", "b"]`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.Equal(tc.b); got != tc.want {
				t.Errorf("Equal() = %v, want %v", got, tc.want)
			}
			if got := tc.a.String(); got != tc.render {
				t.Errorf("String() = %q, want %q", got, tc.render)
			}
		})
	}
}

func TestList_TypeAndPayload(t *testing.T) {
	v := List(TInt, []Value{Int(1), Int(2)})
	if want := ListOf(TInt); !v.Type.Equal(want) {
		t.Fatalf("Type = %s, want %s", v.Type, want)
	}
	if got := v.AsList(); len(got) != 2 || got[0].AsInt() != 1 || got[1].AsInt() != 2 {
		t.Fatalf("AsList() = %v, want [1 2]", got)
	}
}

func TestList_AsListPanicsOnNonList(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("AsList on an int did not panic")
		}
	}()
	_ = Int(1).AsList()
}

// TestValueEqual_IgnoresTheRenderedForm pins invariant 2 of the float carry:
// two floats with the same number are the same value however they render.
// Roughly thirty tests across this package compare values with Equal, and a
// carry that participated would make round(3.5, 2) unequal to 3.5.
func TestValueEqual_IgnoresTheRenderedForm(t *testing.T) {
	plain := Float(3.5)
	carried := floatRendered(3.5, "3.50")
	if !plain.Equal(carried) {
		t.Error("Float(3.5) is not Equal to the same number with a rendered form")
	}
	if !carried.Equal(plain) {
		t.Error("Equal is not symmetric across the rendered form")
	}
	if carried.String() != "3.50" {
		t.Errorf("String() = %q, want %q", carried.String(), "3.50")
	}
	if plain.String() != "3.5" {
		t.Errorf("String() = %q, want %q", plain.String(), "3.5")
	}
}

func TestValueString_ListElementsAreQuoted(t *testing.T) {
	tests := []struct {
		name string
		val  Value
		want string
	}{
		{"strings", List(TString, []Value{String("a"), String("b")}), `["a", "b"]`},
		{"ints are not quoted", List(TInt, []Value{Int(1), Int(2)}), "[1, 2]"},
		{"empty", List(TString, nil), "[]"},
		{"single", List(TString, []Value{String("x")}), `["x"]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.val.String(); got != tt.want {
				t.Errorf("String() = %s; want %s", got, tt.want)
			}
		})
	}
}

// TestValueString_VersusStringFunction asserts that the two renderings AGREE
// -- byte-for-byte, quoting and escaping both -- on every case below, now
// that both quote a list's string elements. This is the durable regression
// check doc.go's BOUNDED EVALUATION bullet cites by name; do not let the two
// drift apart again without updating that citation.
//
// The assertion compares the two outputs to EACH OTHER rather than against a
// hand-typed expected string on purpose: transcribing an escaped literal by
// hand is exactly the failure mode this sub-project keeps finding in itself
// (see the sub-project's own ledger), and the property under test IS "these
// two agree", which a direct comparison states without needing to predict
// either side's exact spelling.
//
// This does not mean the two agree UNIVERSALLY -- see
// TestValueString_DivergesFromStringFunctionOutsideMeasuredSet immediately
// below, which pins four classes of input where they provably do not.
func TestValueString_VersusStringFunction(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{"embedded double quote", `['a"b']`},
		{"non-ASCII", `['café']`},
		{"angle brackets and ampersand", `['a<b>c&d']`},
		{"newline", `['a\nb']`},
		{"backslash", `['a\\b']`},
		{"tab", `['a\tb']`},
		{"backspace", `['a\x08b']`},
		{"formfeed", `['a\x0cb']`},
		{"carriage return", `['a\rb']`},
		{"emoji", `['a😀b']`},
		{"U+2028 line separator", `['a\u2028b']`},
		{"U+2029 paragraph separator", `['a\u2029b']`},
		{"empty string", `['']`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := Eval(tt.src, nil, TAny)
			if err != nil {
				t.Fatalf("Eval(%s): %v", tt.src, err)
			}
			s, err := Eval("string("+tt.src+")", nil, TString)
			if err != nil {
				t.Fatalf("Eval(string(%s)): %v", tt.src, err)
			}
			got, want := v.String(), s.AsStr()
			if got != want {
				t.Errorf("Value.String() = %s; string(list) = %s; want the two to agree", got, want)
			}
		})
	}
}

// TestValueString_DivergesFromStringFunctionOutsideMeasuredSet pins the four
// classes of input where Value.String() (value.go's strconv.Quote) and
// string(list)'s JSON row (funcsconv.go's writeJSONValue, an
// encoding/json.Encoder with SetEscapeHTML(false)) are independent
// implementations that provably do NOT agree, so doc.go's narrow "agrees on
// every case exercised" claim stays testable in both directions rather than
// only the agreeing one.
//
// Constructed directly through the List/String value constructors rather
// than through Eval: an EXPR source string must itself be valid UTF-8, so
// the invalid-UTF-8 case cannot be expressed as parseable source text at
// all. Building every case the same way keeps the six comparable.
func TestValueString_DivergesFromStringFunctionOutsideMeasuredSet(t *testing.T) {
	tests := []struct {
		name string
		s    string
	}{
		{"C0 control U+0000", "\x00"},
		{"C0 control U+0001", "\x01"},
		{"C0 control U+001B (ESC)", "\x1b"},
		{"vertical tab U+000B", "\v"},
		{"DEL U+007F", "\x7f"},
		{"invalid UTF-8", "\xff\xfe"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			list := List(TString, []Value{String(tt.s)})
			valueStr := list.String()
			jsonStr, err := jsonList(list)
			if err != nil {
				t.Fatalf("jsonList(%q): %v", tt.s, err)
			}
			if valueStr == jsonStr {
				t.Errorf("Value.String() and string(list) unexpectedly AGREE on %q (both %s); "+
					"update doc.go's divergence claim if this is now correct", tt.s, valueStr)
			}
		})
	}
}
