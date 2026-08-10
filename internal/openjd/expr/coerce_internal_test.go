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

		// nulltype is admitted by any target whose union names null — the
		// §1.3.2 "args" shape list[T] | T | nulltype, and coerceUnresolved's
		// route to it when narrowing a placeholder's constraint (bfa4cf3).
		// Positive: a target that names null, either via "?" sugar or a bare
		// nulltype union member, admits it.
		{"nulltype reaches a target that names null via optional sugar", "nulltype", "string?", true},
		{"nulltype reaches a union that names nulltype directly", "nulltype", "string | nulltype", true},
		// Negative boundary: a union target that does NOT name null still
		// rejects it, even though it is a multi-member union (not just the
		// single-scalar case above) — proving the null rule looks at whether
		// null is IN the target, not merely that the target is permissive.
		{"nulltype does not reach a union that does not name null", "nulltype", "int | float", false},

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

func TestCoerce_ListConversionPerformed(t *testing.T) {
	// Sub-project B2 implements the three list rules coercible only judged at
	// the type level before now: this performs the conversion rather than
	// reporting it as not-yet-implemented.
	if !coercible(ListOf(TPath), ListOf(TString)) {
		t.Fatal("coercible(list[path], list[string]) = false; want true")
	}
	v := List(TPath, []Value{{Type: TPath, s: "a"}})
	got, err := coerce(v, ListOf(TString))
	if err != nil {
		t.Fatalf("coerce(list[path], list[string]): %v", err)
	}
	if want := "list[string]"; got.Type.String() != want {
		t.Errorf("Type = %s; want %s", got.Type, want)
	}
}

func TestCoerce_RangeExprToListPerformed(t *testing.T) {
	// The third list rule, range_expr -> list[int], has a scalar SOURCE and a
	// list TARGET — the opposite shape from TestCoerce_ListConversionPerformed
	// above. It must be caught by the dst-listness half of coerce's list
	// check, not just the src-listness half that a list-typed source exercises.
	// Sub-project B2 now performs this conversion rather than deferring it.
	if !coercible(TRangeExpr, ListOf(TInt)) {
		t.Fatal("coercible(range_expr, list[int]) = false; want true")
	}
	v := Value{Type: TRangeExpr, s: "1-5"}
	got, err := coerce(v, ListOf(TInt))
	if err != nil {
		t.Fatalf("coerce(range_expr, list[int]): %v", err)
	}
	if want := "[1, 2, 3, 4, 5]"; got.String() != want {
		t.Errorf("coerce(...) = %s; want %s", got, want)
	}
}

func TestCoerce_Lists(t *testing.T) {
	rng, err := RangeExpr("1-3")
	if err != nil {
		t.Fatalf("RangeExpr: %v", err)
	}
	tests := []struct {
		name    string
		v       Value
		target  string
		want    string // rendered value
		wantTyp string
	}{
		{"identity", List(TInt, []Value{Int(1)}), "list[int]", "[1]", "list[int]"},
		{"elementwise int to float", List(TInt, []Value{Int(1), Int(2)}), "list[float]", "[1.0, 2.0]", "list[float]"},
		{"elementwise to string", List(TInt, []Value{Int(1)}), "list[string]", `["1"]`, "list[string]"},
		{"empty adopts any element type", List(TNull, nil), "list[string]", "[]", "list[string]"},
		{"nested elementwise", List(ListOf(TInt), []Value{List(TInt, []Value{Int(1)})}), "list[list[float]]", "[[1.0]]", "list[list[float]]"},
		{"range_expr to list[int]", rng, "list[int]", "[1, 2, 3]", "list[int]"},
		{"list into an optional list", List(TInt, []Value{Int(1)}), "list[int]?", "[1]", "list[int]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			target, err := ParseType(tc.target)
			if err != nil {
				t.Fatalf("ParseType(%q): %v", tc.target, err)
			}
			got, err := coerce(tc.v, target)
			if err != nil {
				t.Fatalf("coerce(%s, %s): %v", tc.v.Type, tc.target, err)
			}
			if s := got.String(); s != tc.want {
				t.Errorf("coerce(...) = %s, want %s", s, tc.want)
			}
			if s := got.Type.String(); s != tc.wantTyp {
				t.Errorf("coerce(...) type = %s, want %s", s, tc.wantTyp)
			}
		})
	}
}

func TestCoerce_ListErrors(t *testing.T) {
	tests := []struct {
		name   string
		v      Value
		target string
	}{
		{"string elements to int", List(TString, []Value{String("a")}), "list[int]"},
		{"list to a scalar", List(TInt, []Value{Int(1)}), "int"},
		{"scalar to a list", Int(1), "list[int]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			target, err := ParseType(tc.target)
			if err != nil {
				t.Fatalf("ParseType(%q): %v", tc.target, err)
			}
			if _, err := coerce(tc.v, target); err == nil {
				t.Fatalf("coerce(%s, %s) = nil error, want one", tc.v.Type, tc.target)
			}
		})
	}
}

// TestCoercibleMatchesCoerce sweeps every value/target pair B2 touches and
// asserts that the type-level answer and the value-level one agree.
//
// A disagreement is the specific failure the unresolved[T] machinery cannot
// tolerate: coercible says an expression type-checks, evaluation then fails at
// submission time on a value that was promised to fit. The exception is
// deliberately narrow — a conversion that CAN fail on a particular value, like
// string -> int on "abc", is legal for the type and wrong for that value.
func TestCoercibleMatchesCoerce(t *testing.T) {
	rng, err := RangeExpr("1-3")
	if err != nil {
		t.Fatalf("RangeExpr: %v", err)
	}
	values := []Value{
		Int(1),
		Float(1.5),
		String("2"),
		Bool(true),
		Null(),
		rng,
		List(TInt, []Value{Int(1), Int(2)}),
		List(TFloat, []Value{Float(1.5)}),
		List(TString, []Value{String("a")}),
		List(TNull, nil),
		List(ListOf(TInt), []Value{List(TInt, []Value{Int(1)})}),
	}
	targetNames := []string{
		"int", "float", "string", "bool", "path", "range_expr", "nulltype",
		"list[int]", "list[float]", "list[string]", "list[list[int]]",
		"int?", "list[int]?", "int | string", "any",
		// A union MIXING a list type with a scalar one, which every name above
		// misses: each is either all-scalar or all-list, so nothing exercised a
		// scalar value against a list-shaped target. That blind spot is what let
		// coerce() take its list branch on the target alone and panic in
		// AsList() on a plain scalar. "string? | list[string]" is verbatim
		// section 1.3.2's target for a template "args" item, so it is the shape
		// a caller actually builds first.
		"int | list[int]", "string? | list[string]",
		// Two list types with DIFFERING element types, which listElem reports as
		// not list-shaped at all — the union-coercion gap doc.go recorded, where
		// a list value that already IS one of the union's own members was
		// rejected by the target that names it.
		"list[float] | list[int]",
	}
	for _, v := range values {
		for _, name := range targetNames {
			t.Run(v.Type.String()+"->"+name, func(t *testing.T) {
				target, err := ParseType(name)
				if err != nil {
					t.Fatalf("ParseType(%q): %v", name, err)
				}
				// The final disjunct mirrors coerce()'s own direct-membership
				// carve-out (coerce.go's "general applicability check" comment on
				// its includes(target, v.Type.Code) test): a scalar or null value
				// already an unambiguous member of a union target needs no
				// conversion, even where coercible's ambiguity guard would
				// otherwise refuse it. It is restricted to non-list codes because
				// that is exactly what coerce() itself does: its list branch
				// (coerce.go, the "three list rules" block) returns before ever
				// reaching that carve-out, deciding list-vs-list solely through
				// coercible/coercibleList, which is element-type aware.
				// includes() is deliberately NOT element-aware for CodeList
				// (TestIncludes: "list matches by code, not element type"), so
				// applying it to a list-shaped v.Type here would call, e.g.,
				// list[int] -> list[list[int]] "no conversion needed" merely
				// because both share the outer list code — which section 1.2.3
				// never sanctions (its only list rule is "list[T] -> list[U] when
				// each element T can be coerced to U"; there is no rule wrapping a
				// list to satisfy a list of lists). Omitting the restriction is a
				// test-harness bug, not a coercible/coerce disagreement: both
				// production functions already agree the conversion is illegal.
				// directUnionMember is coerce()'s own second carve-out, and the
				// element-aware one: it is what admits a list value that IS one
				// of a union target's members, which includes() above cannot
				// answer for exactly the reason the paragraph above gives.
				canDo := coercible(v.Type, target) || v.Type.Equal(target) ||
					target.Code == CodeAny ||
					(v.Type.Code != CodeList && includes(target, v.Type.Code)) ||
					directUnionMember(target, v.Type)
				got, coerceErr := coerce(v, target)
				switch {
				case canDo && coerceErr != nil:
					// The narrow exception: a conversion legal for the type can
					// still fail on a value that does not fit.
					if !valueMayNotFit(v, target) {
						t.Fatalf("coercible says %s -> %s is legal, but coerce failed: %v",
							v.Type, target, coerceErr)
					}
				case !canDo && coerceErr == nil:
					t.Fatalf("coercible says %s -> %s is illegal, but coerce succeeded",
						v.Type, target)
				case coerceErr == nil:
					// A success is only half-verified by "no error": coerce must
					// also have actually produced a value of the promised type,
					// not merely passed the input through unconverted. This is
					// the other half of the unresolved[T] promise — a type check
					// that passed must be backed by a value that really is what
					// it claims to be, not just one that arrived without error.
					if !resultTypeAdmitted(got, target) {
						t.Fatalf("coerce(%s, %s) succeeded but returned type %s, which %s does not admit",
							v, target, got.Type, target)
					}
				}
			})
		}
	}
}

// resultTypeAdmitted reports whether got's type is one target actually
// admits, once coerce has already reported success. "No error" alone is not
// proof the conversion happened: a coerce() defect that returns its input
// unconverted (a no-op) reports no error and still holds the wrong type, and
// a caller trusting the type check would carry that mistyped value forward
// undetected. This closes that gap by checking what target actually promises:
//
//   - CodeAny admits any type unconverted — coerce()'s own first check.
//   - A union target (int?, list[int]?, int | string, all normalized to
//     CodeUnion) admits exactly its own members; containsType is the package's
//     existing union-membership predicate (type.go, used by
//     normalizeUnionMembers), reused rather than hand-rolled here.
//   - Anything else is a single concrete type, which coerce must have hit
//     exactly.
func resultTypeAdmitted(got Value, target Type) bool {
	switch target.Code {
	case CodeAny:
		return true
	case CodeUnion:
		return containsType(target.Params, got.Type)
	default:
		return got.Type.Equal(target)
	}
}

// valueMayNotFit reports whether the conversion is one section 1.2.3 allows to
// fail on a particular value: a narrowing scalar conversion, which is the only
// legal reason coerce may refuse what coercible permitted.
//
// It dispatches on the SOURCE, exactly as coerce() itself now does: a target
// mixing a list type with a scalar one ("int | list[int]") reports a single
// scalar target AND a list element type, so asking the scalar question first
// would answer for a list value with the wrong rule — "is a list a string or a
// float", trivially no — and shadow the list rule coerce actually applied.
func valueMayNotFit(v Value, target Type) bool {
	if _, srcIsList := listElem(v.Type); !srcIsList {
		if to, ok := singleScalarTarget(target); ok {
			switch to {
			case CodeInt, CodeFloat:
				return v.Type.Code == CodeString || v.Type.Code == CodeFloat
			}
		}
		return false
	}
	// Section 1.2.3's list rule, "list[T] -> list[U] when each element T can be
	// coerced to U", performs the very same per-element scalar conversion the
	// bare-scalar case above already covers — so a list conversion can fail on
	// a value for exactly the reason a scalar one can: an element that is a
	// string/float landing in a list[int]/list[float] target may not fit
	// (float 1.5 -> int, string "a" -> int/float), even though coercible
	// correctly permits the type-level list[T] -> list[U] conversion. This is
	// the same exception as the scalar case, just applied elementwise; it is
	// not a new kind of failure.
	if elemFrom, srcOK := listElem(v.Type); srcOK {
		if elemTo, dstOK := listElem(target); dstOK {
			if to, ok := singleScalarTarget(elemTo); ok {
				switch to {
				case CodeInt, CodeFloat:
					return elemFrom.Code == CodeString || elemFrom.Code == CodeFloat
				}
			}
		}
	}
	return false
}

// TestCoerceUnresolved_DirectUnionMember pins the carve-out coerceUnresolved
// gained in EXPR sub-project E4b's whole-branch review fix: a PLACEHOLDER
// whose constraint a union target already names must coerce exactly as
// readily as a CONCRETE value of that same type does.
//
// Before it, coerceUnresolved consulted only coercible, which is deliberately
// pinned FALSE for a type a target already admits unchanged (see
// directUnionMember's own doc comment) -- so an unresolved placeholder was
// strictly harder to coerce than a real value, and a union with more than one
// scalar member rejected every placeholder outright. That is not a corner:
// every job parameter is a placeholder at template-validation time, and
// section 1.3.12's INT range target
// ("int | string | range_expr | list[int]") is precisely such a union, so
// range: "{{Param.Frames}}" was rejected at upload and accepted at submit.
func TestCoerceUnresolved_DirectUnionMember(t *testing.T) {
	rangeField := UnionOf(TInt, TString, TRangeExpr, ListOf(TInt))

	tests := []struct {
		name       string
		constraint Type
		target     Type
		wantOK     bool
	}{
		{"string constraint, 4-member range union", TString, rangeField, true},
		{"int constraint, 4-member range union", TInt, rangeField, true},
		{"range_expr constraint, 4-member range union", TRangeExpr, rangeField, true},
		{"list[int] constraint, 4-member range union", ListOf(TInt), rangeField, true},
		// Not a member and not coercible into one: bool has no conversion to
		// int, string is ambiguous with two scalar members present, and there
		// is no bool rule at all.
		{"bool constraint, 4-member range union", TBool, rangeField, false},
		{"float constraint, 4-member range union", TFloat, rangeField, false},
		// The narrower, single-scalar unions that already worked keep working
		// through coercible's own catch-all, not through the new carve-out.
		{"string constraint, string? | list[string]", TString, UnionOf(OptionalOf(TString), ListOf(TString)), true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := coerce(Unresolved(tc.constraint), tc.target)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("coerce(unresolved[%s], %s) = %v, want it to succeed", tc.constraint, tc.target, err)
				}
				if !got.IsUnresolved() {
					t.Fatalf("coerce returned %v, want a placeholder", got)
				}
				return
			}
			if err == nil {
				t.Fatalf("coerce(unresolved[%s], %s) = %v, want an error", tc.constraint, tc.target, got)
			}
		})
	}
}

// TestCoerceUnresolved_MatchesConcreteValue is the invariant behind the test
// above, stated directly: for every scalar type and the section 1.3.12 range
// union, a placeholder and a concrete value of the same type must get the
// SAME verdict. The asymmetry is the bug; this is what would catch it coming
// back by any route, not only through coerceUnresolved.
func TestCoerceUnresolved_MatchesConcreteValue(t *testing.T) {
	target := UnionOf(TInt, TString, TRangeExpr, ListOf(TInt))
	rng, err := RangeExpr("1-3")
	if err != nil {
		t.Fatalf("RangeExpr: %v", err)
	}
	concretes := []Value{Int(1), String("1-3"), rng, List(TInt, []Value{Int(1)}), Bool(true), Float(2.5)}

	for _, v := range concretes {
		t.Run(v.Type.String(), func(t *testing.T) {
			_, concreteErr := coerce(v, target)
			_, placeholderErr := coerce(Unresolved(v.Type), target)
			if (concreteErr == nil) != (placeholderErr == nil) {
				t.Fatalf(
					"asymmetry for %s against %s: concrete err = %v, placeholder err = %v",
					v.Type, target, concreteErr, placeholderErr,
				)
			}
		})
	}
}

// TestCoerce_ExportedMatchesInternal pins that the exported Coerce is exactly
// the internal coerce -- it exists to give internal/openjd's range resolver
// section 1.2.3's own range_expr -> list[int] conversion rather than a second
// implementation of it (see Coerce's doc comment), so it must not acquire
// behavior of its own.
func TestCoerce_ExportedMatchesInternal(t *testing.T) {
	rng, err := RangeExpr("10-15:2,1-5")
	if err != nil {
		t.Fatalf("RangeExpr: %v", err)
	}

	got, err := Coerce(rng, ListOf(TInt))
	if err != nil {
		t.Fatalf("Coerce(range_expr, list[int]): %v", err)
	}
	// Section 3.4.1.1.1's increasing, de-duplicated order -- the whole reason
	// the resolver must use this conversion and not internal/openjd's own
	// first-seen <IntRangeExpr> reader.
	want := []int64{1, 2, 3, 4, 5, 10, 12, 14}
	elems := got.AsList()
	if len(elems) != len(want) {
		t.Fatalf("Coerce produced %d elements, want %d: %v", len(elems), len(want), elems)
	}
	for i, w := range want {
		if elems[i].AsInt() != w {
			t.Fatalf("element %d = %d, want %d", i, elems[i].AsInt(), w)
		}
	}

	if _, err := Coerce(Bool(true), TInt); err == nil {
		t.Fatal("Coerce(bool, int) = nil error, want the same refusal coerce gives")
	}
}
