// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"strings"
	"testing"
)

func TestType_Equal(t *testing.T) {
	tests := []struct {
		name string
		a, b Type
		want bool
	}{
		{"same scalar", TInt, TInt, true},
		{"different scalar", TInt, TFloat, false},
		{"scalar against a parameterized type", TInt, Type{Code: CodeList, Params: []Type{TInt}}, false},
		{
			name: "same element type",
			a:    Type{Code: CodeList, Params: []Type{TInt}},
			b:    Type{Code: CodeList, Params: []Type{TInt}},
			want: true,
		},
		{
			name: "different element type",
			a:    Type{Code: CodeList, Params: []Type{TInt}},
			b:    Type{Code: CodeList, Params: []Type{TString}},
			want: false,
		},
		{
			name: "nested element type",
			a:    Type{Code: CodeList, Params: []Type{{Code: CodeList, Params: []Type{TInt}}}},
			b:    Type{Code: CodeList, Params: []Type{{Code: CodeList, Params: []Type{TInt}}}},
			want: true,
		},
		{
			name: "same code but different parameter count",
			a:    Type{Code: CodeUnion, Params: []Type{TInt, TString}},
			b:    Type{Code: CodeUnion, Params: []Type{TInt}},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.Equal(tt.b); got != tt.want {
				t.Errorf("%v.Equal(%v) = %v; want %v", tt.a.Code, tt.b.Code, got, tt.want)
			}
			if got := tt.b.Equal(tt.a); got != tt.want {
				t.Errorf("Equal is not symmetric for %v and %v", tt.a.Code, tt.b.Code)
			}
		})
	}
}

func TestCode_String(t *testing.T) {
	// Every code needs a name: the names appear in "unsupported operand types"
	// messages, so a missing one renders a defect as "unknown type".
	codes := []Code{
		CodeNull, CodeBool, CodeInt, CodeFloat, CodeString, CodePath,
		CodeRangeExpr, CodeList, CodeUnion, CodeUnresolved, CodeAny,
		CodeNoReturn, CodeVarT, CodeVarT1, CodeVarT2, CodeVarT3,
	}
	for _, c := range codes {
		if _, ok := codeNames[c]; !ok {
			t.Errorf("code %d has no name in codeNames", int(c))
		}
	}
	if len(codeNames) != len(codes) {
		t.Errorf("codeNames has %d entries; want %d — a code was added without a test case", len(codeNames), len(codes))
	}
}

func TestType_String(t *testing.T) {
	tests := []struct {
		name string
		typ  Type
		want string
	}{
		{"scalar", TInt, "int"},
		{"null", TNull, "nulltype"},
		{"any", TAny, "any"},
		{"noreturn", TNoReturn, "noreturn"},
		{"range expression", TRangeExpr, "range_expr"},
		{"list", Type{Code: CodeList, Params: []Type{TString}}, "list[string]"},
		{
			name: "nested list",
			typ:  Type{Code: CodeList, Params: []Type{{Code: CodeList, Params: []Type{TInt}}}},
			want: "list[list[int]]",
		},
		{
			name: "two member union renders as a union",
			typ:  Type{Code: CodeUnion, Params: []Type{TInt, TString}},
			want: "int | string",
		},
		{
			name: "optional is the two member union ending in nulltype",
			typ:  Type{Code: CodeUnion, Params: []Type{TInt, TNull}},
			want: "int?",
		},
		{
			name: "optional list",
			typ:  Type{Code: CodeUnion, Params: []Type{{Code: CodeList, Params: []Type{TPath}}, TNull}},
			want: "list[path]?",
		},
		{
			name: "three member union with null spells nulltype out",
			typ:  Type{Code: CodeUnion, Params: []Type{TInt, TString, TNull}},
			want: "int | string | nulltype",
		},
		{"unresolved with a constraint", Type{Code: CodeUnresolved, Params: []Type{TInt}}, "unresolved[int]"},
		{"unresolved of any is bare", Type{Code: CodeUnresolved, Params: []Type{TAny}}, "unresolved"},
		{
			name: "unresolved list",
			typ:  Type{Code: CodeUnresolved, Params: []Type{{Code: CodeList, Params: []Type{TInt}}}},
			want: "unresolved[list[int]]",
		},
		{"type variable", Type{Code: CodeVarT}, "T"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.typ.String(); got != tt.want {
				t.Errorf("String() = %q; want %q", got, tt.want)
			}
		})
	}
}

func TestUnionOf(t *testing.T) {
	tests := []struct {
		name string
		in   []Type
		want string // the rendered canonical form
	}{
		{"single member unwraps", []Type{TInt}, "int"},
		{"two members sort alphabetically", []Type{TString, TInt}, "int | string"},
		{"already sorted stays put", []Type{TInt, TString}, "int | string"},
		{"duplicates collapse", []Type{TInt, TInt, TString}, "int | string"},
		{"nulltype sorts last", []Type{TNull, TInt}, "int?"},
		{"nulltype sorts last among several", []Type{TNull, TString, TInt}, "int | string | nulltype"},
		{"any absorbs", []Type{TInt, TAny}, "any"},
		{"any absorbs when it comes first", []Type{TAny, TInt}, "any"},
		{"noreturn collapses", []Type{TInt, TNoReturn}, "int"},
		{"noreturn alone is noreturn", []Type{TNoReturn}, "noreturn"},
		{"nothing is noreturn", nil, "noreturn"},
		{
			name: "nested unions flatten",
			in:   []Type{{Code: CodeUnion, Params: []Type{TInt, TString}}, TBool},
			want: "bool | int | string",
		},
		{
			name: "list members are ordered by their rendering",
			in:   []Type{ListOf(TString), ListOf(TInt)},
			want: "list[int] | list[string]",
		},
		{
			name: "an unresolved member hoists out of the union",
			in:   []Type{TInt, UnresolvedOf(TString)},
			want: "unresolved[int | string]",
		},
		{
			name: "two unresolved members merge their constraints",
			in:   []Type{UnresolvedOf(TInt), UnresolvedOf(TString)},
			want: "unresolved[int | string]",
		},
		{
			name: "an unresolved member survives any absorbing",
			in:   []Type{TAny, UnresolvedOf(TInt)},
			want: "unresolved",
		},
		{
			name: "an unresolved single member unwraps to unresolved",
			in:   []Type{UnresolvedOf(TInt)},
			want: "unresolved[int]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := UnionOf(tt.in...).String(); got != tt.want {
				t.Errorf("UnionOf(...) = %q; want %q", got, tt.want)
			}
		})
	}
}

func TestUnionOf_IsIdempotent(t *testing.T) {
	// Normalizing an already-normalized union must not change it. Everything
	// downstream compares types structurally, so a constructor that produced a
	// different shape on a second pass would make Equal unreliable.
	inputs := []Type{
		UnionOf(TInt, TString),
		UnionOf(TInt, TNull),
		UnionOf(TInt, TString, TNull),
		UnionOf(TInt, UnresolvedOf(TString)),
	}
	for _, in := range inputs {
		t.Run(in.String(), func(t *testing.T) {
			if got := UnionOf(in); !got.Equal(in) {
				t.Errorf("UnionOf(%s) = %s; want it unchanged", in, got)
			}
		})
	}
}

func TestUnresolvedOf(t *testing.T) {
	tests := []struct {
		name string
		in   Type
		want string
	}{
		{"wraps a scalar", TInt, "unresolved[int]"},
		{"wraps any as the bare shorthand", TAny, "unresolved"},
		{"nested unresolved flattens", UnresolvedOf(TInt), "unresolved[int]"},
		{"double nesting flattens to one", UnresolvedOf(UnresolvedOf(TString)), "unresolved[string]"},
		{"wraps a union", UnionOf(TInt, TString), "unresolved[int | string]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := UnresolvedOf(tt.in).String(); got != tt.want {
				t.Errorf("UnresolvedOf(%s) = %q; want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestListOf(t *testing.T) {
	tests := []struct {
		name string
		elem Type
		want string
	}{
		{"list of a scalar", TInt, "list[int]"},
		{"list of a list", ListOf(TInt), "list[list[int]]"},
		{"list of a union", UnionOf(TInt, TString), "list[int | string]"},
		{"list of nulltype is the empty list type", TNull, "list[nulltype]"},
		// The hoisting rule: a list of unresolved elements is an unresolved list.
		{"unresolved element hoists out", UnresolvedOf(TInt), "unresolved[list[int]]"},
		{"unresolved hoists out of a nested list too", ListOf(UnresolvedOf(TInt)), "unresolved[list[list[int]]]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ListOf(tt.elem).String(); got != tt.want {
				t.Errorf("ListOf(%s) = %q; want %q", tt.elem, got, tt.want)
			}
		})
	}
}

func TestOptionalOf(t *testing.T) {
	tests := []struct {
		name string
		in   Type
		want string
	}{
		{"scalar becomes optional", TInt, "int?"},
		{"already optional is unchanged", UnionOf(TInt, TNull), "int?"},
		{"nulltype alone stays nulltype", TNull, "nulltype"},
		{"list becomes optional", ListOf(TPath), "list[path]?"},
		{"union gains nulltype", UnionOf(TInt, TString), "int | string | nulltype"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := OptionalOf(tt.in).String(); got != tt.want {
				t.Errorf("OptionalOf(%s) = %q; want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestUnresolvedIsAlwaysOutermost(t *testing.T) {
	// The canonical form the ExprType section requires: unresolved is either
	// absent or wraps the whole type. It never appears inside a list's element
	// type or a union's members. Later tasks rely on being able to test for it
	// by looking at the outermost code alone.
	built := []Type{
		ListOf(UnresolvedOf(TInt)),
		UnionOf(TInt, UnresolvedOf(TString)),
		UnionOf(UnresolvedOf(TInt), UnresolvedOf(TString)),
		OptionalOf(UnresolvedOf(TInt)),
		ListOf(ListOf(UnresolvedOf(TFloat))),
	}
	for _, typ := range built {
		t.Run(typ.String(), func(t *testing.T) {
			if typ.Code != CodeUnresolved {
				t.Fatalf("%s: outermost code is %v; want CodeUnresolved", typ, typ.Code)
			}
			forEachNestedType(typ.Params[0], func(inner Type) {
				if inner.Code == CodeUnresolved {
					t.Errorf("%s: unresolved appears nested inside the constraint", typ)
				}
			})
		})
	}
}

// forEachNestedType calls fn on t and every type nested inside it.
func forEachNestedType(t Type, fn func(Type)) {
	fn(t)
	for _, p := range t.Params {
		forEachNestedType(p, fn)
	}
}

func TestParseType(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string // rendered canonical form, so normalization is checked too
	}{
		{"scalar", "int", "int"},
		{"null", "nulltype", "nulltype"},
		{"path", "path", "path"},
		{"range expression", "range_expr", "range_expr"},
		{"any", "any", "any"},
		{"noreturn", "noreturn", "noreturn"},
		{"type variable", "T", "T"},
		{"numbered type variable", "T2", "T2"},
		{"list", "list[string]", "list[string]"},
		{"nested list", "list[list[int]]", "list[list[int]]"},
		{"empty list type", "list[nulltype]", "list[nulltype]"},
		{"optional", "int?", "int?"},
		{"optional list", "list[path]?", "list[path]?"},
		{"union", "int | string", "int | string"},
		{"union normalizes on the way in", "string | int", "int | string"},
		{"union with a null member", "int | nulltype", "int?"},
		{"unresolved", "unresolved", "unresolved"},
		{"unresolved with a constraint", "unresolved[int]", "unresolved[int]"},
		{"unresolved of a union", "unresolved[int | string]", "unresolved[int | string]"},
		{"unresolved hoists out of a list", "list[unresolved[int]]", "unresolved[list[int]]"},
		{"whitespace is insignificant", "  list [ int ]  ", "list[int]"},
		{"question mark binds tighter than the bar", "int | string?", "int | string | nulltype"},
		{"redundant optional collapses", "int??", "int?"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseType(tt.src)
			if err != nil {
				t.Fatalf("ParseType(%q): %v", tt.src, err)
			}
			if got.String() != tt.want {
				t.Errorf("ParseType(%q) = %q; want %q", tt.src, got.String(), tt.want)
			}
		})
	}
}

func TestParseType_RoundTrips(t *testing.T) {
	// String and ParseType are inverses. This is the property the symbol table
	// and the test tables both lean on, and it is the cheapest way to catch a
	// rendering or parsing rule that drifted.
	for _, src := range []string{
		"int", "nulltype", "any", "noreturn", "path", "range_expr", "T", "T1",
		"list[int]", "list[list[string]]", "list[nulltype]",
		"int?", "list[path]?", "int | string", "int | string | nulltype",
		"unresolved", "unresolved[int]", "unresolved[list[float]]", "unresolved[int | string]",
	} {
		t.Run(src, func(t *testing.T) {
			typ, err := ParseType(src)
			if err != nil {
				t.Fatalf("ParseType(%q): %v", src, err)
			}
			if got := typ.String(); got != src {
				t.Errorf("round trip: ParseType(%q).String() = %q", src, got)
			}
			again, err := ParseType(typ.String())
			if err != nil {
				t.Fatalf("re-parsing %q: %v", typ.String(), err)
			}
			if !again.Equal(typ) {
				t.Errorf("re-parsing %q produced a different type", src)
			}
		})
	}
}

func TestParseType_Rejected(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantMsg string
	}{
		{"empty", "", "expected a type"},
		{"unknown name", "integer", "unknown type"},
		{"unclosed bracket", "list[int", `expected "]"`},
		{"list without an element type", "list", `expected "["`},
		{"empty brackets", "list[]", "expected a type"},
		{"trailing text", "int extra", "unexpected"},
		{"trailing bar", "int |", "expected a type"},
		{"leading bar", "| int", "expected a type"},
		// Section 1.2.1's two constraints on a list's element type.
		{"three levels of list", "list[list[list[int]]]", "nested"},
		{"optional element", "list[int?]", "optional"},
		{"optional element written as a union", "list[int | nulltype]", "optional"},
		// Regression: ListOf hoists an outer "unresolved" wrapper on the element
		// outward, so checkListElem must see through it — an element written as
		// unresolved[...] must not smuggle a forbidden shape past either check.
		{"optional element hidden behind unresolved", "list[unresolved[int]?]", "optional"},
		{"three levels of list hidden behind unresolved", "list[unresolved[list[list[int]]]]", "nested"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseType(tt.src)
			if err == nil {
				t.Fatalf("ParseType(%q) = nil error; want an error", tt.src)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %q; want it to contain %q", err.Error(), tt.wantMsg)
			}
		})
	}
}

// TestType_String_DefensivePaths exercises the two rendering paths that no
// constructor can reach: paramString's "?" fallback for a Type missing its
// Params, and Code.String's "unknown type" fallback for a code outside
// codeNames. Both are unreachable through UnionOf/ListOf/UnresolvedOf, which
// is why this test builds the Type literals directly. It is a regression
// guard, not a test of reachable behavior — a reordered bounds check could
// silently turn either fallback into a panic, and nothing else in this file
// would catch it.
func TestType_String_DefensivePaths(t *testing.T) {
	if got := (Type{Code: CodeList}).String(); got != "list[?]" {
		t.Errorf("list with no Params = %q; want %q", got, "list[?]")
	}
	if got := (Type{Code: CodeUnresolved}).String(); got != "unresolved[?]" {
		t.Errorf("unresolved with no Params = %q; want %q", got, "unresolved[?]")
	}
	if got := (Type{Code: Code(9999)}).String(); got != "unknown type" {
		t.Errorf("unknown code = %q; want %q", got, "unknown type")
	}
}

// TestParseType_AcceptedFormsReparse exists because a validation bypass once
// let ParseType accept input whose own canonical rendering it then rejected:
// checkListElem ran before ListOf hoisted an outer "unresolved" wrapper on a
// list's element outward, so an element written as unresolved[...] could hide
// an optional or a third level of list nesting from both checks. Rather than
// pinning just the two instances that surfaced that bug, this asserts the
// general property every accepted form must have: if ParseType accepts a
// string, ParseType must also accept that Type's own rendering, and get back
// an equal Type.
func TestParseType_AcceptedFormsReparse(t *testing.T) {
	srcs := []string{
		"int", "nulltype", "any", "noreturn", "path", "range_expr", "T", "T1",
		"list[int]", "list[list[string]]", "list[nulltype]",
		"int?", "list[path]?", "int | string", "int | string | nulltype",
		"unresolved", "unresolved[int]", "unresolved[list[float]]", "unresolved[int | string]",
		"list[unresolved[int]?]", "list[unresolved[list[list[int]]]]",
	}
	for _, src := range srcs {
		t.Run(src, func(t *testing.T) {
			typ, err := ParseType(src)
			if err != nil {
				// Not every string in this list is expected to be accepted by
				// ParseType directly (the two regression srcs above are
				// rejected, as TestParseType_Rejected asserts) — but every
				// string that IS accepted must satisfy the reparse property
				// below, so skip here and let TestParseType_Rejected own the
				// rejection assertion.
				return
			}
			rendered := typ.String()
			again, err := ParseType(rendered)
			if err != nil {
				t.Fatalf("ParseType(%q) succeeded, but re-parsing its own rendering %q failed: %v", src, rendered, err)
			}
			if !again.Equal(typ) {
				t.Errorf("ParseType(%q) succeeded, but re-parsing its own rendering %q produced a different type", src, rendered)
			}
		})
	}
}
