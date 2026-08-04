// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "testing"

func TestMergeFuncs_CombinesGroups(t *testing.T) {
	a := map[string][]Shape{"one": {{Params: []Type{TInt}, Ret: TInt}}}
	b := map[string][]Shape{"two": {{Params: []Type{TString}, Ret: TInt}}}
	got := mergeFuncs(a, b)
	if len(got) != 2 {
		t.Fatalf("mergeFuncs gave %d entries, want 2", len(got))
	}
	for _, name := range []string{"one", "two"} {
		if _, ok := got[name]; !ok {
			t.Errorf("mergeFuncs dropped %q", name)
		}
	}
}

// TestFunctionShapes_RegistersEveryShippedName pins the registry's actual
// contents, so that an entry dropped from a group — or a group that quietly
// stopped being passed to mergeFuncs in funcs.go — fails here rather than
// only in whichever per-group test happens to exercise that one name.
//
// Renamed from TestFunctionShapes_RegistersC1sTwentyTwoNames when C2 began
// adding to the table; the count belongs in the list below, not in the name.
func TestFunctionShapes_RegistersEveryShippedName(t *testing.T) {
	want := []string{
		// funcsconv.go: general conversions, plus fail (validation).
		"len", "bool", "int", "float", "string", "list", "range_expr", "fail",
		// funcsmath.go: math.
		"round", "abs", "floor", "ceil", "min", "max", "sum",
		// funcslist.go: list functions.
		"range", "flatten", "sorted", "reversed", "unique", "any", "all",
		// funcsstrcase.go: string case transforms and classification.
		"upper", "lower", "capitalize", "title",
		"isdigit", "isalpha", "isalnum", "isspace", "isupper", "islower", "isascii",
		// funcsstrfind.go: trim and affix.
		"strip", "lstrip", "rstrip", "removeprefix", "removesuffix",
		// funcsstrfind.go: search and replace.
		"startswith", "endswith", "count", "find", "rfind", "index", "rindex", "replace",
	}
	if len(functionShapes) != len(want) {
		t.Fatalf("functionShapes has %d entries, want %d: %v", len(functionShapes), len(want), want)
	}
	for _, name := range want {
		t.Run(name, func(t *testing.T) {
			if _, ok := functionShapes[name]; !ok {
				t.Errorf("functionShapes is missing %q", name)
			}
		})
	}
}

func TestMergeFuncs_PanicsOnDuplicateName(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("mergeFuncs accepted a duplicate name; want a panic")
		}
	}()
	a := map[string][]Shape{"dup": {{Params: []Type{TInt}, Ret: TInt}}}
	b := map[string][]Shape{"dup": {{Params: []Type{TString}, Ret: TInt}}}
	mergeFuncs(a, b)
}
