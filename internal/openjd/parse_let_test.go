// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

// Tests for the EXPR let: block decoder — E3 task 1. These cover parsing
// only: the raw <LetBinding> strings land on StepTemplate.Let, StepScript.Let,
// and EnvironmentScript.Let, with LetSet distinguishing an omitted let: from
// one declared but empty. Nothing reads these fields yet.

import (
	"fmt"
	"slices"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

func TestParse_LetBlock(t *testing.T) {
	const y = `specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: TestJob
jobEnvironments:
- name: Setup
  script:
    let:
    - greeting = Param.Name
    actions:
      onEnter:
        command: echo
        args: ["hi"]
steps:
- name: Step1
  let:
  - a = 1
  - b = a + 1
  script:
    let:
    - c = b * 2
    actions:
      onRun:
        command: echo
        args: ["hi"]
`
	tmpl, err := openjd.Parse([]byte(y), openjd.FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := tmpl.Steps[0].Let, []string{"a = 1", "b = a + 1"}; !slices.Equal(got, want) {
		t.Errorf("step let = %q, want %q", got, want)
	}
	if !tmpl.Steps[0].LetSet {
		t.Error("step LetSet = false, want true")
	}
	if got, want := tmpl.Steps[0].Script.Let, []string{"c = b * 2"}; !slices.Equal(got, want) {
		t.Errorf("script let = %q, want %q", got, want)
	}
	if got, want := tmpl.JobEnvironments[0].Script.Let, []string{"greeting = Param.Name"}; !slices.Equal(got, want) {
		t.Errorf("env let = %q, want %q", got, want)
	}
}

func TestParse_LetEmptyVersusAbsent(t *testing.T) {
	base := `specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: TestJob
steps:
- name: Step1
%s  script:
    actions:
      onRun:
        command: echo
        args: ["hi"]
`
	for _, tc := range []struct {
		name   string
		inject string
		set    bool
		length int
	}{
		{"absent", "", false, 0},
		{"empty list", "  let: []\n", true, 0},
		{"one entry", "  let: [\"a = 1\"]\n", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpl, err := openjd.Parse(fmt.Appendf(nil, base, tc.inject), openjd.FormatYAML)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := tmpl.Steps[0].LetSet; got != tc.set {
				t.Errorf("LetSet = %v, want %v", got, tc.set)
			}
			if got := len(tmpl.Steps[0].Let); got != tc.length {
				t.Errorf("len(Let) = %d, want %d", got, tc.length)
			}
		})
	}
}
