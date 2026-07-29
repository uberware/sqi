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
		// A list literal now both parses AND evaluates, inferring its element
		// type per section 1.2.6 (this task's change) — no longer belongs in
		// the "not yet implemented" group below, unlike its still-unimplemented
		// neighbor, subscript.
		{name: "list literal, element type inferred", src: "[1, 2]", wantCode: expr.CodeList, want: "[1, 2]"},
		// "Param.Name[0]" now evaluates too (section 2.1.7, this task's
		// change) — no longer belongs in the "not yet implemented" group
		// below.
		{name: "subscript", src: "Param.Name[0]", wantCode: expr.CodeString, want: "s"},
		{name: "SUB-PROJECT C: function call", src: "len(Param.Name)", wantErr: "function and method calls"},
		{name: "function calls are sub-project C", src: "len('ab')", wantErr: "function and method calls are not supported"},
		{name: "SUB-PROJECT C: method call", src: "Param.Name.upper()", wantErr: "function and method calls"},
		// String repetition (section 2.1.2) now evaluates too — this task's
		// change, once limits.go's size bound made an unbounded repeat count
		// safe — no longer belongs in the "not yet implemented" group above,
		// unlike its still-unimplemented neighbors.
		{name: "string repetition", src: `'ab' * 3`, wantCode: expr.CodeString, want: "ababab"},

		// Grammar B1 deliberately does not add. "[1, 2]" no longer belongs
		// in this "grammar only" group at all: it now evaluates, see above.
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

// TestCollections_Composed exercises the collection features AGAINST EACH
// OTHER, which nothing else here does.
//
// Every other collections test is per-feature: list literals in one table,
// subscripts in another, slices in a third, list operators in a fourth. Each
// feature was correct on its own and the suite was green, yet composing two of
// them — subscripting the union that slicing a range_expr produces — was a hard
// type error, because the subscript code and the slice code were written one
// task apart and never met. That is the class of defect this test exists to
// catch, so every row here chains at least two constructs, and every row
// asserts a real result rather than merely the absence of an error.
//
// Each shape appears twice: once fully resolved, and once with an unresolved
// placeholder somewhere in it, since the placeholder path computes its result
// TYPE from a different code path than the value path computes its value.
func TestCollections_Composed(t *testing.T) {
	rng, err := expr.RangeExpr("1-5")
	if err != nil {
		t.Fatalf("RangeExpr: %v", err)
	}
	syms := expr.MapSymbols{
		"Param.Range": rng,
		"Param.Items": expr.Unresolved(expr.ListOf(expr.TInt)),
		"Param.I":     expr.Unresolved(expr.TInt),
		"Param.Flag":  expr.Unresolved(expr.TBool),
		"Param.Rng":   expr.Unresolved(expr.TRangeExpr),
	}
	tests := []struct {
		name     string
		src      string
		want     string // Value.String(), or "" to assert the type only
		wantType string
	}{
		// Concatenation, then a reversing slice, then a subscript.
		{"concat then reverse then index", "([1, 2] + [3])[::-1][0]", "3", "int"},
		{
			"concat then reverse then index, unresolved",
			"(Param.Items + [3])[::-1][0]", "", "unresolved[int]",
		},

		// Slice, then a reversing slice.
		{"slice then reverse", "[1, 2, 3, 4, 5][1:4][::-1]", "[4, 3, 2]", "list[int]"},
		{"slice then reverse, unresolved", "Param.Items[1:4][::-1]", "", "unresolved[list[int]]"},

		// A conditional's chosen branch, then a subscript. With an unknown
		// condition this becomes a union receiver, which is exactly the I2
		// false rejection.
		{"conditional then index", "([1, 2, 3] if true else [4])[0]", "1", "int"},
		{
			"conditional then index, unknown condition",
			"([1, 2, 3] if Param.Flag else [4])[0]", "", "unresolved[int]",
		},

		// A subscript of a slice of a literal, nested two levels deep.
		{"index a slice of a nested literal", "[[1, 2], [3, 4]][1:][0][1]", "4", "int"},
		{
			"index a slice of a literal, unresolved bound",
			"[[1, 2], [3, 4]][Param.I:][0][1]", "", "unresolved[int]",
		},

		// Membership against a SLICED range_expr: the operator table meets the
		// slice result type.
		{"in a sliced range", "2 in Param.Range[1:4]", "true", "bool"},
		{"not in a reversed range", "9 not in Param.Range[::-1]", "true", "bool"},
		{
			"in a sliced range, unresolved item",
			"Param.I in Param.Range[1:4]", "", "unresolved[bool]",
		},

		// Subscripting the union a range_expr slice produces — the I2 case,
		// and the one this package manufactures for itself.
		{"index a sliced range placeholder", "Param.Rng[:][0]", "", "unresolved[int]"},
		{"index a reverse-sliced range placeholder", "Param.Rng[::-1][0]", "", "unresolved[int]"},
		{"slice a sliced range placeholder", "Param.Rng[:][0:2]", "", "unresolved[list[int] | range_expr]"},

		// The empty list in each operator position, composed with the rest.
		{"empty list on the left of concat", "([] + [1])[0]", "1", "int"},
		{"empty list on the right of concat", "([1] + [])[0]", "1", "int"},
		{"empty list repeated", "([] * 3) + [1]", "[1]", "list[int]"},
		{"empty list ordered against a non-empty one", "[] < [1]", "true", "bool"},
		{"empty list as a haystack", "1 in []", "false", "bool"},
		{"empty list sliced then concatenated", "[][:] + [1]", "[1]", "list[int]"},
		{
			"empty list on the right of concat, unresolved",
			"(Param.Items + [])[0]", "", "unresolved[int]",
		},

		// Repetition composed with slicing.
		{"repeat then slice", "([1, 2] * 2)[1:3]", "[2, 1]", "list[int]"},
		// A string behaves as a sequence under the same two constructs.
		{"string slice then reverse", "'abcdef'[1:5][::-1]", "edcb", "string"},
		// The whole chain feeding an arithmetic operator.
		{"chain feeding arithmetic", "([1, 2] + [3])[::-1][0] + 1", "4", "int"},

		// PRE-EXISTING DEFECT, asserted as it behaves rather than omitted, so
		// that fixing it trips this row instead of passing unnoticed. OpAdd's
		// generic list shape declares Ret as list[T] with T bound from the LEFT
		// operand only; concatLists recomputes the real common element type at
		// runtime, but on the placeholder path no Fn runs, so an empty list on
		// the left leaves the static result list[nulltype] instead of
		// list[int]. It is direction-dependent — "Param.Items + []" above is
		// correctly unresolved[int] — and predates this fix wave (verified
		// against the branch point). Out of scope here; the fix belongs in the
		// shape's declared return, not in the composition.
		{
			"empty list on the left of an unresolved concat",
			"([] + Param.Items)[0]", "", "unresolved[nulltype]",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := expr.Eval(tc.src, syms, expr.TAny)
			if err != nil {
				t.Fatalf("Eval(%q): %v", tc.src, err)
			}
			if got := v.Type.String(); got != tc.wantType {
				t.Errorf("Eval(%q) type = %s, want %s", tc.src, got, tc.wantType)
			}
			if tc.want != "" {
				if got := v.String(); got != tc.want {
					t.Errorf("Eval(%q) = %s, want %s", tc.src, got, tc.want)
				}
			}
		})
	}
}
