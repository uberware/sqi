// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "testing"

func TestJobParamTypes(t *testing.T) {
	tests := []struct {
		declared           string
		wantParam, wantRaw Type
	}{
		// Section 1.2.2: Param and RawParam share the same type for every
		// declared type except PATH/LIST[PATH].
		{"STRING", TString, TString},
		{"INT", TInt, TInt},
		{"FLOAT", TFloat, TFloat},
		{"PATH", TPath, TString},
		{"LIST[PATH]", ListOf(TPath), ListOf(TString)},
		// BOOL is not a legal job-parameter declaration per section 1.2.2 (F's
		// job-parameter types are not yet declarable), but ParseType parses
		// it as a type regardless of whether it is a legal job-parameter
		// spelling, so it floors through the generic path to bool/bool, not
		// to any.
		{"BOOL", TBool, TBool},
		// Unrecognized spelling floors to any rather than leaving the name
		// unbound.
		{"", TAny, TAny},
		{"not-a-type", TAny, TAny},
	}
	for _, tc := range tests {
		t.Run(tc.declared, func(t *testing.T) {
			gotParam, gotRaw := JobParamTypes(tc.declared)
			if !gotParam.Equal(tc.wantParam) {
				t.Errorf("JobParamTypes(%q) paramType = %s, want %s", tc.declared, gotParam, tc.wantParam)
			}
			if !gotRaw.Equal(tc.wantRaw) {
				t.Errorf("JobParamTypes(%q) rawType = %s, want %s", tc.declared, gotRaw, tc.wantRaw)
			}
		})
	}
}

func TestTaskParamType(t *testing.T) {
	tests := []struct {
		declared string
		want     Type
	}{
		{"STRING", TString},
		{"INT", TInt},
		{"FLOAT", TFloat},
		{"PATH", TPath},
		{"CHUNK[INT]", TRangeExpr},
		{"", TAny},
		{"not-a-type", TAny},
	}
	for _, tc := range tests {
		t.Run(tc.declared, func(t *testing.T) {
			got := TaskParamType(tc.declared)
			if !got.Equal(tc.want) {
				t.Errorf("TaskParamType(%q) = %s, want %s", tc.declared, got, tc.want)
			}
		})
	}
}

func TestValueFromText(t *testing.T) {
	t.Run("int parses", func(t *testing.T) {
		v := ValueFromText(TInt, "42", PathPOSIX)
		if v.IsUnresolved() || v.AsInt() != 42 {
			t.Errorf("ValueFromText(TInt, %q) = %+v, want concrete Int(42)", "42", v)
		}
	})
	t.Run("int parse failure falls back to unresolved", func(t *testing.T) {
		v := ValueFromText(TInt, "not-an-int", PathPOSIX)
		if !v.IsUnresolved() {
			t.Errorf("ValueFromText(TInt, %q) = %+v, want Unresolved", "not-an-int", v)
		}
	})
	t.Run("float carries the submitted text (section 1.3.4)", func(t *testing.T) {
		v := ValueFromText(TFloat, "3.500", PathPOSIX)
		if v.IsUnresolved() {
			t.Fatal("ValueFromText(TFloat, \"3.500\") is unresolved, want concrete")
		}
		if got := v.String(); got != "3.500" {
			t.Errorf("ValueFromText(TFloat, %q).String() = %q, want %q (carried text)", "3.500", got, "3.500")
		}
	})
	t.Run("float parse failure falls back to unresolved", func(t *testing.T) {
		v := ValueFromText(TFloat, "nope", PathPOSIX)
		if !v.IsUnresolved() {
			t.Errorf("ValueFromText(TFloat, %q) = %+v, want Unresolved", "nope", v)
		}
	})
	t.Run("string always concrete", func(t *testing.T) {
		v := ValueFromText(TString, "1", PathPOSIX)
		if v.IsUnresolved() || v.AsStr() != "1" {
			t.Errorf(`ValueFromText(TString, "1") = %+v, want concrete String("1")`, v)
		}
	})
	t.Run("path uses the caller's flavor", func(t *testing.T) {
		v := ValueFromText(TPath, "a/b", PathWindows)
		if v.IsUnresolved() {
			t.Fatal("ValueFromText(TPath, \"a/b\", PathWindows) is unresolved, want concrete")
		}
		if v.pf != PathWindows {
			t.Errorf("ValueFromText(TPath, ..., PathWindows).pf = %v, want PathWindows", v.pf)
		}
	})
	t.Run("bool, list, range_expr never made concrete here", func(t *testing.T) {
		for _, ty := range []Type{TBool, ListOf(TInt), TRangeExpr} {
			v := ValueFromText(ty, "anything", PathPOSIX)
			if !v.IsUnresolved() {
				t.Errorf("ValueFromText(%s, ...) = %+v, want Unresolved (by construction)", ty, v)
			}
		}
	})
}
