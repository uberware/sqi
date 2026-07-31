// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"strings"
	"testing"
)

func TestRangeAndFlatten(t *testing.T) {
	tests := []struct {
		name     string
		src      string
		want     string
		wantType string
	}{
		{"range with a stop", "range(5)", "[0, 1, 2, 3, 4]", "list[int]"},
		{"range with a zero stop is empty", "range(0)", "[]", "list[nulltype]"},
		{"range with a negative stop is empty", "range(-1)", "[]", "list[nulltype]"},
		{"range with start and stop", "range(1, 5)", "[1, 2, 3, 4]", "list[int]"},
		{"range with an empty span", "range(5, 5)", "[]", "list[nulltype]"},
		{"range with a step", "range(0, 10, 2)", "[0, 2, 4, 6, 8]", "list[int]"},
		{"range counting down", "range(5, 0, -1)", "[5, 4, 3, 2, 1]", "list[int]"},
		{"range down with an empty span", "range(0, 5, -1)", "[]", "list[nulltype]"},
		{"flatten one level", "flatten([[1, 2], [3]])", "[1, 2, 3]", "list[int]"},
		{"flatten with an empty inner list", "flatten([[1], [], [2]])", "[1, 2]", "list[int]"},
		{"flatten of a flat list is the identity", "flatten([1, 2])", "[1, 2]", "list[int]"},
		{"flatten of an empty list", "flatten([])", "[]", "list[nulltype]"},
		{"flatten of strings", `flatten([["a"], ["b"]])`, "[a, b]", "list[string]"},
		{"flatten only removes one level", "flatten([[[1]], [[2]]])", "[[1], [2]]", "list[list[int]]"},
		{"the comprehension idiom from the spec", `flatten([["-e", e] for e in ["A=1", "B=2"]])`, "[-e, A=1, -e, B=2]", "list[string]"},
		// Regression for the flattenNested defect the C1 task-9 review found:
		// the outer list's own element type is list[int] (a slice preserves its
		// receiver's element type at zero length — slice.go), so the flattened
		// result must report list[int] even though it has no elements, not the
		// more permissive list[nulltype] the old "total == 0" branch hardcoded.
		{"flatten of concretely-typed empty inner lists", "flatten([[1, 2][2:2], [3, 4][2:2]])", "[]", "list[int]"},
		// Regression for the rangeCount overflow the C1 task-9 review found:
		// "stop - start" in plain int64 wrapped to -1 for these arguments,
		// which then divided to a count of 0 — silently answering the empty
		// list rather than erroring OR the true 2-element answer.
		{
			"range across the full int64 span with a matching step",
			"range(-9223372036854775807, 9223372036854775807, 9223372036854775807)",
			"[-9223372036854775807, 0]",
			"list[int]",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Eval(tc.src, MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %s, want %s", tc.src, got, tc.want)
			}
			if got := v.Type.String(); got != tc.wantType {
				t.Errorf("Eval(%q) typed %s, want %s", tc.src, got, tc.wantType)
			}
		})
	}
}

// TestRangeAndFlatten_Bounds pins that both new allocation paths report the
// element bound rather than allocating. maxElements is a per-operation floor
// that always applies; the specification's own configurable limits are
// sub-project E's.
func TestRangeAndFlatten_Bounds(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{"range with an enormous stop", "range(100000000)"},
		{"range with an enormous span", "range(-100000000, 100000000)"},
		// The legitimate too-large sibling of the overflow regression above:
		// same full int64 span, but a step of 1 instead of one that happens to
		// divide it down to 2 elements, so the true count is astronomical and
		// must be reported as too large rather than silently wrapped.
		{"range across the full int64 span with a step of one", "range(-9223372036854775807, 9223372036854775807)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Eval(tc.src, MapSymbols{}, TAny)
			if err == nil {
				t.Fatalf("Eval(%q) succeeded; want a size error", tc.src)
			}
			if !strings.Contains(err.Error(), "too large") {
				t.Errorf("Eval(%q) error = %v, want it to report the size bound", tc.src, err)
			}
		})
	}
}

func TestRange_RejectsAZeroStep(t *testing.T) {
	_, err := Eval("range(0, 10, 0)", MapSymbols{}, TAny)
	if err == nil {
		t.Fatal("range with a zero step succeeded; want an error")
	}
	if !strings.Contains(err.Error(), "step") {
		t.Errorf("error = %v, want it to name the step", err)
	}
}

func TestSortedReversedUnique(t *testing.T) {
	tests := []struct {
		name     string
		src      string
		want     string
		wantType string
	}{
		{"sorted ints", "sorted([3, 1, 2])", "[1, 2, 3]", "list[int]"},
		{"sorted strings", `sorted(["b", "a", "c"])`, "[a, b, c]", "list[string]"},
		{"sorted floats", "sorted([3.5, 1.5])", "[1.5, 3.5]", "list[float]"},
		{"sorted bools put false first", "sorted([true, false])", "[false, true]", "list[bool]"},
		{"sorted nested lists compare lexicographically", "sorted([[2], [1, 9]])", "[[1, 9], [2]]", "list[list[int]]"},
		{"sorted is stable on an empty list", "sorted([])", "[]", "list[nulltype]"},
		{"sorted of one element", "sorted([1])", "[1]", "list[int]"},
		{"reversed ints", "reversed([1, 2, 3])", "[3, 2, 1]", "list[int]"},
		{"reversed empty", "reversed([])", "[]", "list[nulltype]"},
		{"unique preserves first-seen order", "unique([2, 1, 2, 3, 1])", "[2, 1, 3]", "list[int]"},
		{"unique of an empty list", "unique([])", "[]", "list[nulltype]"},
		{"unique with nothing to remove", "unique([1, 2])", "[1, 2]", "list[int]"},
		{"unique of strings", `unique(["a", "b", "a"])`, "[a, b]", "list[string]"},
		{"method form", "[3, 1, 2].sorted()", "[1, 2, 3]", "list[int]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Eval(tc.src, MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %s, want %s", tc.src, got, tc.want)
			}
			if got := v.Type.String(); got != tc.wantType {
				t.Errorf("Eval(%q) typed %s, want %s", tc.src, got, tc.wantType)
			}
		})
	}
}

// TestUnique_UsesTheLanguageEquality pins that unique compares with section
// 1.2.5's cross-type "==", not Go identity: 5 and 5.0 are the same value in
// this language, so the list has one distinct element.
func TestUnique_UsesTheLanguageEquality(t *testing.T) {
	v, err := Eval("unique([5, 5.0])", MapSymbols{}, TAny)
	if err != nil {
		t.Fatalf("Eval failed: %v", err)
	}
	if got := len(v.AsList()); got != 1 {
		t.Errorf("unique([5, 5.0]) kept %d elements, want 1 — section 1.2.5 makes 5 == 5.0", got)
	}
}

func TestAnyAll(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"any with a true element", "any([false, true])", "true"},
		{"any with no true element", "any([false, false])", "false"},
		{"any of an empty list is false", "any([])", "false"},
		{"all with every element true", "all([true, true])", "true"},
		{"all with a false element", "all([true, false])", "false"},
		{"all of an empty list is true", "all([])", "true"},
		{"method form", "[true, false].any()", "true"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Eval(tc.src, MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %s, want %s", tc.src, got, tc.want)
			}
			if v.Type.Code != CodeBool {
				t.Errorf("Eval(%q) typed %s, want bool", tc.src, v.Type)
			}
		})
	}
}

// TestAnyAll_RejectNonBoolElements pins that there is no truthiness here: RFC
// 0006 declares any and all over list[bool] alone.
func TestAnyAll_RejectNonBoolElements(t *testing.T) {
	for _, src := range []string{"any([1, 0])", "all([1, 0])", `any(["a"])`} {
		t.Run(src, func(t *testing.T) {
			if _, err := Eval(src, MapSymbols{}, TAny); err == nil {
				t.Fatalf("Eval(%q) succeeded; want an error", src)
			}
		})
	}
}

// TestSorted_RejectsAnUnorderableElementType pins that sorted reuses section
// 1.2.5's comparator and inherits its refusals rather than inventing an order.
//
// The unorderable case has to come from a SYMBOL, not a literal. section 2.1.4
// lists the orderable types and range_expr is not among them, so compareValues
// has no arm for it — but there is no way to write a list of range expressions
// as a literal, and "[null, null]" is not usable either because section 1.3.2
// rejects null in a list literal outright (fixture
// expr1.3.2--null-in-list-literal-error.invalid.yaml). A supplied list[range_expr]
// is the one route that reaches the comparator with a pair it cannot order.
func TestSorted_RejectsAnUnorderableElementType(t *testing.T) {
	syms := MapSymbols{"Param.Ranges": List(TRangeExpr, []Value{
		mustRangeExpr(t, "5-9"),
		mustRangeExpr(t, "1-3"),
	})}
	_, err := Eval("sorted(Param.Ranges)", syms, TAny)
	if err == nil {
		t.Fatal("sorted over range expressions succeeded; section 2.1.4 gives them no ordering")
	}
	if !strings.Contains(err.Error(), "comparison") {
		t.Errorf("error = %v, want it to report an unsupported comparison", err)
	}
}
