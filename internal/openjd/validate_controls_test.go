// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// pathParamTemplate builds a template with one PATH job parameter carrying the
// given userInterface control.
func pathParamTemplate(control string) string {
	return `
specificationVersion: jobtemplate-2023-09
name: PathControlJob
parameterDefinitions:
  - name: ScenePath
    type: PATH
    objectType: FILE
    dataFlow: IN
    userInterface: { control: ` + control + `, label: Scene }
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`
}

func TestValidate_ChooserControlsAccepted(t *testing.T) {
	for _, control := range []string{"CHOOSE_INPUT_FILE", "CHOOSE_OUTPUT_FILE", "CHOOSE_DIRECTORY"} {
		t.Run(control, func(t *testing.T) {
			tmpl := mustParse(t, pathParamTemplate(control))
			if errs := openjd.Validate(tmpl); len(errs) > 0 {
				t.Fatalf("expected %s to be valid on a PATH parameter; got: %v", control, errs)
			}
		})
	}
}
