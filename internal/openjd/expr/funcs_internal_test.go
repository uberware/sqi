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
