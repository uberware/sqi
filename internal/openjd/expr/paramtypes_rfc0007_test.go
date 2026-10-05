// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "testing"

// TestValueFromText_ResolvesTheF1Types is the guard on the hand-off EXPR
// sub-project E4a left in ValueFromText's own doc comment: "BOOL and LIST[*]
// are sub-project F's job/task-parameter types and a template cannot declare
// one yet... F MUST add its cases here when that lands, or its own parameters
// will silently never resolve past a placeholder in phase 3."
//
// It asserts CONCRETENESS rather than correctness, because that is where the
// hazard is: an unhandled type falls to the default branch and returns
// Unresolved(t), which is a LEGITIMATE phase-1 result. Nothing errors, nothing
// logs, and every expression referencing the parameter simply never resolves.
func TestValueFromText_ResolvesTheF1Types(t *testing.T) {
	tests := []struct {
		name string
		typ  Type
		raw  string
	}{
		{"bool true", TBool, "true"},
		{"bool false", TBool, "false"},
		{"bool yes", TBool, "yes"},
		{"bool 1", TBool, "1"},
		{"list of strings", ListOf(TString), `["a","b"]`},
		{"list of ints", ListOf(TInt), `[1,2]`},
		{"list of floats", ListOf(TFloat), `[1.5,2]`},
		{"list of bools", ListOf(TBool), `[true,false]`},
		{"list of paths", ListOf(TPath), `["/tmp/a"]`},
		{"nested list", ListOf(ListOf(TInt)), `[[1,2],[3]]`},
		{"empty list", ListOf(TString), `[]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ValueFromText(tt.typ, tt.raw, PathPOSIX)
			if got.Type.Code == CodeUnresolved {
				t.Fatalf("ValueFromText(%s, %q) stayed Unresolved — a parameter of "+
					"this type would silently never resolve in phase 3", tt.typ, tt.raw)
			}
			if !got.Type.Equal(tt.typ) {
				t.Errorf("ValueFromText(%s, %q) has type %s, want %s",
					tt.typ, tt.raw, got.Type, tt.typ)
			}
		})
	}
}

// TestValueFromText_ListValuesAreCorrect checks the values themselves, not
// only that something concrete came back. Concreteness is the silent hazard;
// this is the ordinary correctness the concreteness test cannot see.
func TestValueFromText_ListValuesAreCorrect(t *testing.T) {
	v := ValueFromText(ListOf(TInt), `[1,2,3]`, PathPOSIX)
	if got, want := v.String(), "[1, 2, 3]"; got != want {
		t.Errorf("list[int] rendered %q, want %q", got, want)
	}

	// A bool element may be spelled any of the ways RFC 0007 accepts, and must
	// arrive as a real bool either way.
	b := ValueFromText(ListOf(TBool), `["off",true,"no",1]`, PathPOSIX)
	if got, want := b.String(), "[false, true, false, true]"; got != want {
		t.Errorf("list[bool] rendered %q, want %q", got, want)
	}

	// Nested lists keep their shape.
	n := ValueFromText(ListOf(ListOf(TInt)), `[[1,2],[3]]`, PathPOSIX)
	if got, want := n.String(), "[[1, 2], [3]]"; got != want {
		t.Errorf("list[list[int]] rendered %q, want %q", got, want)
	}
}

// TestValueFromText_MalformedListStaysUnresolved pins the other half of the
// contract this function already documents: a value that fails to parse is
// reported as still-unknown rather than as a lie about what was bound.
// Validating a submitted value against its declared type happens upstream.
func TestValueFromText_MalformedListStaysUnresolved(t *testing.T) {
	for _, raw := range []string{"not json", "{}", `["a"`, "42", `["a",{"k":1}]`} {
		t.Run(raw, func(t *testing.T) {
			got := ValueFromText(ListOf(TString), raw, PathPOSIX)
			if got.Type.Code != CodeUnresolved {
				t.Errorf("ValueFromText(list[string], %q) = %v, want Unresolved", raw, got)
			}
		})
	}
}

// TestValueFromText_ListElementTypeMismatchStaysUnresolved covers the case
// where the JSON is well formed but an element does not belong: the whole
// value stays unresolved rather than binding a list with a wrong-typed hole
// in it.
func TestValueFromText_ListElementTypeMismatchStaysUnresolved(t *testing.T) {
	for _, tc := range []struct {
		typ Type
		raw string
	}{
		{ListOf(TInt), `[1,"two"]`},
		{ListOf(TBool), `[true,"maybe"]`},
		{ListOf(ListOf(TInt)), `[[1],"notalist"]`},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			if got := ValueFromText(tc.typ, tc.raw, PathPOSIX); got.Type.Code != CodeUnresolved {
				t.Errorf("ValueFromText(%s, %q) = %v, want Unresolved", tc.typ, tc.raw, got)
			}
		})
	}
}

// TestValueFromText_BoolRejectsUnknownSpelling keeps this package's BOOL table
// aligned with internal/openjd's parseBoolParamValue. The two live in
// different packages because the worker binary must never import
// internal/openjd, so nothing but a matching pair of tests relates them: a
// value the VALIDATOR admits but this function rejects would pass submission
// and then resolve to nothing on the host.
func TestValueFromText_BoolRejectsUnknownSpelling(t *testing.T) {
	for _, raw := range []string{"maybe", "2", "y", "", "2.0"} {
		t.Run(raw, func(t *testing.T) {
			if got := ValueFromText(TBool, raw, PathPOSIX); got.Type.Code != CodeUnresolved {
				t.Errorf("ValueFromText(bool, %q) = %v, want Unresolved", raw, got)
			}
		})
	}
	for _, raw := range []string{"true", "false", "yes", "no", "on", "off", "1", "0", "1.0", "0.0"} {
		t.Run("accepts/"+raw, func(t *testing.T) {
			if got := ValueFromText(TBool, raw, PathPOSIX); got.Type.Code == CodeUnresolved {
				t.Errorf("ValueFromText(bool, %q) stayed Unresolved; internal/openjd's "+
					"validator accepts this spelling, so the two tables have drifted", raw)
			}
		})
	}
}

// TestJobParamTypes_F1Types pins the section 1.2.2 mapping for the types RFC
// 0007 adds.
//
// It asserts existing behavior rather than driving new code: JobParamTypes
// special-cases LIST[PATH] and otherwise falls through to
// ParseType(strings.ToLower(declared)), which already understands these names
// because sub-projects B1 and B2 built the types. Nothing needed writing here.
//
// It is worth a test anyway because a regression would be SILENT: an
// unrecognized spelling floors to TAny, which type-checks against everything,
// so the failure would surface as an expression that wrongly PASSES rather
// than as an error.
func TestJobParamTypes_F1Types(t *testing.T) {
	tests := []struct {
		declared           string
		wantParam, wantRaw Type
	}{
		{"BOOL", TBool, TBool},
		{"RANGE_EXPR", TRangeExpr, TRangeExpr},
		{"LIST[STRING]", ListOf(TString), ListOf(TString)},
		{"LIST[INT]", ListOf(TInt), ListOf(TInt)},
		{"LIST[FLOAT]", ListOf(TFloat), ListOf(TFloat)},
		{"LIST[BOOL]", ListOf(TBool), ListOf(TBool)},
		// LIST[PATH] is the only list type whose RawParam form differs: path
		// mapping applies per element, so the raw form is list[string].
		{"LIST[PATH]", ListOf(TPath), ListOf(TString)},
		{"LIST[LIST[INT]]", ListOf(ListOf(TInt)), ListOf(ListOf(TInt))},
	}
	for _, tt := range tests {
		t.Run(tt.declared, func(t *testing.T) {
			gotParam, gotRaw := JobParamTypes(tt.declared)
			if !gotParam.Equal(tt.wantParam) {
				t.Errorf("Param type = %s, want %s", gotParam, tt.wantParam)
			}
			if !gotRaw.Equal(tt.wantRaw) {
				t.Errorf("RawParam type = %s, want %s", gotRaw, tt.wantRaw)
			}
		})
	}
}
