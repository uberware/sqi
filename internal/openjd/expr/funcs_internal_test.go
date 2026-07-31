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

// TestFunctionShapes_RegistersC1sTwentyTwoNames pins the registry's actual
// contents. TestFunctionShapes_IsEmpty (pre-C1) asserted the registry had
// nothing in it yet; it was correctly deleted once C1 populated the table,
// but nothing took its place, so a future edit that dropped an entry from
// convFuncs, mathFuncs or listFuncs — or a group that quietly stopped being
// passed to mergeFuncs in funcs.go — would only be caught by whichever
// per-group tests happen to exercise that one name, not by anything asserting
// the registry as a whole.
func TestFunctionShapes_RegistersC1sTwentyTwoNames(t *testing.T) {
	want := []string{
		// funcsconv.go: general conversions, plus fail (validation).
		"len", "bool", "int", "float", "string", "list", "range_expr", "fail",
		// funcsmath.go: math.
		"round", "abs", "floor", "ceil", "min", "max", "sum",
		// funcslist.go: list functions.
		"range", "flatten", "sorted", "reversed", "unique", "any", "all",
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
