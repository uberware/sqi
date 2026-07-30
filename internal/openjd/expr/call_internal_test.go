// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"strings"
	"testing"
)

// TestFunctionShapes_IsEmpty pins sub-project B3's scope: the registry exists
// and resolves, but every function belongs to sub-project C. If this fails
// because a function was added, that is scope creep, not progress.
func TestFunctionShapes_IsEmpty(t *testing.T) {
	if len(functionShapes) != 0 {
		t.Fatalf("functionShapes has %d entries, want 0 — functions are sub-project C's", len(functionShapes))
	}
}

func TestEvalCall_Errors(t *testing.T) {
	syms := MapSymbols{
		"Param.Name": String("shot01"),
		"Param.List": List(TInt, []Value{Int(1)}),
	}
	tests := []struct {
		name     string
		src      string
		wantSubs string
	}{
		{"unknown plain function", "len(Param.List)", `unknown function "len"`},
		{"unknown method", "Param.Name.upper()", `unknown function "upper"`},
		{"unknown method on a literal", "[1, 2].len()", `unknown function "len"`},
		{"unknown property", "Param.Name.stem", `unknown property "stem"`},
		{"symbol is not callable", "Param.Name()", "is not a function"},
		{"unknown symbol with segments", "Param.Nope.upper()", "unknown symbol"},
		{"dunder call", "__add__(1, 2)", "not directly callable"},
		{"dunder method", "Param.Name.__property_stem__", "not directly callable"},
		{"calling a literal", "5(1)", "cannot be called"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Eval(tc.src, syms, TAny)
			if err == nil {
				t.Fatalf("Eval(%q) = nil error, want one mentioning %q", tc.src, tc.wantSubs)
			}
			if !strings.Contains(err.Error(), tc.wantSubs) {
				t.Fatalf("Eval(%q) error = %q, want it to mention %q", tc.src, err.Error(), tc.wantSubs)
			}
		})
	}
}

// withTestFunction registers a function for the duration of a test, so the call
// path can be exercised end to end while the shipped registry stays empty.
func withTestFunction(t *testing.T, name string, shapes []Shape) {
	t.Helper()
	if _, exists := functionShapes[name]; exists {
		t.Fatalf("%q is already registered", name)
	}
	functionShapes[name] = shapes
	t.Cleanup(func() { delete(functionShapes, name) })
}

func TestEvalCall_DispatchesThroughTheRegistry(t *testing.T) {
	withTestFunction(t, "twice", []Shape{{
		Params: []Type{TInt},
		Ret:    TInt,
		Fn:     func(args []Value) (Value, error) { return Int(args[0].AsInt() * 2), nil },
	}})
	syms := MapSymbols{"Param.N": Int(21), "Param.U": Unresolved(TInt)}
	tests := []struct {
		src      string
		want     string
		wantType string
	}{
		{"twice(21)", "42", "int"},
		{"twice(Param.N)", "42", "int"},
		{"Param.N.twice()", "42", "int"},
		{"twice(Param.U)", "<unresolved[int]>", "unresolved[int]"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, syms, TAny)
			if err != nil {
				t.Fatalf("Eval(%q): %v", tc.src, err)
			}
			if got := v.Type.String(); got != tc.wantType {
				t.Errorf("type = %s, want %s", got, tc.wantType)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("value = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestEvalProperty_DispatchesThroughTheRegistry(t *testing.T) {
	withTestFunction(t, "__property_doubled__", []Shape{{
		Params: []Type{TInt},
		Ret:    TInt,
		Fn:     func(args []Value) (Value, error) { return Int(args[0].AsInt() * 2), nil },
	}})
	v, err := Eval("Param.N.doubled", MapSymbols{"Param.N": Int(21)}, TAny)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if got, want := v.String(), "42"; got != want {
		t.Fatalf("Eval = %s, want %s", got, want)
	}
}
