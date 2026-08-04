// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"testing"
)

// TestTrimAndAffix covers strip/lstrip/rstrip and removeprefix/removesuffix.
//
// The empty-argument rows are the point. RFC 0006 declares an empty argument an
// ERROR for count/find/rfind/index/rindex/replace/split/rsplit and says nothing
// of the kind for these five, so an empty chars, prefix or suffix here is a
// no-op rather than a rejection. Confirmed against the reference.
func TestTrimAndAffix(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"strip whitespace", `strip('  a  ')`, "a"},
		{"strip a tab and newline", `strip('\t a \n')`, "a"},
		{"strip nothing to do", `strip('a')`, "a"},
		{"strip an all-space string", `strip('   ')`, ""},
		{"lstrip whitespace", `lstrip('  a  ')`, "a  "},
		{"rstrip whitespace", `rstrip('  a  ')`, "  a"},
		{"strip a character set", `strip('xxayx', 'xy')`, "a"},
		{"lstrip a character set", `lstrip('xxayx', 'xy')`, "ayx"},
		{"rstrip a character set", `rstrip('xxayx', 'xy')`, "xxa"},
		{"strip an empty set is a no-op", `strip('abc', '')`, "abc"},
		{"rstrip an empty set is a no-op", `rstrip('abc', '')`, "abc"},
		{"removeprefix present", `removeprefix('abcdef', 'abc')`, "def"},
		{"removeprefix absent", `removeprefix('abcdef', 'z')`, "abcdef"},
		{"removeprefix empty is a no-op", `removeprefix('abc', '')`, "abc"},
		{"removesuffix present", `removesuffix('abcdef', 'def')`, "abc"},
		{"removesuffix absent", `removesuffix('abcdef', 'z')`, "abcdef"},
		{"removesuffix empty is a no-op", `removesuffix('abc', '')`, "abc"},
		{"method form", `'  a  '.strip()`, "a"},
		{"method form with an argument", `'xxayx'.strip('xy')`, "a"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Eval(tc.src, MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %q, want %q", tc.src, got, tc.want)
			}
			if got := v.Type.String(); got != "string" {
				t.Errorf("Eval(%q) typed %s, want string", tc.src, got)
			}
		})
	}
}

// TestSearchFunctions covers startswith/endswith/count/find/rfind/index/rindex.
//
// The non-ASCII rows are load-bearing. Indices are CODEPOINT offsets, matching
// len(s) (utf8.RuneCountInString, funcsconv.go) and s[i] (rune-indexed,
// list.go:302). A byte-offset implementation answers 3 for find('héllo','l')
// instead of 2 — a plausible-looking wrong answer that no ASCII test can see.
func TestSearchFunctions(t *testing.T) {
	tests := []struct {
		name     string
		src      string
		want     string
		wantType string
	}{
		{"startswith yes", `startswith('abcdef', 'abc')`, "true", "bool"},
		{"startswith no", `startswith('abcdef', 'z')`, "false", "bool"},
		{"startswith empty is true", `startswith('abc', '')`, "true", "bool"},
		{"endswith yes", `endswith('abcdef', 'def')`, "true", "bool"},
		// endswith's FALSE path, the twin of "startswith no" above — until
		// this row, nothing in the unit table or the oracle corpus exercised
		// endswith returning false for a non-matching suffix.
		{"endswith no", `endswith('abcdef', 'z')`, "false", "bool"},
		{"endswith empty is true", `endswith('abc', '')`, "true", "bool"},
		{"count simple", `count('banana', 'a')`, "3", "int"},
		{"count is non-overlapping", `count('aaaa', 'aa')`, "2", "int"},
		{"count absent", `count('abc', 'z')`, "0", "int"},
		{"count in an empty string", `count('', 'a')`, "0", "int"},
		{"find present", `find('hello', 'l')`, "2", "int"},
		{"find absent", `find('abc', 'z')`, "-1", "int"},
		{"find in an empty string", `find('', 'a')`, "-1", "int"},
		{"rfind present", `rfind('hello', 'l')`, "3", "int"},
		{"rfind absent", `rfind('abc', 'z')`, "-1", "int"},
		{"index present", `index('hello', 'l')`, "2", "int"},
		{"rindex present", `rindex('abcabc', 'b')`, "4", "int"},
		{"find counts codepoints not bytes", `find('héllo', 'l')`, "2", "int"},
		{"rfind counts codepoints not bytes", `rfind('naïve', 'e')`, "4", "int"},
		{"index counts codepoints not bytes", `index('héllo', 'l')`, "2", "int"},
		{"method form", `'hello'.find('l')`, "2", "int"},
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

// TestSearchFunctions_Reject pins the two error conditions RFC 0006 states
// explicitly. Both back conformance fixtures — expr2.2.4--count-empty-substring,
// --find-empty-substring, --index-not-found and --rindex-not-found — which pass
// today only because the function is unknown.
func TestSearchFunctions_Reject(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantErr error
	}{
		{"count rejects an empty substring", `count('abc', '')`, errEmptySubstring},
		{"find rejects an empty substring", `find('abc', '')`, errEmptySubstring},
		{"rfind rejects an empty substring", `rfind('abc', '')`, errEmptySubstring},
		{"index rejects an empty substring", `index('abc', '')`, errEmptySubstring},
		{"rindex rejects an empty substring", `rindex('abc', '')`, errEmptySubstring},
		{"replace rejects an empty old", `replace('abc', '', 'x')`, errEmptySubstring},
		{"index rejects a missing substring", `index('abc', 'z')`, errSubstringNotFound},
		{"rindex rejects a missing substring", `rindex('abc', 'z')`, errSubstringNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Eval(tc.src, MapSymbols{}, TAny)
			if err == nil {
				t.Fatalf("Eval(%q) succeeded; want %v", tc.src, tc.wantErr)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Eval(%q) = %v, want it to wrap %v", tc.src, err, tc.wantErr)
			}
		})
	}
}

func TestReplace(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"replaces every occurrence", `replace('aaa', 'a', 'b')`, "bbb"},
		{"grows", `replace('aaa', 'a', 'bb')`, "bbbbbb"},
		{"shrinks to nothing", `replace('aaa', 'a', '')`, ""},
		{"absent old is a no-op", `replace('abc', 'z', 'y')`, "abc"},
		{"multi-character old", `replace('a.b.c', '.', '/')`, "a/b/c"},
		{"method form", `'a.b'.replace('.', '-')`, "a-b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Eval(tc.src, MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %q, want %q", tc.src, got, tc.want)
			}
		})
	}
}

// TestReplace_BoundsGrowth checks the bound AT the limit rather than near it.
// C1's float-to-int narrowing defect was found this way and only this way: a
// test that stops short of the boundary proves the happy path and nothing else.
func TestReplace_BoundsGrowth(t *testing.T) {
	// 'a' * 1000 replaced by a 20000-byte string is 20,000,000 bytes — twice
	// maxStringBytes — so it must be refused rather than allocated.
	src := `replace('a' * 1000, 'a', 'b' * 20000)`
	_, err := Eval(src, MapSymbols{}, TAny)
	if err == nil {
		t.Fatalf("Eval(%q) succeeded; want it refused as too large", src)
	}
	if !errors.Is(err, errTooLarge) {
		t.Errorf("Eval(%q) = %v, want it to wrap errTooLarge", src, err)
	}
}
