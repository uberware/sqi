// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"strings"
	"testing"
)

func TestIncludes(t *testing.T) {
	tests := []struct {
		name   string
		target Type
		code   Code
		want   bool
	}{
		{"scalar matches itself", TInt, CodeInt, true},
		{"scalar does not match another", TInt, CodeFloat, false},
		{"any matches everything", TAny, CodeList, true},
		{"union member matches", UnionOf(TInt, TString), CodeString, true},
		{"union non-member does not", UnionOf(TInt, TString), CodeFloat, false},
		{"optional includes its member", OptionalOf(TInt), CodeInt, true},
		{"optional includes nulltype", OptionalOf(TInt), CodeNull, true},
		{"unresolved looks through to its constraint", UnresolvedOf(TInt), CodeInt, true},
		{"unresolved of any matches everything", UnresolvedOf(TAny), CodeString, true},
		{"list matches by code, not element type", ListOf(TInt), CodeList, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := includes(tt.target, tt.code); got != tt.want {
				t.Errorf("includes(%s, %s) = %v; want %v", tt.target, tt.code, got, tt.want)
			}
		})
	}
}

func TestCoercible(t *testing.T) {
	tests := []struct {
		name string
		from string // parsed with ParseType, so the table reads as the spec does
		to   string
		want bool
	}{
		{"identical", "int", "int", true},
		{"anything reaches any", "list[int]", "any", true},

		// int -> float when the target does not include int.
		{"int to float", "int", "float", true},
		{"int stays int when the target admits int", "int", "int | float", false},

		// path -> string when the target does not include path.
		{"path to string", "path", "string", true},
		{"path stays path when the target admits path", "path", "path | string", false},

		// range_expr -> string, and -> list[int].
		{"range to string", "range_expr", "string", true},
		{"range to list of int", "range_expr", "list[int]", true},
		{"range stays put when the target admits range", "range_expr", "range_expr | string", false},
		{"range does not become a list of strings", "range_expr", "list[string]", false},

		// list[T] -> list[U] elementwise, and the empty list.
		{"list of path to list of string", "list[path]", "list[string]", true},
		{"list of int to list of float", "list[int]", "list[float]", true},
		{"list of nulltype reaches any list", "list[nulltype]", "list[string]", true},
		{"list of int does not reach list of bool", "list[int]", "list[bool]", false},
		{"nested list elementwise", "list[list[int]]", "list[list[float]]", true},

		// Any scalar to a single scalar target.
		{"bool to string", "bool", "string", true},
		{"float to string", "float", "string", true},
		{"string to path", "string", "path", true},
		{"float to int", "float", "int", true},
		{"string to int", "string", "int", true},
		{"string to float", "string", "float", true},

		// A union target with more than one scalar is not a single scalar target.
		{"bool does not reach an ambiguous scalar union", "bool", "int | float", false},

		// A union target with more than one differing list element type is
		// likewise ambiguous: listElem's seen-and-differing guard reports no
		// match, for the same reason singleScalarTarget rejects a union with
		// two differing scalars — an ambiguous target cannot be resolved.
		{"list does not reach an ambiguous list-element union", "list[int]", "list[int] | list[string]", false},

		// Not coercible at all.
		{"scalar does not reach a list", "int", "list[int]", false},
		{"list does not reach a scalar", "list[int]", "int", false},
		{"nulltype does not reach a scalar", "nulltype", "int", false},

		// Unresolved is transparent on both sides: the constraint is what matters.
		{"unresolved source uses its constraint", "unresolved[int]", "float", true},
		{"unresolved target uses its constraint", "int", "unresolved[float]", true},
		{"unresolved on both sides", "unresolved[path]", "unresolved[string]", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			from, err := ParseType(tt.from)
			if err != nil {
				t.Fatalf("ParseType(%q): %v", tt.from, err)
			}
			to, err := ParseType(tt.to)
			if err != nil {
				t.Fatalf("ParseType(%q): %v", tt.to, err)
			}
			if got := coercible(from, to); got != tt.want {
				t.Errorf("coercible(%s, %s) = %v; want %v", from, to, got, tt.want)
			}
		})
	}
}

func TestCoerce(t *testing.T) {
	tests := []struct {
		name   string
		v      Value
		target string
		want   Value
	}{
		{"already the target type", Int(5), "int", Int(5)},
		{"anything to any is unchanged", Int(5), "any", Int(5)},
		{"int to float", Int(5), "float", Float(5)},
		{"int stays int when the target admits int", Int(5), "int | float", Int(5)},
		{"bool to string", Bool(true), "string", String("true")},
		{"int to string", Int(5), "string", String("5")},
		{"float to string", Float(2.5), "string", String("2.5")},
		{"float to string keeps the float rendering", Float(4), "string", String("4.0")},
		{"string to int", String("42"), "int", Int(42)},
		{"negative string to int", String("-42"), "int", Int(-42)},
		{"float to int when exact", Float(4), "int", Int(4)},
		{"negative float to int when exact", Float(-4), "int", Int(-4)},
		{"string to float", String("2.5"), "float", Float(2.5)},
		{"int to float via a union target", Int(3), "float?", Float(3)},
		{"null reaching an optional target is unchanged", Null(), "int?", Null()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, err := ParseType(tt.target)
			if err != nil {
				t.Fatalf("ParseType(%q): %v", tt.target, err)
			}
			got, err := coerce(tt.v, target)
			if err != nil {
				t.Fatalf("coerce(%s, %s): %v", tt.v, target, err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("coerce(%s, %s) = %s (%s); want %s (%s)",
					tt.v, target, got, got.Type, tt.want, tt.want.Type)
			}
		})
	}
}

func TestCoerce_Rejected(t *testing.T) {
	tests := []struct {
		name    string
		v       Value
		target  string
		wantMsg string
	}{
		// Section 1.2.3's own examples of a destructive conversion.
		{"float with a fraction to int", Float(3.75), "int", "cannot be represented"},
		{"empty string to int", String(""), "int", "cannot be represented"},
		{"decimal string to int", String("3.1"), "int", "cannot be represented"},
		{"empty string to float", String(""), "float", "cannot be parsed"},
		{"non-numeric string to float", String("nothing"), "float", "cannot be parsed"},
		// Not coercible at all.
		{"int to bool", Int(1), "bool", "cannot be coerced"},
		{"string to bool", String("true"), "bool", "cannot be coerced"},
		{"null to a scalar", Null(), "int", "cannot be coerced"},
		{"int to a list", Int(1), "list[int]", "cannot be coerced"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, err := ParseType(tt.target)
			if err != nil {
				t.Fatalf("ParseType(%q): %v", tt.target, err)
			}
			_, err = coerce(tt.v, target)
			if err == nil {
				t.Fatalf("coerce(%s, %s) = nil error; want an error", tt.v, target)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %q; want it to contain %q", err.Error(), tt.wantMsg)
			}
		})
	}
}

func TestCoerce_PlaceholderKeepsItsConstraint(t *testing.T) {
	// Coercing a placeholder narrows its constraint rather than producing a
	// value: there is no value to convert. The result is still a placeholder.
	got, err := coerce(Unresolved(TInt), TFloat)
	if err != nil {
		t.Fatalf("coerce: %v", err)
	}
	if !got.IsUnresolved() {
		t.Fatalf("result is %s; want a placeholder", got.Type)
	}
	if want := UnresolvedOf(TFloat); !got.Type.Equal(want) {
		t.Errorf("Type = %s; want %s", got.Type, want)
	}
}

func TestCoerce_ListConversionIsDeferred(t *testing.T) {
	// The three list rules are type-level only in B1: coercible says yes, but
	// performing one must report that it is not implemented rather than silently
	// returning the value unchanged, which would be a wrong value.
	if !coercible(ListOf(TPath), ListOf(TString)) {
		t.Fatal("coercible(list[path], list[string]) = false; want true")
	}
	v := Value{Type: ListOf(TPath)}
	if _, err := coerce(v, ListOf(TString)); err == nil {
		t.Error("coerce of a list = nil error; want a not-implemented error")
	} else if !strings.Contains(err.Error(), "sub-project B2") {
		t.Errorf("error = %q; want it to name sub-project B2", err.Error())
	}
}
