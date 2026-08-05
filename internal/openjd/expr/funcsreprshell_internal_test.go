// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "testing"

// TestReprSh pins Python's shlex.quote, which RFC 0006 names explicitly as the
// behavior repr_sh follows.
//
// The reference implementation produces DIFFERENT TEXT for the same inputs — a
// mixed strategy splicing double- and single-quoted segments, e.g.
// repr_sh("it's $HOME") gives `"it's "'$HOME'`. That output is shell-equivalent
// and safe, it is simply not what the specification names, so sqi implements
// shlex.quote and the difference is baselined.
func TestReprSh(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"safe string is bare", `repr_sh('hello')`, "hello"},
		{"safe punctuation is bare", `repr_sh('a/b_c-d.e')`, "a/b_c-d.e"},
		{"empty string", `repr_sh('')`, "''"},
		{"space forces quoting", `repr_sh('hello world')`, "'hello world'"},
		{"dollar is quoted not escaped", `repr_sh('plain $HOME')`, "'plain $HOME'"},
		{"single quote is spliced", `repr_sh("it's")`, `'it'"'"'s'`},
		{"non-ascii forces quoting", `repr_sh('café')`, "'café'"},
		{"semicolon forces quoting", `repr_sh('a;b')`, "'a;b'"},
		{"list joins with spaces", `repr_sh(['echo', 'hello world'])`, "echo 'hello world'"},
		{"empty list", `repr_sh([])`, ""},
		{"method form", `'hello world'.repr_sh()`, "'hello world'"},
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

// TestReprCmd covers RFC 0006's cmd.exe rules. Every expectation here is one of
// the specification's own worked examples, and the reference reproduced all of
// them during design, so this function is expected to carry no divergence.
//
// The newline stripping is a SECURITY rule, not formatting: cmd.exe has no
// escape for a literal newline inside a quoted argument, so anything after one
// would be read as a new command.
func TestReprCmd(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"simple string is bare", `repr_cmd('hello')`, "hello"},
		{"ampersand is quoted", `repr_cmd('a & b')`, `"a & b"`},
		{"caret is doubled", `repr_cmd('a ^ b')`, `"a ^^ b"`},
		{"percent is doubled", `repr_cmd('100%')`, `"100%%"`},
		{"quote is caret-escaped", `repr_cmd('say "hi"')`, `"say ^"hi^""`},
		{"bang is double-caret-escaped", `repr_cmd('hello!')`, `"hello^^!"`},
		{"newlines are stripped", `repr_cmd('a\nb')`, "ab"},
		{"list joins with spaces", `repr_cmd(['echo', 'hello & world'])`, `echo "hello & world"`},
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

func TestReprPwsh(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"string is single quoted", `repr_pwsh('hello')`, "'hello'"},
		{"embedded quote is doubled", `repr_pwsh('it\'s')`, "'it''s'"},
		{"int is bare", `repr_pwsh(42)`, "42"},
		{"negative int is bare", `repr_pwsh(-1)`, "-1"},
		{"float is bare", `repr_pwsh(1.5)`, "1.5"},
		{"true", `repr_pwsh(true)`, "$true"},
		{"false", `repr_pwsh(false)`, "$false"},
		{"list becomes an array literal", `repr_pwsh(['a', 'b'])`, "@('a', 'b')"},
		{"int list", `repr_pwsh([1, 2])`, "@(1, 2)"},
		{"empty list", `repr_pwsh([])`, "@()"},
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

// TestReprShell_PathAndRange covers the rows that need a symbol table, since
// path() and a range_expr literal are not available before sub-project C4.
func TestReprShell_PathAndRange(t *testing.T) {
	syms := MapSymbols{
		"Param.Dir":    Value{Type: TPath, s: "/a b/c"},
		"Param.Frames": Value{Type: TRangeExpr, s: "1-10"},
	}
	tests := []struct{ src, want string }{
		{`repr_sh(Param.Dir)`, "'/a b/c'"},
		{`repr_pwsh(Param.Dir)`, "'/a b/c'"},
		{`repr_pwsh(Param.Frames)`, "'1-10'"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, syms, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %q, want %q", tc.src, got, tc.want)
			}
		})
	}
}
