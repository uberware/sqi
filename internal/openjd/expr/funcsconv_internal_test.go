// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"strings"
	"testing"
)

func TestLen(t *testing.T) {
	syms := MapSymbols{
		"Param.P": Value{Type: TPath, s: "/a/bc"},
		"Param.R": mustRangeExpr(t, "1-10"),
		"Param.U": Unresolved(TString),
	}
	tests := []struct {
		name     string
		src      string
		want     string
		wantType string
	}{
		{"list", "len([1, 2, 3])", "3", "int"},
		{"empty list", "len([])", "0", "int"},
		{"nested list counts the outer", "len([[1, 2], [3]])", "2", "int"},
		{"string counts codepoints", `len("héllo")`, "5", "int"},
		{"string with an astral codepoint", `len("a😀b")`, "3", "int"},
		{"empty string", `len("")`, "0", "int"},
		{"path counts its text", "len(Param.P)", "5", "int"},
		{"range_expr counts its values", "len(Param.R)", "10", "int"},
		{"method form", `"abc".len()`, "3", "int"},
		{"unresolved argument gives a typed placeholder", "len(Param.U)", "<unresolved[int]>", "unresolved[int]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Eval(tc.src, syms, TAny)
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

func TestLen_Rejects(t *testing.T) {
	tests := []struct {
		name     string
		src      string
		wantSubs string
	}{
		{"int has no length", "len(1)", "no signature"},
		{"bool has no length", "len(true)", "no signature"},
		{"null has no length", "len(null)", "no signature"},
		{"too many arguments", `len("a", "b")`, "no signature"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Eval(tc.src, MapSymbols{}, TAny)
			if err == nil {
				t.Fatalf("Eval(%q) succeeded; want an error", tc.src)
			}
			if !strings.Contains(err.Error(), tc.wantSubs) {
				t.Errorf("Eval(%q) error = %v, want it to contain %q", tc.src, err, tc.wantSubs)
			}
		})
	}
}

// mustRangeExpr is defined in ops_internal_test.go and reused here.
