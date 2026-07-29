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

		// A union SOURCE is coercible only where every member is: it is some
		// one of its members, decided at runtime, so a target that would
		// reject any one of them cannot safely receive it.
		{"every union member reaches the target", "int | float", "float", true},
		// path has no route to float at all (not even through the permissive
		// catch-all: scalarCoercible admits only int/string into float), so
		// adding it as a member sinks the whole union.
		{"a union member with no route to the target sinks it", "int | path", "float", false},
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

// TestPromotable covers promotable, the narrower subset of section 1.2.3 that
// overload selection uses (shape.go's argCost): only the compatible pairs the
// spec names explicitly — int -> float, path -> string, range_expr ->
// string/list[int] — plus the two defects the two-tier tests below found in
// the first cut of this function (then named losslesslyCoercible): an
// unresolved constraint on either side must be unwrapped before the list/
// conditional checks can see it, and a list conversion is promotable only when
// its element conversion is — not simply because coercible permits it.
//
// The single-scalar catch-all (bool/int/float/path -> string, string -> path,
// and the rest) is deliberately EXCLUDED here even though coerce() honors it:
// that catch-all answers "what may this value become at an explicit target",
// not "which overload should the language pick on the caller's behalf". Firing
// it during overload selection let every scalar pair reach a bare-string
// shape, which is why "string to path widens" below flipped from true to
// false — string -> path is the catch-all's doing, not one of the four named
// compatible pairs, so it no longer promotes.
func TestPromotable(t *testing.T) {
	tests := []struct {
		name string
		from string // parsed with ParseType, so the table reads as the spec does
		to   string
		want bool
	}{
		// The already-passing cases, kept here so a future change to the
		// unresolved/list branches is checked against the whole picture, not
		// just the two defects below.
		{"int to float widens", "int", "float", true},
		{"float to int can fail", "float", "int", false},
		{"string to int can fail", "string", "int", false},
		{"string to float can fail", "string", "float", false},
		{"path to string widens", "path", "string", true},
		// Not a named compatible pair (only path -> string is), and it is the
		// single-scalar catch-all that would otherwise say yes — the exact
		// mechanism this function exists to exclude from overload selection.
		{"string to path is the catch-all, not a compatible pair", "string", "path", false},

		// F2: unresolved must unwrap to its constraint on both sides before
		// the scalar/non-scalar branch runs, exactly like coercible does.
		{"unresolved float to int is still lossy", "unresolved[float]", "int", false},
		{"unresolved int to float still widens", "unresolved[int]", "float", true},
		{"int to unresolved float still widens", "int", "unresolved[float]", true},
		{"float to unresolved int is still lossy", "float", "unresolved[int]", false},

		// F3: a list conversion is only as lossless as its element
		// conversion — coercible merely says the conversion is PERMITTED,
		// not that it cannot fail on some value.
		{"list of float to list of int can fail elementwise", "list[float]", "list[int]", false},
		{"list of int to list of float widens elementwise", "list[int]", "list[float]", true},
		{"list of string to list of int can fail elementwise", "list[string]", "list[int]", false},
		// The empty-list literal's type has no element that could ever fail
		// to convert, so it stays lossless into any list type.
		{"list of nulltype reaches any list losslessly", "list[nulltype]", "list[int]", true},
		// A nested list is elementwise too: the inner list[float] -> list[int]
		// step is what makes the whole thing lossy.
		{"nested list is lossy exactly when its element is", "list[list[float]]", "list[list[int]]", false},
		{"nested list widens when its element does", "list[list[int]]", "list[list[float]]", true},

		// A union SOURCE promotes only where every member does — same rule as
		// coercible's union branch, but scored by promotable per member since
		// this function answers the narrower, lossless question. int -> float
		// is a named compatible pair, so union(int, float) -> float is
		// lossless regardless of which member shows up; string -> float can
		// fail (it is coercible's permissive catch-all, not a named
		// compatible pair), so adding a string member sinks it.
		{"union of a widening member and the target itself promotes", "int | float", "float", true},
		{"a union member with only a lossy route sinks it", "int | string", "float", false},
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
			if got := promotable(from, to); got != tt.want {
				t.Errorf("promotable(%s, %s) = %v; want %v", from, to, got, tt.want)
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
		// Regression: coerceScalar's switch has a case CodePath that calls
		// v.AsStr(), which panics on a non-string payload. targetScalarCode's
		// catch-all alone would report "path" reachable from any scalar,
		// since path is the target's single admitted scalar; only the
		// applicability gate ahead of coerceScalar stops these before they
		// reach AsStr().
		{"bool to path panics without the gate", Bool(true), "path", "cannot be coerced"},
		{"int to path panics without the gate", Int(5), "path", "cannot be coerced"},
		{"float to path panics without the gate", Float(1.5), "path", "cannot be coerced"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A panic inside coerce would otherwise crash the whole test
			// binary rather than just failing this subtest, silently losing
			// the very regression these cases exist to catch. Turn it into a
			// clean, attributable failure instead.
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("coerce(%s, %s) panicked: %v", tt.v, tt.target, r)
				}
			}()
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

func TestCoerce_RangeExprToListIsDeferred(t *testing.T) {
	// The third list rule, range_expr -> list[int], has a scalar SOURCE and a
	// list TARGET — the opposite shape from TestCoerce_ListConversionIsDeferred
	// above. It must be caught by the dst-listness half of coerce's list
	// check, not just the src-listness half that a list-typed source exercises.
	if !coercible(TRangeExpr, ListOf(TInt)) {
		t.Fatal("coercible(range_expr, list[int]) = false; want true")
	}
	v := Value{Type: TRangeExpr, s: "1-5"}
	if _, err := coerce(v, ListOf(TInt)); err == nil {
		t.Error("coerce of a range_expr to list[int] = nil error; want a not-implemented error")
	} else if !strings.Contains(err.Error(), "sub-project B2") {
		t.Errorf("error = %q; want it to name sub-project B2", err.Error())
	}
}
