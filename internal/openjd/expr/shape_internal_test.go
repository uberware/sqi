// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "testing"

// noFn is a placeholder implementation for shapes whose Fn is never called,
// which is every shape in this file: these tests are about matching, not
// arithmetic.
func noFn(_ []Value) (Value, error) { return Value{}, nil }

func TestMatchShapes_ExactBeatsCoercion(t *testing.T) {
	// The float shape is listed FIRST so that a shape-by-shape search
	// stopping at the first admissible candidate would coerce the ints and
	// pick it. Ranking every candidate by cost, and taking the cheapest, is
	// what makes 1 + 1 stay an int instead.
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
	// float and the float overload is used". That falls out of cost ranking:
	// the (int, int) shape is inadmissible (float has no lossless route to
	// int), so (float, float) is the only, and therefore cheapest, candidate.
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

// TestMatchShapes_UnionParameterMatchesAMember covers a real signature from
// the spec's own function library, int(value: int | float | string) -> int:
// a union parameter must accept an argument that is exactly one of its
// members, scored at costExact rather than requiring any coercion at all.
// Pitting it against an "any" shape (which always costs costWiden) is what
// makes the exact-cost claim observable through matchShapes rather than by
// reaching into argCost directly.
func TestMatchShapes_UnionParameterMatchesAMember(t *testing.T) {
	shapes := []Shape{
		{Params: []Type{UnionOf(TInt, TFloat, TString)}, Ret: TInt, Fn: noFn},
		{Params: []Type{TAny}, Ret: TBool, Fn: noFn},
	}
	tests := []struct {
		name string
		arg  Type
	}{
		{"an exact int member", TInt},
		{"an exact float member", TFloat},
		{"an exact string member", TString},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, ok := matchShapes(shapes, []Type{tt.arg})
			if !ok {
				t.Fatal("no shape matched")
			}
			if !got.Ret.Equal(TInt) {
				t.Errorf("winner Ret = %s; want int — an exact member costs costExact and "+
					"must beat the any shape's costWiden", got.Ret)
			}
		})
	}
}

// TestMatchShapes_UnionParameterWidensToAMember checks that a union
// parameter accepts an argument that reaches one of its members by a
// PROMOTABLE conversion — one of the spec's own named compatible pairs — the
// same way a bare parameter of that member's type would.
//
// path is such a case: path -> string is the int -> float promotion's own
// sibling, coercibleConditional's named rule, and it cannot fail on any value,
// so it must still match here even though the union offers three members —
// only the string member is reachable, and it is reachable by a rule the spec
// names explicitly.
//
// bool is the pair this test used to get wrong: it has no promotable path to
// any of int, float or string. Its only route to string is section 1.2.3's
// single-scalar catch-all (bool/int/float/path -> string, unconditionally),
// and that catch-all is deliberately excluded from overload selection — see
// promotable's doc comment in coerce.go. Asserting bool does NOT match here is
// what pins that exclusion at the point selection actually happens, rather
// than only in promotable's own unit tests.
func TestMatchShapes_UnionParameterWidensToAMember(t *testing.T) {
	shapes := []Shape{{Params: []Type{UnionOf(TInt, TFloat, TString)}, Ret: TInt, Fn: noFn}}
	if _, _, ok := matchShapes(shapes, []Type{TPath}); !ok {
		t.Error("path did not match a union offering string, via the named path -> string promotion")
	}
	if _, _, ok := matchShapes(shapes, []Type{TBool}); ok {
		t.Error("bool matched a union offering string; its only route is the catch-all, " +
			"which overload selection must not use")
	}
}

// TestMatchShapes_UnionParameterRejectsAMismatchedList pins the case a
// membership test on type codes alone would get wrong: a union containing
// list[int] must not accept list[string], even though "list" is among the
// union's member codes — the element types differ and list[string] cannot
// losslessly become list[int] (a string element can fail to parse).
func TestMatchShapes_UnionParameterRejectsAMismatchedList(t *testing.T) {
	shapes := []Shape{{Params: []Type{UnionOf(TInt, ListOf(TInt))}, Ret: TBool, Fn: noFn}}
	if _, _, ok := matchShapes(shapes, []Type{ListOf(TString)}); ok {
		t.Error("list[string] matched a union containing list[int]; want no match")
	}
}

// TestMatchShapes_EmptyListArgumentMatchesAnyListParameter covers a seam
// between argCost and promotable: promotable(list[nulltype], list[int]) is
// true per section 1.2.3's empty-list rule, but argCost's list-descent
// branch used to recurse into the element types BEFORE promotable was ever
// consulted, so it never saw that rule and rejected list[nulltype] outright.
// Inert while B1 registers no list-typed shapes, but the first one B2 adds
// would otherwise reject "[]" against any list parameter.
func TestMatchShapes_EmptyListArgumentMatchesAnyListParameter(t *testing.T) {
	shapes := []Shape{{Params: []Type{ListOf(TInt)}, Ret: TBool, Fn: noFn}}
	if _, _, ok := matchShapes(shapes, []Type{ListOf(TNull)}); !ok {
		t.Error("list[nulltype] did not match a list[int] parameter; want the empty-list rule to admit it")
	}
}

// TestMatchShapes_UnionArgumentRequiresEveryMemberAdmissible covers the
// argument-side dual of the union-PARAMETER tests above: a union-typed
// ARGUMENT is admissible only where every one of its members is, since the
// union could BE any one of them at runtime. __pow__'s own declared return,
// int | float, is exactly this shape once it feeds into another operator —
// the case the B1 review found unusable end to end.
func TestMatchShapes_UnionArgumentRequiresEveryMemberAdmissible(t *testing.T) {
	shapes := []Shape{{Params: []Type{TFloat, TFloat}, Ret: TFloat, Fn: noFn}}
	if _, _, ok := matchShapes(shapes, []Type{UnionOf(TInt, TFloat), TFloat}); !ok {
		t.Error("union(int, float) did not match (float, float); both members have a lossless route to float")
	}
	if _, _, ok := matchShapes(shapes, []Type{UnionOf(TInt, TString), TFloat}); ok {
		t.Error("union(int, string) matched (float, float); string has no lossless route to float")
	}
}

// TestMatchShapes_UnionArgumentBindingConflictIsInadmissible covers the
// bindings hazard a union argument introduces that a union parameter does
// not: list[T] against a union of list[int] and list[string] would bind T to
// int for one member and string for the other. Neither binding is correct
// regardless of which member the value turns out to be at runtime, so the
// whole argument must be rejected rather than arbitrarily keeping one
// member's binding.
func TestMatchShapes_UnionArgumentBindingConflictIsInadmissible(t *testing.T) {
	shapes := []Shape{{
		Params: []Type{ListOf(Type{Code: CodeVarT})},
		Ret:    Type{Code: CodeVarT},
		Fn:     noFn,
	}}
	arg := UnionOf(ListOf(TInt), ListOf(TString))
	if _, _, ok := matchShapes(shapes, []Type{arg}); ok {
		t.Error("a union argument with conflicting type-variable bindings matched; want no match")
	}
}

// TestOrderingAdmissibility pins section 2.1.4's restriction that ordering
// operands may cross type only between int/float and string/path. Sub-project
// B1 documented a divergence here: promotable was global, so section 1.2.3's
// range_expr -> string coercion leaked into ordering's (string, string) shape.
func TestOrderingAdmissibility(t *testing.T) {
	rng, err := RangeExpr("1-3")
	if err != nil {
		t.Fatalf("RangeExpr: %v", err)
	}
	syms := MapSymbols{
		"Param.Range": rng,
		"Param.Dir":   Value{Type: TPath, s: "/a"},
	}
	rejected := []string{
		"Param.Range < 'a'",
		"'a' < Param.Range",
		"Param.Range > 'a'",
		"Param.Range <= 'a'",
		"Param.Range >= 'a'",
	}
	for _, src := range rejected {
		t.Run("rejected/"+src, func(t *testing.T) {
			if _, err := Eval(src, syms, TAny); err == nil {
				t.Fatalf("Eval(%q) = nil error, want ordering to reject a range_expr", src)
			}
		})
	}
	accepted := []struct {
		src  string
		want string
	}{
		{"1 < 2.5", "true"},               // int/float, a named compatible pair
		{"Param.Dir < '/b'", "true"},      // string/path, a named compatible pair
		{"'a' + Param.Range", "a1-3"},     // section 2.1.2 gives + an explicit row
		{"Param.Range == '1-3'", "false"}, // equality is total and never rejects
		{"1 < 2 < 3", "true"},             // chains still work
	}
	for _, tc := range accepted {
		t.Run("accepted/"+tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, syms, TAny)
			if err != nil {
				t.Fatalf("Eval(%q): %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Fatalf("Eval(%q) = %s, want %s", tc.src, got, tc.want)
			}
		})
	}
}

// TestPromotableUnder_DefaultMatchesPromotable pins promoteDefault's whole
// reason for existing as the promotion zero value: every OTHER operator in
// the language relies on it being byte-identical to the old, global
// promotable predicate, and the shapeCost/argCost call chain now threads a
// promotion through every one of them. Reasoning about the two call sites
// staying in lock-step is exactly what sub-project B1 got wrong once already
// (the very divergence this task closes); this asserts it across the
// conversions promotable actually recognizes, including the list and
// unresolved-wrapper cases where the two functions could diverge silently.
func TestPromotableUnder_DefaultMatchesPromotable(t *testing.T) {
	pairs := []struct {
		from, to Type
	}{
		{TInt, TFloat},
		{TFloat, TInt},
		{TPath, TString},
		{TString, TPath},
		{TRangeExpr, TString},
		{TString, TRangeExpr},
		{TRangeExpr, ListOf(TInt)},
		{TRangeExpr, TFloat},
		{TInt, TString},
		{TBool, TInt},
		{ListOf(TInt), ListOf(TFloat)},
		{ListOf(TFloat), ListOf(TInt)},
		{ListOf(TNull), ListOf(TString)},
		{ListOf(TPath), ListOf(TString)},
		{UnresolvedOf(TInt), TFloat},
		{TInt, UnresolvedOf(TFloat)},
		{TInt, TInt},
	}
	for _, p := range pairs {
		t.Run(p.from.String()+"->"+p.to.String(), func(t *testing.T) {
			want := promotable(p.from, p.to)
			got := promotableUnder(promoteDefault, p.from, p.to)
			if got != want {
				t.Errorf("promotableUnder(promoteDefault, %s, %s) = %v, want %v (promotable)",
					p.from, p.to, got, want)
			}
		})
	}
}
