// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "testing"

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
