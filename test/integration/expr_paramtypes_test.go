// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

import (
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// TestEXPRParamTypes_BindAndCarry walks a template carrying every RFC 0007
// parameter type through parse and binding, and asserts the bound values are
// the canonical text the store, the wire and expr.ValueFromText all consume.
//
// IT DOES NOT GO THROUGH THE HTTP GATE. An earlier revision of this comment
// added "and cannot", because validateExtensions rejected extensions: [EXPR] at
// /extensions/0 while the registry entry was StatusInProgress — that is no
// longer true: EXPR sub-project H2 flipped it to StatusSupported, and
// TestEXPRJobEndToEnd (expr_realworker_test.go) now submits an EXPR job through
// POST /api/v1/jobs and lets a real sqi-worker resolve its expressions, as does
// scripts/smoke.sh. This test stays at the binding layer on purpose: it asserts
// the canonical TEXT of every RFC 0007 parameter type, which a job's logs cannot
// show.
func TestEXPRParamTypes_BindAndCarry(t *testing.T) {
	const tmplYAML = `
specificationVersion: jobtemplate-2023-09
extensions:
- EXPR
name: ParamTypes
parameterDefinitions:
- name: Flag
  type: BOOL
  default: true
- name: Frames
  type: RANGE_EXPR
  default: "1-10"
- name: Cameras
  type: LIST[STRING]
  default: ["main", "closeup"]
- name: Textures
  type: LIST[PATH]
  default: ["/proj/a.exr"]
  dataFlow: IN
  objectType: FILE
- name: Counts
  type: LIST[INT]
  default: [1, 2]
steps:
- name: S
  script:
    actions:
      onRun:
        command: echo
        args: ["{{ Param.Cameras[0] }}"]
`

	tmpl, err := openjd.Parse([]byte(tmplYAML), openjd.FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	bound, errs := openjd.BindJobParameters(tmpl.ParameterDefinitions, nil)
	if len(errs) != 0 {
		t.Fatalf("BindJobParameters: %v", errs)
	}

	want := map[string]string{
		"Flag":     "true",
		"Frames":   "1-10",
		"Cameras":  `["main","closeup"]`,
		"Textures": `["/proj/a.exr"]`,
		"Counts":   "[1,2]",
	}
	for name, w := range want {
		if bound[name] != w {
			t.Errorf("bound %s = %q, want %q", name, bound[name], w)
		}
	}
}

// TestEXPRParamTypes_SubmittedListValuesAreValidated pins that a submitted
// value is checked, not merely carried: the encoding is a string, so nothing
// but this validation stands between a typo and a job that fails on the host.
func TestEXPRParamTypes_SubmittedListValuesAreValidated(t *testing.T) {
	params := []openjd.JobParameter{
		{Name: "Counts", Type: openjd.JobParamTypeListInt},
	}
	if _, errs := openjd.BindJobParameters(params, map[string]string{
		"Counts": "[1,2]",
	}); len(errs) != 0 {
		t.Errorf("a valid list value was rejected: %v", errs)
	}
	if _, errs := openjd.BindJobParameters(params, map[string]string{
		"Counts": `["not","ints"]`,
	}); len(errs) == 0 {
		t.Error("a list of strings was accepted for a LIST[INT] parameter")
	}
}
