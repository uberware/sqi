// SPDX-License-Identifier: AGPL-3.0-or-later

package expr_test

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd/expr"
)

// TestLanguage states sub-project A's language as a table: expression text in,
// expected value or error out. Read it as the specification it is.
func TestLanguage(t *testing.T) {
	syms := expr.MapSymbols{
		"Param.X":          expr.Int(10),
		"Param.Y":          expr.Float(2.5),
		"Param.Name":       expr.String("shot01"),
		"Param.Flag":       expr.Bool(true),
		"Param.Nothing":    expr.Null(),
		"Param.if":         expr.String("kw"),
		"Task.Param.Frame": expr.Int(7),
	}

	tests := []struct {
		name     string
		src      string
		wantKind expr.Kind
		want     string
		wantErr  string
	}{
		// Literals — section 1.1.1, 1.1.4, 1.1.6.
		{name: "decimal integer", src: "42", wantKind: expr.KindInt, want: "42"},
		{name: "hex with underscores", src: "0xFF_FF", wantKind: expr.KindInt, want: "65535"},
		{name: "octal", src: "0o52", wantKind: expr.KindInt, want: "42"},
		{name: "binary with underscores", src: "0b1010_1010", wantKind: expr.KindInt, want: "170"},
		{name: "underscore separators", src: "1_000_000", wantKind: expr.KindInt, want: "1000000"},
		{name: "float", src: "3.14", wantKind: expr.KindFloat, want: "3.14"},
		{name: "scientific notation", src: "1.5e-3", wantKind: expr.KindFloat, want: "0.0015"},
		{name: "integer exponent yields a float", src: "1e10", wantKind: expr.KindFloat, want: "10000000000.0"},
		{name: "single quoted string", src: `'hi'`, wantKind: expr.KindString, want: "hi"},
		{name: "triple quoted string", src: `"""a b"""`, wantKind: expr.KindString, want: "a b"},
		{name: "raw string keeps backslashes", src: `r'C:\out'`, wantKind: expr.KindString, want: `C:\out`},
		{name: "escapes are expanded", src: `'a\tb'`, wantKind: expr.KindString, want: "a\tb"},
		{name: "unicode name escape", src: `'\N{BULLET}'`, wantKind: expr.KindString, want: "•"},
		{name: "python bool", src: "True", wantKind: expr.KindBool, want: "true"},
		{name: "json bool alias", src: "false", wantKind: expr.KindBool, want: "false"},
		{name: "python none", src: "None", wantKind: expr.KindNull, want: "null"},
		{name: "json null alias", src: "null", wantKind: expr.KindNull, want: "null"},
		{name: "leading zeros are a syntax error", src: "007", wantErr: "leading zeros"},

		// Names — section 1.1.3.
		{name: "dotted name", src: "Task.Param.Frame", wantKind: expr.KindInt, want: "7"},
		{name: "keyword as an attribute", src: "Param.if", wantKind: expr.KindString, want: "kw"},
		{name: "unknown name", src: "Param.Nope", wantErr: `unknown symbol "Param.Nope"`},

		// Operators — section 2.1.
		{name: "precedence", src: "1 + 2 * 3", wantKind: expr.KindInt, want: "7"},
		{name: "parentheses", src: "(1 + 2) * 3", wantKind: expr.KindInt, want: "9"},
		{name: "division always yields a float", src: "10 / 5", wantKind: expr.KindFloat, want: "2.0"},
		{name: "floored floor division", src: "-7 // 3", wantKind: expr.KindInt, want: "-3"},
		{name: "floored modulo", src: "-7 % 3", wantKind: expr.KindInt, want: "2"},
		{name: "power is right associative", src: "2 ** 3 ** 2", wantKind: expr.KindInt, want: "512"},
		{name: "negative exponent yields a float", src: "2 ** -3", wantKind: expr.KindFloat, want: "0.125"},
		{name: "float floor division yields an int", src: "7.5 // 2.5", wantKind: expr.KindInt, want: "3"},
		{name: "string concatenation", src: `'a' + 'b'`, wantKind: expr.KindString, want: "ab"},
		{name: "substring test", src: `'ell' in 'hello'`, wantKind: expr.KindBool, want: "true"},
		{name: "chained comparison", src: "1 < 2 < 3", wantKind: expr.KindBool, want: "true"},
		{name: "chained comparison that fails in the middle", src: "1 < 3 < 2", wantKind: expr.KindBool, want: "false"},
		{name: "int equals an exactly equal float", src: "5 == 5.0", wantKind: expr.KindBool, want: "true"},
		{name: "a string never equals a number", src: `'5' == 5`, wantKind: expr.KindBool, want: "false"},
		{name: "a bool never equals a number", src: "true == 1", wantKind: expr.KindBool, want: "false"},
		{name: "conditional", src: `'hi' if Param.Flag else 'lo'`, wantKind: expr.KindString, want: "hi"},
		{name: "or is null-coalescing", src: `Param.Nothing or 'fallback'`, wantKind: expr.KindString, want: "fallback"},
		{name: "zero is truthy", src: "0 or 'fallback'", wantKind: expr.KindInt, want: "0"},
		{name: "not", src: "not Param.Flag", wantKind: expr.KindBool, want: "false"},

		// Errors — section 1.3.11.
		{name: "division by zero", src: "1 / 0", wantErr: "division by zero"},
		{name: "int64 overflow", src: "9223372036854775807 + 1", wantErr: "integer overflow"},
		{name: "infinity is an error", src: "1e300 * 1e300", wantErr: "infinite"},
		{name: "zero to a negative power", src: "0 ** -1", wantErr: "negative power"},
		{name: "a conditional condition must be a bool", src: "1 if 1 else 2", wantErr: "must be a bool"},

		// Deliberately not in sub-project A. Each of these becomes valid later;
		// see the package documentation and the plan's scope table.
		{name: "SUB-PROJECT B: int plus float", src: "1 + 2.5", wantErr: "unsupported operand types"},
		{name: "SUB-PROJECT B: int compared to float", src: "1 < 2.5", wantErr: "unsupported operand types"},
		{name: "SUB-PROJECT B: list literal", src: "[1, 2]", wantErr: "list expressions are not supported"},
		{name: "SUB-PROJECT B: subscript", src: "Param.Name[0]", wantErr: "subscript and slice"},
		{name: "SUB-PROJECT C: function call", src: "len(Param.Name)", wantErr: "function and method calls"},
		{name: "SUB-PROJECT C: method call", src: "Param.Name.upper()", wantErr: "function and method calls"},
		{name: "SUB-PROJECT E: string repetition", src: `'ab' * 3`, wantErr: "unsupported operand types"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := expr.Eval(tt.src, syms)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Eval(%q) = %v; want an error containing %q", tt.src, got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q; want it to contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Eval(%q): %v", tt.src, err)
			}
			if got.Kind != tt.wantKind {
				t.Errorf("Kind = %s; want %s", got.Kind, tt.wantKind)
			}
			if got.String() != tt.want {
				t.Errorf("= %q; want %q", got.String(), tt.want)
			}
		})
	}
}

func TestParseThenEvalTwice(t *testing.T) {
	e, err := expr.Parse("Param.Frame * 2")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, tc := range []struct {
		in   int64
		want string
	}{{1, "2"}, {21, "42"}} {
		got, err := e.Eval(expr.MapSymbols{"Param.Frame": expr.Int(tc.in)})
		if err != nil {
			t.Fatalf("Eval: %v", err)
		}
		if got.String() != tc.want {
			t.Errorf("Eval with Frame=%d = %s; want %s", tc.in, got.String(), tc.want)
		}
	}
}

func TestNamesFromOutside(t *testing.T) {
	e, err := expr.Parse("Param.A + Task.Param.Frame if Param.Flag else Param.A")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{"Param.A", "Param.Flag", "Task.Param.Frame"}
	got := e.Names()
	if len(got) != len(want) {
		t.Fatalf("Names() = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() = %v; want %v", got, want)
		}
	}
}

func TestErrorsCarryLineAndColumn(t *testing.T) {
	_, err := expr.Eval("Param.X\n  + 'oops'", expr.MapSymbols{"Param.X": expr.Int(1)})
	if err == nil {
		t.Fatal("Eval = nil error; want unsupported operand types")
	}
	if !strings.HasPrefix(err.Error(), "line 2, col 3:") {
		t.Errorf("error = %q; want it to start with the operator's line and column", err.Error())
	}
}
