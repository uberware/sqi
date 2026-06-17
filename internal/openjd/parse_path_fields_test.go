// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

// Tests for parsing objectType and dataFlow fields on PATH job parameters.
// These two fields are defined in the OpenJD jobtemplate-2023-09 spec and are
// PATH-exclusive; they drive input/output semantics for path mapping.

import (
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

func TestParse_YAML_PathParam_ObjectTypeAndDataFlow(t *testing.T) {
	cases := []struct {
		name           string
		yaml           string
		wantObjectType openjd.PathObjectType
		wantDataFlow   openjd.PathDataFlow
	}{
		{
			name: "objectType FILE and dataFlow INOUT are parsed",
			yaml: `
specificationVersion: jobtemplate-2023-09
name: Job
parameterDefinitions:
  - name: Output
    type: PATH
    objectType: FILE
    dataFlow: INOUT
steps:
  - name: S
    script:
      actions:
        onRun:
          command: run
`,
			wantObjectType: openjd.PathObjectTypeFile,
			wantDataFlow:   openjd.PathDataFlowInOut,
		},
		{
			name: "absent objectType and dataFlow parse as empty/unset",
			yaml: `
specificationVersion: jobtemplate-2023-09
name: Job
parameterDefinitions:
  - name: Output
    type: PATH
steps:
  - name: S
    script:
      actions:
        onRun:
          command: run
`,
			wantObjectType: "",
			wantDataFlow:   "",
		},
		{
			name: "objectType DIRECTORY parses correctly",
			yaml: `
specificationVersion: jobtemplate-2023-09
name: Job
parameterDefinitions:
  - name: WorkDir
    type: PATH
    objectType: DIRECTORY
steps:
  - name: S
    script:
      actions:
        onRun:
          command: run
`,
			wantObjectType: openjd.PathObjectTypeDirectory,
			wantDataFlow:   "",
		},
		{
			name: "dataFlow IN parses correctly",
			yaml: `
specificationVersion: jobtemplate-2023-09
name: Job
parameterDefinitions:
  - name: SceneFile
    type: PATH
    dataFlow: IN
steps:
  - name: S
    script:
      actions:
        onRun:
          command: run
`,
			wantObjectType: "",
			wantDataFlow:   openjd.PathDataFlowIn,
		},
		{
			name: "dataFlow OUT parses correctly",
			yaml: `
specificationVersion: jobtemplate-2023-09
name: Job
parameterDefinitions:
  - name: RenderOutput
    type: PATH
    dataFlow: OUT
steps:
  - name: S
    script:
      actions:
        onRun:
          command: run
`,
			wantObjectType: "",
			wantDataFlow:   openjd.PathDataFlowOut,
		},
		{
			name: "dataFlow NONE parses correctly",
			yaml: `
specificationVersion: jobtemplate-2023-09
name: Job
parameterDefinitions:
  - name: CachePath
    type: PATH
    dataFlow: NONE
steps:
  - name: S
    script:
      actions:
        onRun:
          command: run
`,
			wantObjectType: "",
			wantDataFlow:   openjd.PathDataFlowNone,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := mustParse(t, tc.yaml)
			if len(tmpl.ParameterDefinitions) != 1 {
				t.Fatalf("parameterDefinitions: got %d, want 1", len(tmpl.ParameterDefinitions))
			}
			p := tmpl.ParameterDefinitions[0]
			if p.ObjectType != tc.wantObjectType {
				t.Errorf("ObjectType: got %q, want %q", p.ObjectType, tc.wantObjectType)
			}
			if p.DataFlow != tc.wantDataFlow {
				t.Errorf("DataFlow: got %q, want %q", p.DataFlow, tc.wantDataFlow)
			}
		})
	}
}

// TestParse_YAML_PathParam_NonPathFieldsUnaffected confirms that objectType and
// dataFlow fields on non-PATH parameters are also parsed verbatim (validation
// catches the type mismatch; parsing is neutral).
func TestParse_YAML_PathParam_NonPathFieldsUnaffected(t *testing.T) {
	yaml := `
specificationVersion: jobtemplate-2023-09
name: Job
parameterDefinitions:
  - name: Count
    type: INT
steps:
  - name: S
    script:
      actions:
        onRun:
          command: run
`
	tmpl := mustParse(t, yaml)
	p := tmpl.ParameterDefinitions[0]
	if p.ObjectType != "" {
		t.Errorf("INT param ObjectType: got %q, want empty", p.ObjectType)
	}
	if p.DataFlow != "" {
		t.Errorf("INT param DataFlow: got %q, want empty", p.DataFlow)
	}
}
