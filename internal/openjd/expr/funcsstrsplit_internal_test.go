// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"testing"
)

// TestSplitAndJoin covers RFC 0006's split, rsplit and join.
//
// Three rows are specification text rather than intuition:
//   - split(”, ',') is [""], one empty string, NOT an empty list. RFC 0006
//     states this explicitly and calls out that it matches Python.
//   - split('   ') with no separator is [], because the whitespace form strips
//     the ends before splitting.
//   - a NEGATIVE maxsplit means unlimited. The reference answers [] instead,
//     which is a defect; two baselined divergences record it.
func TestSplitAndJoin(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"split on whitespace", `split('a b c')`, "[a, b, c]"},
		{"split collapses whitespace runs", `split('a b  c')`, "[a, b, c]"},
		{"split trims the ends", `split('  a b  ')`, "[a, b]"},
		{"split an all-space string", `split('   ')`, "[]"},
		{"split on a separator", `split('a,b,c', ',')`, "[a, b, c]"},
		{"split keeps empty fields", `split('a,b,,c', ',')`, "[a, b, , c]"},
		{"split an empty string", `split('', ',')`, "[]"},
		{"split with maxsplit", `split('a,b,c', ',', 1)`, "[a, b,c]"},
		{"split with maxsplit zero", `split('a,b,c', ',', 0)`, "[a,b,c]"},
		{"split with a negative maxsplit is unlimited", `split('a,b,c', ',', -1)`, "[a, b, c]"},
		{"split with maxsplit past the end", `split('a,b,c', ',', 100)`, "[a, b, c]"},
		{"rsplit on whitespace", `rsplit('  a b  ')`, "[a, b]"},
		{"rsplit on a separator", `rsplit('a,b,c', ',')`, "[a, b, c]"},
		{"rsplit with maxsplit takes from the right", `rsplit('a b c', ' ', 1)`, "[a b, c]"},
		{"rsplit with a negative maxsplit is unlimited", `rsplit('a b c', ' ', -1)`, "[a, b, c]"},
		{"join", `join(['a', 'b', 'c'], ',')`, "a,b,c"},
		{"join with an empty separator", `join(['a', 'b'], '')`, "ab"},
		{"join a single element", `join(['a'], ',')`, "a"},
		{"join an empty list", `join([], '-')`, ""},
		{"round trip", `join(split('a;b;c', ';'), ',')`, "a,b,c"},
		{"method form", `'a,b,c'.split(',').join(';')`, "a;b;c"},
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

// TestSplit_EmptyStringIsOneEmptyField isolates RFC 0006's most
// counter-intuitive split rule from the table above so a regression names it.
func TestSplit_EmptyStringIsOneEmptyField(t *testing.T) {
	v, err := Eval(`len(split('', ','))`, MapSymbols{}, TAny)
	if err != nil {
		t.Fatalf("Eval failed: %v", err)
	}
	if got := v.String(); got != "1" {
		t.Errorf(`len(split('', ',')) = %s, want 1 — the result is [""], not []`, got)
	}
}

func TestSplit_RejectsAnEmptySeparator(t *testing.T) {
	for _, src := range []string{`split('a,b', '')`, `rsplit('a,b', '')`, `split('a,b', '', 1)`} {
		t.Run(src, func(t *testing.T) {
			_, err := Eval(src, MapSymbols{}, TAny)
			if err == nil {
				t.Fatalf("Eval(%q) succeeded; want an empty-separator error", src)
			}
			if !errors.Is(err, errEmptySeparator) {
				t.Errorf("Eval(%q) = %v, want it to wrap errEmptySeparator", src, err)
			}
		})
	}
}

// TestJoin_SelectsEmptyListRow pins that [].join(sep) reaches the
// list[nulltype] row and returns "" rather than failing to match.
//
// It exists because row ORDER was load-bearing in C1's flatten() and nothing
// warned about it: [] scores costExact against BOTH list[nulltype] and
// list[string] would-be rivals, and matchShapesExactFirst breaks an exact tie
// to the earliest shape. This asserts the outcome is right regardless.
func TestJoin_SelectsEmptyListRow(t *testing.T) {
	for _, src := range []string{`join([], ',')`, `[].join(',')`} {
		t.Run(src, func(t *testing.T) {
			v, err := Eval(src, MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", src, err)
			}
			if got := v.String(); got != "" {
				t.Errorf("Eval(%q) = %q, want the empty string", src, got)
			}
			if got := v.Type.String(); got != "string" {
				t.Errorf("Eval(%q) typed %s, want string", src, got)
			}
		})
	}
}

// TestJoin_AcceptsAPathList covers RFC 0006's third join row. A path list
// cannot be built by a literal before sub-project C4 ships path(), so the
// values come from the symbol table.
func TestJoin_AcceptsAPathList(t *testing.T) {
	syms := MapSymbols{"Param.Dirs": List(TPath, []Value{
		{Type: TPath, s: "/a"},
		{Type: TPath, s: "/b"},
	})}
	v, err := Eval(`join(Param.Dirs, ':')`, syms, TAny)
	if err != nil {
		t.Fatalf("Eval failed: %v", err)
	}
	if got := v.String(); got != "/a:/b" {
		t.Errorf("join(Param.Dirs, ':') = %q, want %q", got, "/a:/b")
	}
}
