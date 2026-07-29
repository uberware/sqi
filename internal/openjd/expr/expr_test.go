// SPDX-License-Identifier: AGPL-3.0-or-later

package expr_test

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd/expr"
)

// TestLanguage states the language this package implements as a table:
// expression text in, expected value or error out. Read it as the
// specification it is.
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
		wantCode expr.Code
		want     string
		wantErr  string
	}{
		// Literals — section 1.1.1, 1.1.4, 1.1.6.
		{name: "decimal integer", src: "42", wantCode: expr.CodeInt, want: "42"},
		{name: "hex with underscores", src: "0xFF_FF", wantCode: expr.CodeInt, want: "65535"},
		{name: "octal", src: "0o52", wantCode: expr.CodeInt, want: "42"},
		{name: "binary with underscores", src: "0b1010_1010", wantCode: expr.CodeInt, want: "170"},
		{name: "underscore separators", src: "1_000_000", wantCode: expr.CodeInt, want: "1000000"},
		{name: "float", src: "3.14", wantCode: expr.CodeFloat, want: "3.14"},
		{name: "scientific notation", src: "1.5e-3", wantCode: expr.CodeFloat, want: "0.0015"},
		{name: "integer exponent yields a float", src: "1e10", wantCode: expr.CodeFloat, want: "10000000000.0"},
		{name: "single quoted string", src: `'hi'`, wantCode: expr.CodeString, want: "hi"},
		{name: "triple quoted string", src: `"""a b"""`, wantCode: expr.CodeString, want: "a b"},
		{name: "raw string keeps backslashes", src: `r'C:\out'`, wantCode: expr.CodeString, want: `C:\out`},
		{name: "escapes are expanded", src: `'a\tb'`, wantCode: expr.CodeString, want: "a\tb"},
		{name: "unicode name escape", src: `'\N{BULLET}'`, wantCode: expr.CodeString, want: "•"},
		{name: "python bool", src: "True", wantCode: expr.CodeBool, want: "true"},
		{name: "json bool alias", src: "false", wantCode: expr.CodeBool, want: "false"},
		{name: "python none", src: "None", wantCode: expr.CodeNull, want: "null"},
		{name: "json null alias", src: "null", wantCode: expr.CodeNull, want: "null"},
		{name: "leading zeros are a syntax error", src: "007", wantErr: "leading zeros"},

		// Names — section 1.1.3.
		{name: "dotted name", src: "Task.Param.Frame", wantCode: expr.CodeInt, want: "7"},
		{name: "keyword as an attribute", src: "Param.if", wantCode: expr.CodeString, want: "kw"},
		{name: "unknown name", src: "Param.Nope", wantErr: `unknown symbol "Param.Nope"`},

		// Operators — section 2.1.
		{name: "precedence", src: "1 + 2 * 3", wantCode: expr.CodeInt, want: "7"},
		{name: "parentheses", src: "(1 + 2) * 3", wantCode: expr.CodeInt, want: "9"},
		{name: "division always yields a float", src: "10 / 5", wantCode: expr.CodeFloat, want: "2.0"},
		{name: "floored floor division", src: "-7 // 3", wantCode: expr.CodeInt, want: "-3"},
		{name: "floored modulo", src: "-7 % 3", wantCode: expr.CodeInt, want: "2"},
		{name: "power is right associative", src: "2 ** 3 ** 2", wantCode: expr.CodeInt, want: "512"},
		{name: "negative exponent yields a float", src: "2 ** -3", wantCode: expr.CodeFloat, want: "0.125"},
		{name: "float floor division yields an int", src: "7.5 // 2.5", wantCode: expr.CodeInt, want: "3"},
		{name: "string concatenation", src: `'a' + 'b'`, wantCode: expr.CodeString, want: "ab"},
		// Section 2.1.1/2.1.4: mixing int and float promotes the int and uses
		// the float overload — B1's coercing shape match, not A's same-type-
		// only dispatch. See the SUB-PROJECT B comment below for what is still
		// deliberately out of scope.
		{name: "int plus float promotes to float", src: "1 + 2.5", wantCode: expr.CodeFloat, want: "3.5"},
		{name: "int compared to float promotes to float", src: "1 < 2.5", wantCode: expr.CodeBool, want: "true"},
		{name: "substring test", src: `'ell' in 'hello'`, wantCode: expr.CodeBool, want: "true"},
		{name: "chained comparison", src: "1 < 2 < 3", wantCode: expr.CodeBool, want: "true"},
		{name: "chained comparison that fails in the middle", src: "1 < 3 < 2", wantCode: expr.CodeBool, want: "false"},
		{name: "int equals an exactly equal float", src: "5 == 5.0", wantCode: expr.CodeBool, want: "true"},
		{name: "a string never equals a number", src: `'5' == 5`, wantCode: expr.CodeBool, want: "false"},
		{name: "a bool never equals a number", src: "true == 1", wantCode: expr.CodeBool, want: "false"},
		{name: "conditional", src: `'hi' if Param.Flag else 'lo'`, wantCode: expr.CodeString, want: "hi"},
		{name: "or is null-coalescing", src: `Param.Nothing or 'fallback'`, wantCode: expr.CodeString, want: "fallback"},
		{name: "zero is truthy", src: "0 or 'fallback'", wantCode: expr.CodeInt, want: "0"},
		{name: "not", src: "not Param.Flag", wantCode: expr.CodeBool, want: "false"},

		// Implicit coercion, section 1.2.3. Sub-project A rejected every one of
		// these as an unsupported operand pair. "1 + 2.5", "1 < 2.5" and
		// "5 == 5.0" already appear in the Operators section above, so they are
		// not repeated here.
		{name: "float plus int promotes the int", src: "2.5 + 1", wantCode: expr.CodeFloat, want: "3.5"},
		{name: "int divided by int is a float", src: "7 / 2", wantCode: expr.CodeFloat, want: "3.5"},
		{name: "float floor division is an int", src: "7.5 // 2.0", wantCode: expr.CodeInt, want: "3"},
		{name: "int to a negative power is a float", src: "2 ** -2", wantCode: expr.CodeFloat, want: "0.25"},

		// Still errors, because no shape accepts them.
		{name: "string plus int has no signature", src: "'a' + 1", wantErr: "unsupported operand types"},
		{name: "bool arithmetic has no signature", src: "true + true", wantErr: "unsupported operand types"},

		// Errors — section 1.3.11.
		{name: "division by zero", src: "1 / 0", wantErr: "division by zero"},
		{name: "int64 overflow", src: "9223372036854775807 + 1", wantErr: "integer overflow"},
		{name: "infinity is an error", src: "1e300 * 1e300", wantErr: "infinite"},
		{name: "zero to a negative power", src: "0 ** -1", wantErr: "negative power"},
		{name: "a conditional condition must be a bool", src: "1 if 1 else 2", wantErr: "must be a bool"},

		// Deliberately not in sub-project A. Each of these becomes valid later;
		// see the package documentation and the plan's scope table. Int/float
		// promotion (+ and <) moved up to the Operators section above: B1
		// delivers it.
		// "[1, 2]" now parses (this sub-project's own change); evaluating a
		// ListLit is still unimplemented until B2 gives it a value.
		{name: "SUB-PROJECT B: list literal", src: "[1, 2]", wantErr: "cannot evaluate"},
		// "Param.Name[0]" now parses too (this task's change); evaluating an
		// Index is still unimplemented until a later sub-project gives it a
		// value.
		{name: "SUB-PROJECT B: subscript", src: "Param.Name[0]", wantErr: "cannot evaluate"},
		{name: "SUB-PROJECT C: function call", src: "len(Param.Name)", wantErr: "function and method calls"},
		{name: "function calls are sub-project C", src: "len('ab')", wantErr: "function and method calls are not supported"},
		{name: "SUB-PROJECT C: method call", src: "Param.Name.upper()", wantErr: "function and method calls"},
		{name: "SUB-PROJECT E: string repetition", src: `'ab' * 3`, wantErr: "unsupported operand types"},

		// Grammar B1 deliberately does not add. "[1, 2]" and "'ab' * 3" already
		// appear above (SUB-PROJECT B and SUB-PROJECT E), so they are not
		// repeated here.
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := expr.Eval(tt.src, syms, expr.TAny)
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
			if got.Type.Code != tt.wantCode {
				t.Errorf("Type = %s; want code %s", got.Type, tt.wantCode)
			}
			if got.String() != tt.want {
				t.Errorf("= %q; want %q", got.String(), tt.want)
			}
		})
	}
}

// TestLanguage_TargetAndPlaceholders states the target-type and static-type-
// checking half of the language as the same kind of table: an expression, an
// optional symbol table of placeholders, and a target type in; the RESULT'S
// TYPE, rendered, or an error out. It is a separate function from TestLanguage
// because these rows need arguments — syms and target — that the rest of the
// table does not carry.
func TestLanguage_TargetAndPlaceholders(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		syms    expr.MapSymbols
		target  expr.Type
		want    string // the result's type, rendered
		wantErr string
	}{
		{name: "no constraint keeps the natural type", src: "1 + 1", target: expr.TAny, want: "int"},
		{name: "an int result reaches a string target", src: "1 + 1", target: expr.TString, want: "string"},
		{name: "a fractional float will not narrow to an int", src: "3.75", target: expr.TInt, wantErr: "cannot be represented"},
		{
			name:   "arithmetic on a declared int is checkable before its value exists",
			src:    "Param.Frame + 1",
			syms:   expr.MapSymbols{"Param.Frame": expr.Unresolved(expr.TInt)},
			target: expr.TAny,
			want:   "unresolved[int]",
		},
		{
			name:    "a type error is caught before any value exists",
			src:     "Param.Name + 5",
			syms:    expr.MapSymbols{"Param.Name": expr.Unresolved(expr.TString)},
			target:  expr.TAny,
			wantErr: "unsupported operand types",
		},
		{
			name:   "an unknown condition unions both branches",
			src:    "1 if Param.Flag else 'x'",
			syms:   expr.MapSymbols{"Param.Flag": expr.Unresolved(expr.TBool)},
			target: expr.TAny,
			want:   "unresolved[int | string]",
		},
		// A union-typed value can now be USED, not just produced: __pow__'s
		// declared return, int | float, feeds "+ 1" here. Every member has a
		// route into the (float, float) shape (int by widening, float
		// exactly), so it is the only admissible candidate; neither member
		// alone would pick (int, int), since float has no lossless route to
		// int. The result is unresolved[float], not unresolved[float | int]:
		// once "+" has selected its shape, the shape's OWN declared return —
		// float, not a union — is what propagates, same as any other
		// operator call.
		{
			name:   "a union produced by ** is usable by + that consumes it",
			src:    "Param.X ** 2 + 1",
			syms:   expr.MapSymbols{"Param.X": expr.Unresolved(expr.TInt)},
			target: expr.TAny,
			want:   "unresolved[float]",
		},
		// The same union, unary-negated: -(int | float) has a route through
		// both members (OpNeg has both an (int) and a (float) shape), so it
		// is admissible too, and by the same reasoning settles on float.
		{
			name:   "a union produced by ** is usable by unary minus",
			src:    "-(Param.X ** 2)",
			syms:   expr.MapSymbols{"Param.X": expr.Unresolved(expr.TInt)},
			target: expr.TAny,
			want:   "unresolved[float]",
		},
		// The same union reaching an explicit target directly: both members
		// (int, float) are individually coercible to float, so the whole
		// union is, per coercible's new union-source rule.
		{
			name:   "a union produced by ** reaches a float target",
			src:    "Param.X ** 2",
			syms:   expr.MapSymbols{"Param.X": expr.Unresolved(expr.TInt)},
			target: expr.TFloat,
			want:   "unresolved[float]",
		},
		// evalLogical's own union (int | string here) reaching a string
		// target: both members are individually coercible to string (every
		// scalar is, via section 1.2.3's catch-all), so the union is too.
		{
			name:   "a union produced by the conditional expression reaches a string target",
			src:    "1 if Param.Flag else 'x'",
			syms:   expr.MapSymbols{"Param.Flag": expr.Unresolved(expr.TBool)},
			target: expr.TString,
			want:   "unresolved[string]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := expr.Eval(tt.src, tt.syms, tt.target)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v; want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Eval(%q): %v", tt.src, err)
			}
			if got.Type.String() != tt.want {
				t.Errorf("type = %q; want %q", got.Type.String(), tt.want)
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
		got, err := e.Eval(expr.MapSymbols{"Param.Frame": expr.Int(tc.in)}, expr.TAny)
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
	_, err := expr.Eval("Param.X\n  + 'oops'", expr.MapSymbols{"Param.X": expr.Int(1)}, expr.TAny)
	if err == nil {
		t.Fatal("Eval = nil error; want unsupported operand types")
	}
	if !strings.HasPrefix(err.Error(), "line 2, col 3:") {
		t.Errorf("error = %q; want it to start with the operator's line and column", err.Error())
	}
}
