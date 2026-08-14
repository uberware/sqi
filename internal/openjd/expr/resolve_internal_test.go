// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"strings"
	"testing"
)

func TestResolveName(t *testing.T) {
	syms := MapSymbols{
		"Param.File":       String("/a/b.exr"),
		"Task.Param.Frame": Int(7),
		"Job.Name":         String("job"),
		"item":             Int(1),
		"a.b.c.d":          Int(2),
	}
	tests := []struct {
		name       string
		src        string
		wantOK     bool
		wantPrefix string
		wantRest   []string
	}{
		{"exact two-segment symbol", "Param.File", true, "Param.File", nil},
		{"exact three-segment symbol", "Task.Param.Frame", true, "Task.Param.Frame", nil},
		{"bare identifier", "item", true, "item", nil},
		{"one trailing property", "Param.File.stem", true, "Param.File", []string{"stem"}},
		{"two trailing properties", "Param.File.parent.name", true, "Param.File", []string{"parent", "name"}},
		{"longest wins over shorter", "a.b.c.d", true, "a.b.c.d", nil},
		{"property on a long symbol", "a.b.c.d.e", true, "a.b.c.d", []string{"e"}},
		{"unknown", "Param.Nope", false, "", nil},
		{"unknown bare", "nope", false, "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := &Name{Parts: strings.Split(tc.src, ".")}
			got, ok := resolveName(n, syms)
			if ok != tc.wantOK {
				t.Fatalf("resolveName(%q) ok = %v, want %v", tc.src, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got.Prefix != tc.wantPrefix {
				t.Errorf("Prefix = %q, want %q", got.Prefix, tc.wantPrefix)
			}
			if strings.Join(got.Rest, ".") != strings.Join(tc.wantRest, ".") {
				t.Errorf("Rest = %v, want %v", got.Rest, tc.wantRest)
			}
		})
	}
}

func TestEvalName_UsesLongestPrefix(t *testing.T) {
	syms := MapSymbols{"Param.File": String("/a/b.exr")}
	// A bare symbol still evaluates.
	v, err := Eval("Param.File", syms, TAny)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if got, want := v.String(), "/a/b.exr"; got != want {
		t.Fatalf("Eval = %q, want %q", got, want)
	}
	// A trailing segment is a property that does not exist.
	_, err = Eval("Param.File.nosuchprop", syms, TAny)
	if err == nil {
		t.Fatal("Eval of a property = nil error, want unknown property")
	}
	if !strings.Contains(err.Error(), "unknown property") {
		t.Fatalf("error = %q, want it to mention unknown property", err.Error())
	}
	// An unknown symbol names the longest candidate the author wrote.
	_, err = Eval("Param.Nope.nosuchprop", syms, TAny)
	if err == nil || !strings.Contains(err.Error(), `"Param.Nope.nosuchprop"`) {
		t.Fatalf("error = %v, want it to name the longest candidate", err)
	}
}
