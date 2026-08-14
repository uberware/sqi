// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import "testing"

// TestDecodeJobParameter_CanonicalizesOnlyUnderEXPR is the guard on RFC 0007's
// blast radius. Case-insensitivity touches the type field of every parameter
// in every template, so the gate is what keeps base-spec behavior identical:
// without extensions: [EXPR], "int" must stay "int" and be rejected downstream
// as an unknown type, exactly as it is today.
func TestDecodeJobParameter_CanonicalizesOnlyUnderEXPR(t *testing.T) {
	tests := []struct {
		name         string
		exprDeclared bool
		raw          string
		want         JobParamType
	}{
		{"expr canonicalizes lowercase", true, "int", JobParamTypeInt},
		{"expr canonicalizes mixed case", true, "pAtH", JobParamTypePath},
		{"expr canonicalizes a list type", true, "List[Int]", JobParamTypeListInt},
		{"expr accepts bool", true, "bool", JobParamTypeBool},
		{"expr accepts range_expr", true, "range_expr", JobParamTypeRangeExpr},
		{"expr leaves an unknown type verbatim", true, "map[string]", JobParamType("map[string]")},
		{"base spec leaves lowercase verbatim", false, "int", JobParamType("int")},
		{"base spec leaves bool verbatim", false, "BOOL", JobParamType("BOOL")},
		{"base spec leaves a list type verbatim", false, "LIST[INT]", JobParamType("LIST[INT]")},
		{"uppercase base type is unchanged either way", false, "INT", JobParamTypeInt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := decodeJobParameter(map[string]any{
				"name": "P",
				"type": tt.raw,
			}, tt.exprDeclared)
			if err != nil {
				t.Fatalf("decodeJobParameter: %v", err)
			}
			if p.Type != tt.want {
				t.Errorf("Type = %q, want %q", p.Type, tt.want)
			}
		})
	}
}

// TestDecodeTaskParamDefinition_CanonicalizesOnlyUnderEXPR is the task-side
// half of the same gate. RFC 0007 makes task type names case-insensitive too,
// but adds no task types -- so a list type stays verbatim (and is rejected
// downstream) even under EXPR.
func TestDecodeTaskParamDefinition_CanonicalizesOnlyUnderEXPR(t *testing.T) {
	tests := []struct {
		name         string
		exprDeclared bool
		raw          string
		want         TaskParamType
	}{
		{"expr canonicalizes lowercase", true, "int", TaskParamTypeInt},
		{"expr canonicalizes chunk", true, "chunk[int]", TaskParamTypeChunkInt},
		{"expr leaves a job-only list type verbatim", true, "list[int]", TaskParamType("list[int]")},
		{"base spec leaves lowercase verbatim", false, "int", TaskParamType("int")},
		{"uppercase is unchanged either way", false, "CHUNK[INT]", TaskParamTypeChunkInt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tp, err := decodeTaskParamDefinition(map[string]any{
				"name": "T",
				"type": tt.raw,
			}, tt.exprDeclared)
			if err != nil {
				t.Fatalf("decodeTaskParamDefinition: %v", err)
			}
			if tp.Type != tt.want {
				t.Errorf("Type = %q, want %q", tp.Type, tt.want)
			}
		})
	}
}

// TestValidate_BaseSpecStillRejectsLowercaseType is the end-to-end half of the
// guard, driven through the real Parse + Validate path rather than the decoder
// alone: a base-spec template using "int" must still be rejected.
func TestValidate_BaseSpecStillRejectsLowercaseType(t *testing.T) {
	tmpl, err := Parse([]byte(`
specificationVersion: jobtemplate-2023-09
name: T
parameterDefinitions:
- name: P
  type: int
steps:
- name: S
  script:
    actions:
      onRun:
        command: echo
`), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	errs := Validate(tmpl)
	if len(errs) == 0 {
		t.Fatal("base-spec template with type: int was accepted; RFC 0007's " +
			"case-insensitivity must be gated on extensions: [EXPR]")
	}
}

// TestValidate_EXPRAcceptsLowercaseBaseType is its mirror: the same template
// with the extension declared must get past the unknown-type check. It asserts
// the ABSENCE of a type error rather than overall acceptance, because the
// EXPR extension is registered-but-unsupported and the status gate rejects the
// template for an unrelated reason (see internal/openjd/extension.go).
func TestValidate_EXPRAcceptsLowercaseBaseType(t *testing.T) {
	tmpl, err := Parse([]byte(`
specificationVersion: jobtemplate-2023-09
extensions:
- EXPR
name: T
parameterDefinitions:
- name: P
  type: int
steps:
- name: S
  script:
    actions:
      onRun:
        command: echo
`), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, e := range Validate(tmpl) {
		if e.Pointer == "/parameterDefinitions/0/type" {
			t.Fatalf("lowercase type rejected under EXPR: %s", e.Message)
		}
	}
}
