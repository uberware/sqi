// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// Template Schemas §7.3: a format string may only reference variables defined
// at its location. In a template without EXPR, a reference to a job parameter
// that is not declared, or to a task parameter its step does not define, is a
// template error at every position — not a job whose tasks all fail when a
// worker resolves the reference. With EXPR the expression checker reports the
// same reference as an unknown symbol.

const declaredRefsTemplate = `
specificationVersion: jobtemplate-2023-09
name: "Render {{Param.Shot}}"
parameterDefinitions:
  - {name: Shot, type: STRING, default: sh010}
jobEnvironments:
  - name: JobEnv
    variables: {SHOT: "{{Param.Shot}}"}
    script:
      actions:
        onEnter: {command: echo, args: ["{{RawParam.Shot}}"]}
steps:
  - name: Step1
    parameterSpace:
      taskParameterDefinitions:
        - {name: Frame, type: INT, range: "1-3"}
    stepEnvironments:
      - name: StepEnv
        script:
          actions:
            onEnter: {command: echo, args: ["{{Param.Shot}}"]}
    script:
      embeddedFiles:
        - {name: Run, type: TEXT, data: "{{Param.Shot}} {{Task.Param.Frame}}"}
      actions:
        onRun:
          command: "{{Task.File.Run}}"
          args: ["{{Task.Param.Frame}}", "{{Task.RawParam.Frame}}"]
`

func TestValidate_DeclaredReferencesAreAccepted(t *testing.T) {
	if errs := openjd.Validate(mustParse(t, declaredRefsTemplate)); len(errs) != 0 {
		t.Fatalf("Validate() = %v, want no errors", errs)
	}
}

func TestValidate_UndeclaredReferencesAreRejectedEverywhere(t *testing.T) {
	tests := []struct {
		name, yaml, want string
	}{
		{"job name", `
specificationVersion: jobtemplate-2023-09
name: "Render {{Param.Missing}}"
steps:
  - name: Step1
    script: {actions: {onRun: {command: echo}}}
`, "/name"},
		{"onRun command", `
specificationVersion: jobtemplate-2023-09
name: J
steps:
  - name: Step1
    script: {actions: {onRun: {command: "{{Param.Missing}}"}}}
`, "/steps/0/script/actions/onRun/command"},
		{"onRun arg naming an undefined task parameter", `
specificationVersion: jobtemplate-2023-09
name: J
steps:
  - name: Step1
    parameterSpace:
      taskParameterDefinitions:
        - {name: Frame, type: INT, range: "1-3"}
    script: {actions: {onRun: {command: echo, args: ["{{Task.Param.Fram}}"]}}}
`, "/steps/0/script/actions/onRun/args/0"},
		{"a task parameter in a step that has none", `
specificationVersion: jobtemplate-2023-09
name: J
steps:
  - name: Step1
    script: {actions: {onRun: {command: echo, args: ["{{Task.RawParam.Frame}}"]}}}
`, "/steps/0/script/actions/onRun/args/0"},
		{"job environment variable", `
specificationVersion: jobtemplate-2023-09
name: J
jobEnvironments:
  - {name: E, variables: {X: "{{Param.Missing}}"}}
steps:
  - name: Step1
    script: {actions: {onRun: {command: echo}}}
`, "/jobEnvironments/0/variables/X"},
		{"step environment action", `
specificationVersion: jobtemplate-2023-09
name: J
steps:
  - name: Step1
    stepEnvironments:
      - name: E
        script: {actions: {onEnter: {command: "{{RawParam.Missing}}"}}}
    script: {actions: {onRun: {command: echo}}}
`, "/steps/0/stepEnvironments/0/script/actions/onEnter/command"},
		{"embedded file data", `
specificationVersion: jobtemplate-2023-09
name: J
steps:
  - name: Step1
    script:
      embeddedFiles:
        - {name: F, type: TEXT, data: "{{Param.Missing}}"}
      actions: {onRun: {command: "{{Task.File.F}}"}}
`, "/steps/0/script/embeddedFiles/0"},
		{"host requirement value", `
specificationVersion: jobtemplate-2023-09
name: J
steps:
  - name: Step1
    hostRequirements:
      amounts: [{name: amount.worker.vcpu, min: "{{Param.Missing}}"}]
    script: {actions: {onRun: {command: echo}}}
`, "/steps/0/hostRequirements/amounts/0/min"},
		{"range list element", `
specificationVersion: jobtemplate-2023-09
name: J
steps:
  - name: Step1
    parameterSpace:
      taskParameterDefinitions:
        - {name: X, type: INT, range: [1, "{{Param.Missing}}"]}
    script: {actions: {onRun: {command: echo}}}
`, "/steps/0/parameterSpace/taskParameterDefinitions/0/range/1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := mustParse(t, tc.yaml)
			assertValidationContains(t, tmpl, tc.want)
		})
	}
}

// TestValidate_DerivedTaskParameterNamesAreDeclared pins that a name derived
// from a declared task parameter — SQI_CHUNK_BOUNDS's Task.Param.<name>.Start
// and .End — counts as declared by its parameter.
func TestValidate_DerivedTaskParameterNamesAreDeclared(t *testing.T) {
	tmpl := mustParse(t, `
specificationVersion: jobtemplate-2023-09
extensions: [TASK_CHUNKING, SQI_CHUNK_BOUNDS]
name: J
steps:
  - name: Step1
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: CHUNK[INT]
          range: "1-10"
          chunks: {defaultTaskCount: 5, rangeConstraint: CONTIGUOUS}
    script:
      actions:
        onRun: {command: render, args: ["-s", "{{Task.Param.Frame.Start}}", "-e", "{{Task.Param.Frame.End}}"]}
`)
	if errs := openjd.Validate(tmpl); len(errs) != 0 {
		t.Fatalf("Validate() = %v, want no errors", errs)
	}
}
