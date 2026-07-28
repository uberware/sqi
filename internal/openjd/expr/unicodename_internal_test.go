// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"strings"
	"testing"
)

func TestUnicodeByName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  rune
	}{
		{"ascii letter", "LATIN SMALL LETTER A", 'a'},
		{"symbol", "BULLET", '•'},
		{"pictograph", "SNOWMAN", '☃'},
		{"astral plane", "GRINNING FACE", '\U0001F600'},
		{"accented latin", "LATIN SMALL LETTER E WITH ACUTE", 'é'},
		{"lookup is case insensitive", "snowman", '☃'},
		{"surrounding space is ignored", "  BULLET  ", '•'},
		{"algorithmic cjk name", "CJK UNIFIED IDEOGRAPH-4E00", '一'},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := unicodeByName(tt.input)
			if !ok {
				t.Fatalf("unicodeByName(%q) not found", tt.input)
			}
			if got != tt.want {
				t.Errorf("unicodeByName(%q) = U+%04X; want U+%04X", tt.input, got, tt.want)
			}
		})
	}
}

func TestUnicodeByName_NotFound(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"nonsense", "NOT A REAL CHARACTER NAME"},
		{"placeholder names are not exposed", "<CJK IDEOGRAPH>"},
		{"cjk name outside the ideograph blocks", "CJK UNIFIED IDEOGRAPH-0041"},
		{"hangul syllables are documented as unsupported", "HANGUL SYLLABLE GA"},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if r, ok := unicodeByName(tt.input); ok {
				t.Errorf("unicodeByName(%q) = U+%04X, true; want not found", tt.input, r)
			}
		})
	}
}

func TestLexString_NamedEscape(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"named escape", `'\N{BULLET}'`, "•"},
		{"named escape among text", `'a\N{SNOWMAN}b'`, "a☃b"},
		{"raw string does not decode it", `r'\N{BULLET}'`, `\N{BULLET}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			toks, err := tokenize(tt.src)
			if err != nil {
				t.Fatalf("tokenize(%s): %v", tt.src, err)
			}
			if toks[0].s != tt.want {
				t.Errorf("value = %q; want %q", toks[0].s, tt.want)
			}
		})
	}
}

func TestLexString_NamedEscapeRejected(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantMsg string
	}{
		{"no braces", `'\Nx'`, "name in braces"},
		{"missing closing brace", `'\N{BULLET'`, "closing brace"},
		{"unknown name", `'\N{NOT A CHARACTER}'`, "unknown unicode character name"},
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
