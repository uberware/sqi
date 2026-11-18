// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "testing"

// TestNumericString_ConversionIgnoresSurroundingWhiteSpace pins RFC 0005
// §1.2.3 and RFC 0006's int()/float() rows: a string converted to int or float
// may have white space before and after the number, which is ignored, while
// white space inside the number is still an error. White space is the Unicode
// White_Space property (Template Schemas §7.5), which excludes U+001C–U+001F
// and U+200B even though isspace() accepts the former.
func TestNumericString_ConversionIgnoresSurroundingWhiteSpace(t *testing.T) {
	ok := []struct {
		src  string
		want string
	}{
		{`int(' 7 ')`, "7"},
		{`int('\t-7\n')`, "-7"},
		{`int(' 7　')`, "7"},
		{`float('2.5\n')`, "2.5"},
		{`float(' 3 ')`, "3.0"},
		{`int(' 7 ') + 1`, "8"},
	}
	for _, tc := range ok {
		t.Run(tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %s, want %s", tc.src, got, tc.want)
			}
		})
	}
	for _, src := range []string{
		`int('1 000')`,
		"int('\\u200b7')",
		`int('\u001c7')`,
		"int('\\ufeff7')",
		`int('   ')`,
		`float('2. 5')`,
	} {
		t.Run(src, func(t *testing.T) {
			if v, err := Eval(src, MapSymbols{}, TAny); err == nil {
				t.Errorf("Eval(%q) = %s, want an error", src, v)
			}
		})
	}
}

// TestValueFromText_TrimsNumericWhiteSpace pins the binding half: a value given
// as a numeric string for an INT or FLOAT parameter may carry white space around
// the number, and that white space is not part of a FLOAT's original text
// (Template Schemas §7.5, RFC 0005 §1.3.4): " 3.500 " renders "3.500".
func TestValueFromText_TrimsNumericWhiteSpace(t *testing.T) {
	if v := ValueFromText(TInt, " 7 ", PathPOSIX); v.String() != "7" {
		t.Errorf("ValueFromText(int, %q) = %s, want 7", " 7 ", v)
	}
	if v := ValueFromText(TFloat, " 3.500 ", PathPOSIX); v.String() != "3.500" {
		t.Errorf("ValueFromText(float, %q) = %s, want 3.500", " 3.500 ", v)
	}
	if v := ValueFromText(TInt, "1 000", PathPOSIX); v.Type.Code != CodeUnresolved {
		t.Errorf("ValueFromText(int, %q) = %s, want unresolved", "1 000", v)
	}
	lt, err := ParseType("list[float]")
	if err != nil {
		t.Fatal(err)
	}
	v, err := listFromJSON(lt, `[" 1.00 ", "2.50"]`, PathPOSIX)
	if err != nil {
		t.Fatalf("listFromJSON failed: %v", err)
	}
	if got := v.String(); got != "[1.00, 2.50]" {
		t.Errorf("listFromJSON = %s, want [1.00, 2.50]", got)
	}
}
