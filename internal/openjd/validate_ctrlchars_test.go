// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"strings"
	"testing"
)

// TestValidateArgStringChars pins the EXPR extension's amendment to the
// <ArgString> type: "The ArgString type is amended to allow CR (U+000D), LF
// (U+000A), and TAB (U+0009) characters to support multi-line expressions in
// YAML literal block scalars" (Expression Language, "When EXPR is enabled").
//
// It is a WHOLE-VALUE amendment. An earlier implementation exempted only the
// insides of a {{ }} — a narrower reading that looks more conservative and is
// wrong: the fixtures this exists to clear are multi-line Python scripts with
// single-line expressions embedded, so their newlines are all in literal text.
// Only the three named characters become legal; every other Cc character is
// still rejected.
func TestValidateArgStringChars(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"plain text", "echo hello", false},
		{"newline in literal text", "line one\nline two", false},
		{"carriage return", "a\rb", false},
		{"tab", "a\tb", false},
		{"CRLF", "a\r\nb", false},
		{"newline inside an expression", "{{ 1 +\n2 }}", false},
		{
			"a multi-line script with embedded expressions, as the fixtures write it",
			"# Addition\nprint(r'{{ Param.X + 3 }}')\n# Subtraction\nprint(r'{{ Param.X - 3 }}')\n",
			false,
		},

		{"NUL is still rejected", "a\x00b", true},
		{"bell is still rejected", "a\ab", true},
		{"escape is still rejected", "a\x1bb", true},
		{"delete is still rejected", "a\x7fb", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := validateArgStringChars(tt.in, "/p")
			if tt.wantErr && len(errs) == 0 {
				t.Errorf("%q was accepted; only CR, LF and TAB are amended in", tt.in)
			}
			if !tt.wantErr && len(errs) != 0 {
				t.Errorf("%q was rejected: %v", tt.in, errs)
			}
		})
	}
}

// TestValidateAction_ArgNewlinesGatedOnEXPR pins that the amendment applies
// ONLY to a template that declares the extension. A base-spec template's
// behavior must be byte-for-byte unchanged, diagnostics included.
func TestValidateAction_ArgNewlinesGatedOnEXPR(t *testing.T) {
	a := Action{Command: "echo", Args: []string{"line one\nline two"}}

	if errs := validateAction(a, "/a", ScopeStepScript, nil, false); !hasCtrlCharError(errs) {
		t.Errorf("a multi-line argument was accepted without extensions: [EXPR]; errors: %v", errs)
	}
	if errs := validateAction(a, "/a", ScopeStepScript, nil, true); hasCtrlCharError(errs) {
		t.Errorf("a multi-line argument was rejected under EXPR: %v", errs)
	}
}

// TestValidateAction_CommandIsNotAmended is the boundary of the amendment, and
// the reason it is worth its own test: <CommandString> and <ArgString> are
// SEPARATE types in the schema (Template Schemas §5.1 and §5.2), and the EXPR
// specification amends only the second. Relaxing both would be easy, symmetric,
// and unauthorized — a command carrying a line break still cannot survive the
// round trip to argv, and nothing in the spec says otherwise.
func TestValidateAction_CommandIsNotAmended(t *testing.T) {
	a := Action{Command: "echo\nhello"}
	for _, exprDeclared := range []bool{false, true} {
		t.Run(map[bool]string{false: "base spec", true: "EXPR"}[exprDeclared], func(t *testing.T) {
			errs := validateAction(a, "/a", ScopeStepScript, nil, exprDeclared)
			if !hasCtrlCharError(errs) {
				t.Errorf("a newline in the COMMAND was accepted (exprDeclared=%v); "+
					"EXPR amends <ArgString>, not <CommandString>", exprDeclared)
			}
		})
	}
}

// hasCtrlCharError reports whether any error is the control-character one, so
// these tests assert on the rule under test rather than on overall acceptance:
// the actions here reference no embedded files and may draw unrelated errors.
func hasCtrlCharError(errs ValidationErrors) bool {
	for _, e := range errs {
		if strings.Contains(e.Message, "control characters") {
			return true
		}
	}
	return false
}
