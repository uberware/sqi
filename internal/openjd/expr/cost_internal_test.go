// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"slices"
	"strings"
	"testing"
)

// TestCost_ArgElements charges rule 2 from a declared argument position.
func TestCost_ArgElements(t *testing.T) {
	s := Shape{
		Params: []Type{ListOf(TInt)},
		Ret:    TInt,
		Cost:   Cost{ArgElements: []int{0}},
		Fn:     func(args []Value) (Value, error) { return Int(int64(len(args[0].AsList()))), nil },
	}
	ec := newEvalCtx("", nil, nil)
	list := List(TInt, []Value{Int(1), Int(2), Int(3)})
	if _, err := callShape(ec, s, bindings{}, []Value{list}); err != nil {
		t.Fatalf("callShape: %v", err)
	}
	// 1 for the call (rule 1) + 3 elements (rule 2).
	if ec.m.ops != 4 {
		t.Errorf("ops = %d; want 4 (1 call + 3 elements)", ec.m.ops)
	}
}

// TestCost_ArgBytes charges rule 3 from a declared argument position.
func TestCost_ArgBytes(t *testing.T) {
	s := Shape{
		Params: []Type{TString},
		Ret:    TString,
		Cost:   Cost{ArgBytes: []int{0}},
		Fn:     func(args []Value) (Value, error) { return args[0], nil },
	}
	ec := newEvalCtx("", nil, nil)
	// 300 bytes -> ceil(300/256) = 2.
	if _, err := callShape(ec, s, bindings{}, []Value{String(strings.Repeat("a", 300))}); err != nil {
		t.Fatalf("callShape: %v", err)
	}
	if ec.m.ops != 3 {
		t.Errorf("ops = %d; want 3 (1 call + 2 byte units)", ec.m.ops)
	}
}

// TestCost_Result charges rule 2 for a value the function PRODUCED.
//
// This case is not symmetric with the argument ones and is not optional:
// range(1000) costs 1001 and list(range_expr('1-100')) costs 102, and both
// charge elements they produced having consumed no list at all. A policy that
// only looked at arguments would undercount every generator in the library.
func TestCost_Result(t *testing.T) {
	s := Shape{
		Params: []Type{TInt},
		Ret:    ListOf(TInt),
		Cost:   Cost{ResultElements: true},
		Fn: func(args []Value) (Value, error) {
			n := int(args[0].AsInt())
			out := make([]Value, n)
			for i := range out {
				out[i] = Int(int64(i))
			}
			return List(TInt, out), nil
		},
	}
	ec := newEvalCtx("", nil, nil)
	if _, err := callShape(ec, s, bindings{}, []Value{Int(5)}); err != nil {
		t.Fatalf("callShape: %v", err)
	}
	if ec.m.ops != 6 {
		t.Errorf("ops = %d; want 6 (1 call + 5 produced elements)", ec.m.ops)
	}
}

// TestCost_ZeroPolicyChargesOnlyTheCall pins the default: a Shape with no
// declared Cost charges rule 1 and nothing else. len() relies on this.
func TestCost_ZeroPolicyChargesOnlyTheCall(t *testing.T) {
	s := Shape{
		Params: []Type{ListOf(TInt)},
		Ret:    TInt,
		Fn:     func(args []Value) (Value, error) { return Int(int64(len(args[0].AsList()))), nil },
	}
	ec := newEvalCtx("", nil, nil)
	list := List(TInt, []Value{Int(1), Int(2), Int(3)})
	if _, err := callShape(ec, s, bindings{}, []Value{list}); err != nil {
		t.Fatalf("callShape: %v", err)
	}
	if ec.m.ops != 1 {
		t.Errorf("ops = %d; want 1 (the call only)", ec.m.ops)
	}
}

// TestSpecNamedIteratingFunctions pins section 1.3.10 rule 2's own named-
// REGISTRY-function list (not verbatim against the wiki's token list --
// "contains()" is deliberately omitted; see specNamedIteratingFunctions's
// CORRECTION comment, shape.go), so a future edit that drifts from the spec
// text fails a test rather than silently shrinking what Tasks 5-8's coverage
// test checks.
func TestSpecNamedIteratingFunctions(t *testing.T) {
	want := []string{
		"sum", "min", "max", "any", "all", "sorted", "reversed", "flatten",
		"join", "range",
		"repr_sh", "repr_py", "repr_json", "repr_pwsh", "repr_cmd",
	}
	got := specNamedIteratingFunctions()
	if !slices.Equal(got, want) {
		t.Errorf("specNamedIteratingFunctions() = %v; want %v", got, want)
	}
}

// TestSpecNamedStringFunctions is TestSpecNamedIteratingFunctions's
// counterpart for rule 3.
func TestSpecNamedStringFunctions(t *testing.T) {
	want := []string{
		"upper", "lower", "replace", "split", "join", "strip",
		"repr_sh",
	}
	got := specNamedStringFunctions()
	if !slices.Equal(got, want) {
		t.Errorf("specNamedStringFunctions() = %v; want %v", got, want)
	}
}
