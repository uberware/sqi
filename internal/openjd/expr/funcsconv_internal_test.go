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

func TestString(t *testing.T) {
	syms := MapSymbols{
		"Param.P": Value{Type: TPath, s: "/a/b"},
		"Param.R": mustRangeExpr(t, "1-5:2"),
	}
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"bool", "string(true)", "true"},
		{"null renders as null", "string(null)", "null"},
		{"int", "string(42)", "42"},
		{"float keeps its point", "string(1.5)", "1.5"},
		{"float that looks whole keeps .0", "string(2.0)", "2.0"},
		{"string passes through", `string("hi")`, "hi"},
		{"path renders its text", "string(Param.P)", "/a/b"},
		{"range_expr renders its text", "string(Param.R)", "1-5:2"},
		{"list of ints is JSON", "string([1, 2])", "[1, 2]"},
		{"list of strings is QUOTED, unlike Value.String", `string(["a", "b"])`, `["a", "b"]`},
		{"empty list", "string([])", "[]"},
		{"nested list", "string([[1], [2]])", "[[1], [2]]"},
		{"list of bools", "string([true, false])", "[true, false]"},
		{"a quote inside a string element is escaped", `string(["a\"b"])`, `["a\"b"]`},
		{"a backslash inside a string element is escaped", `string(["a\\b"])`, `["a\\b"]`},
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
			if v.Type.Code != CodeString {
				t.Errorf("Eval(%q) typed %s, want string", tc.src, v.Type)
			}
		})
	}
}

func TestListAndRangeExpr(t *testing.T) {
	syms := MapSymbols{"Param.R": mustRangeExpr(t, "1-5:2")}
	tests := []struct {
		name     string
		src      string
		want     string
		wantType string
	}{
		{"list expands a range_expr", "list(Param.R)", "[1, 3, 5]", "list[int]"},
		{"range_expr parses text", `range_expr("1-3")`, "1-3", "range_expr"},
		{"range_expr parses a comma form", `range_expr("1,3,5-7")`, "1,3,5-7", "range_expr"},
		{"round trip through list", `list(range_expr("1-3"))`, "[1, 2, 3]", "list[int]"},
		{"range_expr from a contiguous list", "range_expr([1, 2, 3])", "1-3", "range_expr"},
		{"range_expr from a stepped list", "range_expr([1, 3, 5])", "1-5:2", "range_expr"},
		{"range_expr sorts and dedupes", "range_expr([5, 1, 2])", "1,2,5", "range_expr"},
		{"range_expr drops duplicates", "range_expr([1, 1, 2])", "1,2", "range_expr"},
		{"a run of two stays separate", "range_expr([1, 3])", "1,3", "range_expr"},
		{"a single value", "range_expr([7])", "7", "range_expr"},
		{"two short runs stay separate", "range_expr([1, 2, 4, 5])", "1,2,4,5", "range_expr"},
		{"mixed runs", "range_expr([1, 2, 3, 7, 9, 11])", "1-3,7-11:2", "range_expr"},
		{"negative values", "range_expr([-3, -2, -1])", "-3--1", "range_expr"},
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

// TestRangeExpr_CanonicalFormRoundTrips guards the canonicalization against
// producing text its own parser rejects — "-3--1" is the case that makes this
// worth a test rather than an assumption.
func TestRangeExpr_CanonicalFormRoundTrips(t *testing.T) {
	for _, src := range []string{
		"range_expr([1, 2, 3])",
		"range_expr([1, 3, 5])",
		"range_expr([-3, -2, -1])",
		"range_expr([1, 2, 3, 7, 9, 11])",
		"range_expr([7])",
	} {
		t.Run(src, func(t *testing.T) {
			v, err := Eval(src, MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", src, err)
			}
			if _, err := RangeExpr(v.AsRangeExpr()); err != nil {
				t.Fatalf("Eval(%q) produced %q, which does not parse back: %v", src, v.AsRangeExpr(), err)
			}
		})
	}
}

func TestListAndRangeExpr_Reject(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{"empty string", `range_expr("")`},
		{"whitespace only", `range_expr(" ")`},
		{"empty list", "range_expr([])"},
		{"unparseable text", `range_expr("not-a-range")`},
		{"list of strings", `range_expr(["1-3"])`},
		{"list of a plain int", "list(3)"},
		{"list of a list", "list([1, 2])"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Eval(tc.src, MapSymbols{}, TAny); err == nil {
				t.Fatalf("Eval(%q) succeeded; want an error", tc.src)
			}
		})
	}
}

// TestFail covers both routes to RFC 0006's stated outcome, because they run on
// different machinery and a regression in one would hide behind the other.
//
// With a RESOLVED argument fail() errors, and condResult suppresses the branch
// that erred. With an UNRESOLVED argument callFunction returns a placeholder
// before Fn ever runs — nothing has failed yet under static checking — and
// UnionOf then drops the noreturn member. Both give the precise type RFC 0006
// promises: float, not float?.
func TestFail(t *testing.T) {
	t.Run("errors with its message when reached", func(t *testing.T) {
		_, err := Eval(`fail("Count must be positive")`, MapSymbols{}, TAny)
		if err == nil {
			t.Fatal("fail() succeeded; want an error")
		}
		if !strings.Contains(err.Error(), "Count must be positive") {
			t.Errorf("fail() error = %v, want it to carry the message", err)
		}
	})

	t.Run("is not reached when the condition is known", func(t *testing.T) {
		v, err := Eval(`5 if 1 > 0 else fail("unreachable")`, MapSymbols{}, TAny)
		if err != nil {
			t.Fatalf("Eval failed: %v", err)
		}
		if got := v.String(); got != "5" {
			t.Errorf("got %s, want 5", got)
		}
	})

	t.Run("does not widen the type when the condition is unknown", func(t *testing.T) {
		syms := MapSymbols{"Param.Rate": Unresolved(TFloat)}
		v, err := Eval(`Param.Rate if Param.Rate > 0.0 else fail("must be positive")`, syms, TAny)
		if err != nil {
			t.Fatalf("Eval failed: %v", err)
		}
		if got := v.Type.String(); got != "unresolved[float]" {
			t.Errorf("typed %s, want unresolved[float] — noreturn must collapse out of the union", got)
		}
	})

	t.Run("an unresolved message does not error", func(t *testing.T) {
		syms := MapSymbols{"Param.Msg": Unresolved(TString)}
		v, err := Eval("fail(Param.Msg)", syms, TAny)
		if err != nil {
			t.Fatalf("fail() with an unresolved argument errored: %v", err)
		}
		if got := v.Type.String(); got != "unresolved[noreturn]" {
			t.Errorf("typed %s, want unresolved[noreturn]", got)
		}
	})

	t.Run("or short-circuits past it", func(t *testing.T) {
		v, err := Eval(`"fast" in ["fast", "slow"] or fail("bad mode")`, MapSymbols{}, TAny)
		if err != nil {
			t.Fatalf("Eval failed: %v", err)
		}
		if got := v.String(); got != "true" {
			t.Errorf("got %s, want true", got)
		}
	})
}
