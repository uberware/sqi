// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"strings"
	"testing"
)

// kinds runs tokenize and returns just the token kinds, dropping the trailing
// tokEOF, so a test can state the shape of a token stream compactly.
func kinds(t *testing.T, src string) []tokenKind {
	t.Helper()
	toks, err := tokenize(src)
	if err != nil {
		t.Fatalf("tokenize(%q): %v", src, err)
	}
	if len(toks) == 0 || toks[len(toks)-1].kind != tokEOF {
		t.Fatalf("tokenize(%q) did not end with tokEOF: %v", src, toks)
	}
	got := make([]tokenKind, 0, len(toks)-1)
	for _, tok := range toks[:len(toks)-1] {
		got = append(got, tok.kind)
	}
	return got
}

func TestTokenize_Operators(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []tokenKind
	}{
		{"maximal munch prefers double star", "**", []tokenKind{tokDoubleStar}},
		{"maximal munch prefers double slash", "//", []tokenKind{tokDoubleSlash}},
		{"single star after double", "* *", []tokenKind{tokStar, tokStar}},
		{"comparison pairs", "<= >= == !=", []tokenKind{tokLe, tokGe, tokEq, tokNe}},
		{"single comparisons", "< >", []tokenKind{tokLt, tokGt}},
		{"arithmetic", "+ - * / %", []tokenKind{tokPlus, tokMinus, tokStar, tokSlash, tokPercent}},
		{"grouping and punctuation", "()[],.:", []tokenKind{
			tokLParen, tokRParen, tokLBracket, tokRBracket, tokComma, tokDot, tokColon,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := kinds(t, tt.src)
			if len(got) != len(tt.want) {
				t.Fatalf("tokenize(%q) kinds = %v; want %v", tt.src, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("tokenize(%q) kinds = %v; want %v", tt.src, got, tt.want)
				}
			}
		})
	}
}

func TestTokenize_Identifiers(t *testing.T) {
	toks, err := tokenize("Param.Frame _x9 if")
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	want := []struct {
		kind tokenKind
		text string
	}{
		{tokIdent, "Param"},
		{tokDot, "."},
		{tokIdent, "Frame"},
		{tokIdent, "_x9"},
		{tokIdent, "if"},
		{tokEOF, ""},
	}
	if len(toks) != len(want) {
		t.Fatalf("got %d tokens, want %d: %+v", len(toks), len(want), toks)
	}
	for i, w := range want {
		if toks[i].kind != w.kind || toks[i].text != w.text {
			t.Errorf("token %d = %v %q; want %v %q", i, toks[i].kind, toks[i].text, w.kind, w.text)
		}
	}
}

func TestTokenize_WhitespaceAndNewlines(t *testing.T) {
	// Section 1.1.7: expressions span lines with no continuation syntax, so a
	// newline is ordinary whitespace.
	got := kinds(t, "Param.X +\n\t 1")
	want := []tokenKind{tokIdent, tokDot, tokIdent, tokPlus, tokInt}
	if len(got) != len(want) {
		t.Fatalf("kinds = %v; want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("kinds = %v; want %v", got, want)
		}
	}
}

func TestTokenize_Offsets(t *testing.T) {
	toks, err := tokenize("1 + 2")
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	wantOffsets := []int{0, 2, 4, 5}
	for i, want := range wantOffsets {
		if toks[i].offset != want {
			t.Errorf("token %d offset = %d; want %d", i, toks[i].offset, want)
		}
	}
}

func TestTokenize_UnexpectedCharacter(t *testing.T) {
	// Every character here is outside EXPR's grammar. Rejecting them at the
	// lexer is what makes the expr1.1--reject-* conformance fixtures pass:
	// bitwise operators, dict/set literals, matmul, walrus, and statements.
	for _, src := range []string{"1 & 2", "1 | 2", "1 ^ 2", "~1", "{1: 2}", "{1}", "a @ b", "x = 1", "!x", "a; b", "$x", "a ? b"} {
		t.Run(src, func(t *testing.T) {
			if _, err := tokenize(src); err == nil {
				t.Fatalf("tokenize(%q) = nil error; want an error", src)
			}
		})
	}
}

func TestTokenize_UnexpectedCharacterCarriesPosition(t *testing.T) {
	_, err := tokenize("1 + @")
	if err == nil {
		t.Fatal("tokenize = nil error; want an error")
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error is %T; want *Error", err)
	}
	if e.Offset != 4 {
		t.Errorf("Offset = %d; want 4", e.Offset)
	}
}

func TestLexNumber_Integers(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want int64
	}{
		{"decimal", "42", 42},
		{"zero", "0", 0},
		{"double zero is valid", "00", 0},
		{"hex lower", "0x2A", 42},
		{"hex upper prefix", "0X2a", 42},
		{"hex with underscores", "0xFF_FF", 65535},
		{"octal", "0o52", 42},
		{"octal upper prefix", "0O52", 42},
		{"binary", "0b101010", 42},
		{"binary upper prefix", "0B101010", 42},
		{"binary with underscores", "0b1010_1010", 170},
		{"underscore separators", "1_000_000", 1000000},
		{"underscore after base prefix", "0x_FF", 255},
		{"max int64", "9223372036854775807", 9223372036854775807},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			toks, err := tokenize(tt.src)
			if err != nil {
				t.Fatalf("tokenize(%q): %v", tt.src, err)
			}
			if toks[0].kind != tokInt {
				t.Fatalf("kind = %v; want tokInt", toks[0].kind)
			}
			if toks[0].i != tt.want {
				t.Errorf("value = %d; want %d", toks[0].i, tt.want)
			}
		})
	}
}

func TestLexNumber_Floats(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want float64
	}{
		{"decimal float", "3.14", 3.14},
		{"leading dot", ".5", 0.5},
		{"trailing dot", "5.", 5},
		{"scientific lower", "1.5e-3", 0.0015},
		{"scientific upper", "1.5E-3", 0.0015},
		{"positive exponent", "1.5e+3", 1500},
		{"integer with exponent produces a float", "1e10", 1e10},
		{"underscores either side of the point", "1_000.000_001", 1000.000001},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			toks, err := tokenize(tt.src)
			if err != nil {
				t.Fatalf("tokenize(%q): %v", tt.src, err)
			}
			if toks[0].kind != tokFloat {
				t.Fatalf("kind = %v; want tokFloat", toks[0].kind)
			}
			if toks[0].f != tt.want {
				t.Errorf("value = %v; want %v", toks[0].f, tt.want)
			}
		})
	}
}

func TestLexNumber_Rejected(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantMsg string
	}{
		{"leading zero decimal", "007", "leading zeros"},
		{"leading zero decimal longer", "0123", "leading zeros"},
		{"int64 overflow", "9223372036854775808", "out of range"},
		{"double underscore", "1__0", "invalid"},
		{"trailing underscore", "1_", "invalid"},
		{"bare base prefix", "0x", "invalid"},
		{"float out of range", "1e400", "out of range"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tokenize(tt.src)
			if err == nil {
				t.Fatalf("tokenize(%q) = nil error; want an error", tt.src)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %q; want it to contain %q", err.Error(), tt.wantMsg)
			}
		})
	}
}

func TestLexNumber_ExponentMarkerIsNotAlwaysAnExponent(t *testing.T) {
	// "0x1e+2" is hex 0x1e, then "+", then 2 — Python's reading. If the lexer
	// treated "e+" as an exponent inside a based literal it would produce one
	// bogus token instead of three.
	got := kinds(t, "0x1e+2")
	want := []tokenKind{tokInt, tokPlus, tokInt}
	if len(got) != len(want) {
		t.Fatalf("kinds = %v; want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("kinds = %v; want %v", got, want)
		}
	}
}

func TestLexString_Forms(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"single quoted", `'hello'`, "hello"},
		{"double quoted", `"hello"`, "hello"},
		{"empty", `''`, ""},
		{"triple single quoted", `'''hello'''`, "hello"},
		{"triple double quoted", `"""hello"""`, "hello"},
		{"triple spans lines", "'''a\nb'''", "a\nb"},
		{"quote of the other kind is literal", `'say "hi"'`, `say "hi"`},
		{"raw single quoted", `r'hello\n'`, `hello\n`},
		{"raw double quoted", `R"C:\path\to"`, `C:\path\to`},
		{"raw triple quoted", `r'''a\tb'''`, `a\tb`},
		{"raw keeps the backslash before an escaped quote", `r'\''`, `\'`},
		{"escape backslash", `'a\\b'`, `a\b`},
		{"escape single quote", `'it\'s'`, "it's"},
		{"escape double quote", `"say \"hi\""`, `say "hi"`},
		{"escape newline", `'a\nb'`, "a\nb"},
		{"escape carriage return", `'a\rb'`, "a\rb"},
		{"escape tab", `'a\tb'`, "a\tb"},
		{"hex escape", `'\x41'`, "A"},
		{"hex escape above ascii", `'\xe9'`, "\u00e9"},
		{"16-bit unicode escape", `'\u00e9'`, "\u00e9"},
		{"32-bit unicode escape", `'\U0001F600'`, "\U0001F600"},
		{"unrecognized escape is kept verbatim", `'a\db'`, `a\db`},
		{"bell is not in the spec table so it is kept verbatim", `'\a'`, `\a`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			toks, err := tokenize(tt.src)
			if err != nil {
				t.Fatalf("tokenize(%s): %v", tt.src, err)
			}
			if toks[0].kind != tokString {
				t.Fatalf("kind = %v; want tokString", toks[0].kind)
			}
			if toks[0].s != tt.want {
				t.Errorf("value = %q; want %q", toks[0].s, tt.want)
			}
		})
	}
}

func TestLexString_Rejected(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantMsg string
	}{
		{"unterminated short string", `'abc`, "unterminated string literal"},
		{"newline in a short string", "'a\nb'", "newline may not appear"},
		{"unterminated long string", `'''abc`, "unterminated string literal"},
		{"lone trailing backslash", `'abc\`, "unterminated string literal"},
		{"hex escape too short", `'\x4'`, "hexadecimal digits"},
		{"unicode escape too short", `'\u00e'`, "hexadecimal digits"},
		{"hex escape with a non-hex digit", `'\xzz'`, "hexadecimal digits"},
		{"surrogate code point", `'\ud800'`, "not a valid unicode code point"},
		{"code point above the maximum", `'\U0011FFFF'`, "not a valid unicode code point"},
		{"hex escape value truncates to -1 as a rune", `'\UFFFFFFFF'`, "not a valid unicode code point"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tokenize(tt.src)
			if err == nil {
				t.Fatalf("tokenize(%s) = nil error; want an error", tt.src)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %q; want it to contain %q", err.Error(), tt.wantMsg)
			}
		})
	}
}

func TestLexString_PrefixMustTouchTheQuote(t *testing.T) {
	// "r 'x'" is the name r followed by a string, not a raw string. The parser
	// rejects it; the lexer must not silently glue them together.
	got := kinds(t, `r 'x'`)
	want := []tokenKind{tokIdent, tokString}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("kinds = %v; want %v", got, want)
	}
}

func TestLexString_RejectsPythonOnlyPrefixes(t *testing.T) {
	// f-strings and b-strings are not EXPR. They lex as a name followed by a
	// string, which the parser rejects — fixtures expr1.1--reject-fstring and
	// expr1.1--reject-bstring depend on this.
	for _, src := range []string{`f'{x}'`, `b'bytes'`} {
		t.Run(src, func(t *testing.T) {
			got := kinds(t, src)
			if len(got) != 2 || got[0] != tokIdent || got[1] != tokString {
				t.Fatalf("kinds = %v; want [name, string literal]", got)
			}
		})
	}
}
