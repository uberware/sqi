// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import "testing"

func TestValidate_OnRunCommandRequired(t *testing.T) {
	yaml := `
specificationVersion: jobtemplate-2023-09
name: NoCommandJob
steps:
  - name: Step1
    script:
      actions:
        onRun:
          args: ["nothing"]
`
	tmpl := mustParse(t, yaml)
	assertValidationContains(t, tmpl, "/steps/0/script/actions/onRun/command")
}

func TestValidate_EnvironmentActionCommandRequired(t *testing.T) {
	yaml := `
specificationVersion: jobtemplate-2023-09
name: EnvNoCommandJob
jobEnvironments:
  - name: Setup
    script:
      actions:
        onEnter:
          args: ["nothing"]
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`
	tmpl := mustParse(t, yaml)
	assertValidationContains(t, tmpl, "/jobEnvironments/0/script/actions/onEnter/command")
}
