// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "testing"

// noFn is a placeholder implementation for shapes whose Fn is never called,
// which is every shape in this file: these tests are about matching, not
// arithmetic.
func noFn(_ []Value) (Value, error) { return Value{}, nil }

func TestMatchShapes_ExactBeatsCoercion(t *testing.T) {
	// The float shape is listed FIRST so that a shape-by-shape search would
	// coerce the ints and pick it. Two passes over the whole list are what make
	// 1 + 1 stay an int.
	shapes := []Shape{
		{Params: []Type{TFloat, TFloat}, Ret: TFloat, Fn: noFn},
		{Params: []Type{TInt, TInt}, Ret: TInt, Fn: noFn},
	}
	got, _, ok := matchShapes(shapes, []Type{TInt, TInt})
	if !ok {
		t.Fatal("no shape matched two ints")
	}
	if !got.Ret.Equal(TInt) {
		t.Errorf("Ret = %s; want int — the exact shape lost to the coercing one", got.Ret)
	}
}

func TestMatchShapes_CoercionPromotesAMixedPair(t *testing.T) {
	// Section 2.1.1: "when mixing int and float operands, the int is promoted to
	// float and the float overload is used". That falls out of the second pass.
	shapes := []Shape{
		{Params: []Type{TInt, TInt}, Ret: TInt, Fn: noFn},
		{Params: []Type{TFloat, TFloat}, Ret: TFloat, Fn: noFn},
	}
	got, _, ok := matchShapes(shapes, []Type{TInt, TFloat})
	if !ok {
		t.Fatal("no shape matched an int and a float")
	}
	if !got.Ret.Equal(TFloat) {
		t.Errorf("Ret = %s; want float", got.Ret)
	}
}

func TestMatchShapes_NoMatch(t *testing.T) {
	shapes := []Shape{{Params: []Type{TInt, TInt}, Ret: TInt, Fn: noFn}}
	tests := []struct {
		name string
		args []Type
	}{
		{"wrong type", []Type{TString, TString}},
		{"too few arguments", []Type{TInt}},
		{"too many arguments", []Type{TInt, TInt, TInt}},
		{"a list where a scalar is wanted", []Type{ListOf(TInt), TInt}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, ok := matchShapes(shapes, tt.args); ok {
				t.Error("a shape matched; want no match")
			}
		})
	}
}

func TestMatchShapes_UnresolvedMatchesOnItsConstraint(t *testing.T) {
	// A placeholder has no value, but its constraint is a type, so it can select
	// a shape. This is the step that makes type checking work.
	shapes := []Shape{{Params: []Type{TInt, TInt}, Ret: TInt, Fn: noFn}}
	args := []Type{UnresolvedOf(TInt), TInt}
	got, _, ok := matchShapes(shapes, args)
	if !ok {
		t.Fatal("no shape matched a placeholder int against an int")
	}
	if !got.Ret.Equal(TInt) {
		t.Errorf("Ret = %s; want int", got.Ret)
	}
}

func TestMatchShapes_BindsTypeVariables(t *testing.T) {
	// __getitem__(list[T], int) -> T: the return type follows the element type.
	shapes := []Shape{{
		Params: []Type{ListOf(Type{Code: CodeVarT}), TInt},
		Ret:    Type{Code: CodeVarT},
		Fn:     noFn,
	}}
	tests := []struct {
		name string
		list Type
		want string
	}{
		{"list of int yields int", ListOf(TInt), "int"},
		{"list of string yields string", ListOf(TString), "string"},
		{"list of list yields the inner list", ListOf(ListOf(TFloat)), "list[float]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, b, ok := matchShapes(shapes, []Type{tt.list, TInt})
			if !ok {
				t.Fatalf("no shape matched %s", tt.list)
			}
			if ret := substitute(got.Ret, b).String(); ret != tt.want {
				t.Errorf("substituted Ret = %q; want %q", ret, tt.want)
			}
		})
	}
}

func TestMatchShapes_ATypeVariableBindsOnce(t *testing.T) {
	// One variable used twice must see the same type both times. A signature
	// wanting two independent types uses T1 and T2.
	same := []Shape{{
		Params: []Type{{Code: CodeVarT}, {Code: CodeVarT}},
		Ret:    Type{Code: CodeVarT},
		Fn:     noFn,
	}}
	if _, _, ok := matchShapes(same, []Type{TInt, TInt}); !ok {
		t.Error("T,T did not match two ints")
	}
	if _, _, ok := matchShapes(same, []Type{TInt, TString}); ok {
		t.Error("T,T matched an int and a string; a variable must bind once")
	}

	independent := []Shape{{
		Params: []Type{{Code: CodeVarT1}, {Code: CodeVarT2}},
		Ret:    TBool,
		Fn:     noFn,
	}}
	if _, _, ok := matchShapes(independent, []Type{TInt, TString}); !ok {
		t.Error("T1,T2 did not match an int and a string")
	}
}

func TestMatchShapes_AnyParamAcceptsAnything(t *testing.T) {
	shapes := []Shape{{Params: []Type{TAny}, Ret: TBool, Fn: noFn}}
	for _, arg := range []Type{TInt, TString, ListOf(TPath), TNull, UnresolvedOf(TFloat)} {
		t.Run(arg.String(), func(t *testing.T) {
			if _, _, ok := matchShapes(shapes, []Type{arg}); !ok {
				t.Errorf("an any parameter rejected %s", arg)
			}
		})
	}
}

func TestSubstitute(t *testing.T) {
	b := bindings{CodeVarT: TInt, CodeVarT1: TString}
	tests := []struct {
		name string
		in   Type
		want string
	}{
		{"a bound variable", Type{Code: CodeVarT}, "int"},
		{"a second bound variable", Type{Code: CodeVarT1}, "string"},
		{"an unbound variable is left alone", Type{Code: CodeVarT2}, "T2"},
		{"a variable inside a list", ListOf(Type{Code: CodeVarT}), "list[int]"},
		{"a concrete type is unchanged", ListOf(TFloat), "list[float]"},
		{
			"a variable inside a union normalizes after substitution",
			UnionOf(Type{Code: CodeVarT}, TString), "int | string",
		},
		{
			"substituting a duplicate collapses it",
			UnionOf(Type{Code: CodeVarT}, TInt), "int",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := substitute(tt.in, b).String(); got != tt.want {
				t.Errorf("substitute(%s) = %q; want %q", tt.in, got, tt.want)
			}
		})
	}
}
