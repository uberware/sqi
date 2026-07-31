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

// TestLen_EmptyListReceiver pins the section 1.2.4 ruling that an empty list
// literal is a legal METHOD receiver.
//
// Binding an unbound type variable to nulltype converts nothing, so there is no
// implicit coercion for the receiver restriction to suppress. Before this was
// ruled, "[].len()" failed while "len([])" succeeded — the same call in two
// syntaxes disagreeing. The reference implementation returns 0 for both.
func TestLen_EmptyListReceiver(t *testing.T) {
	for _, src := range []string{"[].len()", "len([])"} {
		t.Run(src, func(t *testing.T) {
			v, err := Eval(src, MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", src, err)
			}
			if got := v.String(); got != "0" {
				t.Errorf("Eval(%q) = %s, want 0", src, got)
			}
		})
	}
}

func TestBool(t *testing.T) {
	syms := MapSymbols{"Param.U": Unresolved(TInt)}
	tests := []struct {
		name     string
		src      string
		want     string
		wantType string
	}{
		{"bool passes through", "bool(true)", "true", "bool"},
		{"null is false", "bool(null)", "false", "bool"},
		{"zero is false", "bool(0)", "false", "bool"},
		{"nonzero is true", "bool(3)", "true", "bool"},
		{"negative is true", "bool(-1)", "true", "bool"},
		{"zero float is false", "bool(0.0)", "false", "bool"},
		{"nonzero float is true", "bool(0.5)", "true", "bool"},
		{"string one", `bool("1")`, "true", "bool"},
		{"string true", `bool("true")`, "true", "bool"},
		{"string on", `bool("on")`, "true", "bool"},
		{"string yes", `bool("yes")`, "true", "bool"},
		{"string zero", `bool("0")`, "false", "bool"},
		{"string false", `bool("false")`, "false", "bool"},
		{"string off", `bool("off")`, "false", "bool"},
		{"string no", `bool("no")`, "false", "bool"},
		{"string matching is case-insensitive", `bool("YES")`, "true", "bool"},
		{"string matching is case-insensitive for false", `bool("False")`, "false", "bool"},
		{"unresolved argument gives a typed placeholder", "bool(Param.U)", "<unresolved[bool]>", "unresolved[bool]"},
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

// TestBool_RejectsWithTheSpecifiedWording pins RFC 0006's demand that path and
// list produce "a clear error message such as 'Cannot convert path to bool'".
// Both are registered as noreturn rows for exactly that: without them the
// diagnostic would be the generic "no signature of bool accepts (path)".
func TestBool_RejectsWithTheSpecifiedWording(t *testing.T) {
	syms := MapSymbols{"Param.P": Value{Type: TPath, s: "/a"}}
	tests := []struct {
		name     string
		src      string
		wantSubs string
	}{
		{"path", "bool(Param.P)", "Cannot convert path to bool"},
		{"list", "bool([1, 2])", "Cannot convert list to bool"},
		{"empty list", "bool([])", "Cannot convert list to bool"},
		{"unrecognized string", `bool("maybe")`, `cannot convert "maybe" to bool`},
		{"empty string", `bool("")`, `cannot convert "" to bool`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Eval(tc.src, syms, TAny)
			if err == nil {
				t.Fatalf("Eval(%q) succeeded; want an error", tc.src)
			}
			if !strings.Contains(err.Error(), tc.wantSubs) {
				t.Errorf("Eval(%q) error = %v, want it to contain %q", tc.src, err, tc.wantSubs)
			}
		})
	}
}

func TestIntAndFloat(t *testing.T) {
	tests := []struct {
		name     string
		src      string
		want     string
		wantType string
	}{
		{"int passes through", "int(5)", "5", "int"},
		{"int from a whole float", "int(5.0)", "5", "int"},
		{"int from a negative whole float", "int(-5.0)", "-5", "int"},
		{"int from a string", `int("42")`, "42", "int"},
		{"int from a negative string", `int("-42")`, "-42", "int"},
		{"float passes through", "float(1.5)", "1.5", "float"},
		{"float from an int", "float(5)", "5.0", "float"},
		{"float from a string", `float("1.5")`, "1.5", "float"},
		{"float from an integral string", `float("2")`, "2.0", "float"},
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

// TestIntAndFloat_Reject covers the fixtures TestConformance_C1ProtectedFixtures
// guards: RFC 0006 requires a destructive conversion to be an error, and the
// language produces no infinity and no NaN (section 1.3.4).
func TestIntAndFloat_Reject(t *testing.T) {
	for _, src := range []string{
		"int(3.75)",
		`int("3.75")`,
		`int("")`,
		`int("abc")`,
		`float("")`,
		`float("abc")`,
		`float("inf")`,
		`float("-inf")`,
		`float("nan")`,
		"int([1])",
		"float([1])",
		"int(null)",
	} {
		t.Run(src, func(t *testing.T) {
			if _, err := Eval(src, MapSymbols{}, TAny); err == nil {
				t.Fatalf("Eval(%q) succeeded; want an error", src)
			}
		})
	}
}
