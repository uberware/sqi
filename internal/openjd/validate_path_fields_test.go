// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

// Tests for objectType and dataFlow validation in validateJobParams.
//
// Covered here:
//   - Valid objectType values (FILE, DIRECTORY) on PATH params pass
//   - Valid dataFlow values (NONE, IN, OUT, INOUT) on PATH params pass
//   - Both fields absent on a PATH param is valid
//   - Invalid objectType value → error at /parameterDefinitions/<i>/objectType
//   - Invalid dataFlow value → error at /parameterDefinitions/<i>/dataFlow
//   - objectType on a non-PATH parameter → error at /parameterDefinitions/<i>/objectType
//   - dataFlow on a non-PATH parameter → error at /parameterDefinitions/<i>/dataFlow

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

func TestValidate_PathParam_ObjectTypeAndDataFlow(t *testing.T) {
	type tc struct {
		name    string
		param   openjd.JobParameter
		wantErr bool
		errPtr  string // exact JSON pointer expected in one of the errors
		errMsg  string // substring expected in that error's message
	}

	cases := []tc{
		// ── valid baselines ───────────────────────────────────────────────────────

		{
			name: "PATH with objectType FILE is valid",
			param: openjd.JobParameter{
				Name:       "Output",
				Type:       openjd.JobParamTypePath,
				ObjectType: openjd.PathObjectTypeFile,
			},
		},
		{
			name: "PATH with objectType DIRECTORY is valid",
			param: openjd.JobParameter{
				Name:       "Output",
				Type:       openjd.JobParamTypePath,
				ObjectType: openjd.PathObjectTypeDirectory,
			},
		},
		{
			name: "PATH with dataFlow NONE is valid",
			param: openjd.JobParameter{
				Name:     "Output",
				Type:     openjd.JobParamTypePath,
				DataFlow: openjd.PathDataFlowNone,
			},
		},
		{
			name: "PATH with dataFlow IN is valid",
			param: openjd.JobParameter{
				Name:     "SceneFile",
				Type:     openjd.JobParamTypePath,
				DataFlow: openjd.PathDataFlowIn,
			},
		},
		{
			name: "PATH with dataFlow OUT is valid",
			param: openjd.JobParameter{
				Name:     "RenderOutput",
				Type:     openjd.JobParamTypePath,
				DataFlow: openjd.PathDataFlowOut,
			},
		},
		{
			name: "PATH with dataFlow INOUT is valid",
			param: openjd.JobParameter{
				Name:     "CacheDir",
				Type:     openjd.JobParamTypePath,
				DataFlow: openjd.PathDataFlowInOut,
			},
		},
		{
			name: "PATH with both objectType FILE and dataFlow INOUT is valid",
			param: openjd.JobParameter{
				Name:       "Output",
				Type:       openjd.JobParamTypePath,
				ObjectType: openjd.PathObjectTypeFile,
				DataFlow:   openjd.PathDataFlowInOut,
			},
		},
		{
			name: "PATH with neither objectType nor dataFlow is valid",
			param: openjd.JobParameter{
				Name: "Output",
				Type: openjd.JobParamTypePath,
			},
		},

		// ── invalid objectType value ──────────────────────────────────────────────

		{
			name: "PATH with invalid objectType BOGUS is an error",
			param: openjd.JobParameter{
				Name:       "Output",
				Type:       openjd.JobParamTypePath,
				ObjectType: "BOGUS",
			},
			wantErr: true,
			errPtr:  "/parameterDefinitions/0/objectType",
			errMsg:  "FILE",
		},
		{
			name: "PATH with objectType lowercase file is an error",
			param: openjd.JobParameter{
				Name:       "Output",
				Type:       openjd.JobParamTypePath,
				ObjectType: "file",
			},
			wantErr: true,
			errPtr:  "/parameterDefinitions/0/objectType",
			errMsg:  "FILE",
		},

		// ── invalid dataFlow value ────────────────────────────────────────────────

		{
			name: "PATH with invalid dataFlow SIDEWAYS is an error",
			param: openjd.JobParameter{
				Name:     "Output",
				Type:     openjd.JobParamTypePath,
				DataFlow: "SIDEWAYS",
			},
			wantErr: true,
			errPtr:  "/parameterDefinitions/0/dataFlow",
			errMsg:  "NONE",
		},
		{
			name: "PATH with dataFlow lowercase in is an error",
			param: openjd.JobParameter{
				Name:     "Output",
				Type:     openjd.JobParamTypePath,
				DataFlow: "in",
			},
			wantErr: true,
			errPtr:  "/parameterDefinitions/0/dataFlow",
			errMsg:  "NONE",
		},

		// ── objectType on non-PATH parameters ─────────────────────────────────────

		{
			name: "STRING with objectType FILE is an error",
			param: openjd.JobParameter{
				Name:       "Label",
				Type:       openjd.JobParamTypeString,
				ObjectType: openjd.PathObjectTypeFile,
			},
			wantErr: true,
			errPtr:  "/parameterDefinitions/0/objectType",
			errMsg:  "PATH",
		},
		{
			name: "INT with objectType DIRECTORY is an error",
			param: openjd.JobParameter{
				Name:       "Count",
				Type:       openjd.JobParamTypeInt,
				ObjectType: openjd.PathObjectTypeDirectory,
			},
			wantErr: true,
			errPtr:  "/parameterDefinitions/0/objectType",
			errMsg:  "PATH",
		},
		{
			name: "FLOAT with objectType FILE is an error",
			param: openjd.JobParameter{
				Name:       "Scale",
				Type:       openjd.JobParamTypeFloat,
				ObjectType: openjd.PathObjectTypeFile,
			},
			wantErr: true,
			errPtr:  "/parameterDefinitions/0/objectType",
			errMsg:  "PATH",
		},

		// ── dataFlow on non-PATH parameters ──────────────────────────────────────

		{
			name: "STRING with dataFlow IN is an error",
			param: openjd.JobParameter{
				Name:     "Label",
				Type:     openjd.JobParamTypeString,
				DataFlow: openjd.PathDataFlowIn,
			},
			wantErr: true,
			errPtr:  "/parameterDefinitions/0/dataFlow",
			errMsg:  "PATH",
		},
		{
			name: "INT with dataFlow OUT is an error",
			param: openjd.JobParameter{
				Name:     "Count",
				Type:     openjd.JobParamTypeInt,
				DataFlow: openjd.PathDataFlowOut,
			},
			wantErr: true,
			errPtr:  "/parameterDefinitions/0/dataFlow",
			errMsg:  "PATH",
		},
		{
			name: "FLOAT with dataFlow INOUT is an error",
			param: openjd.JobParameter{
				Name:     "Scale",
				Type:     openjd.JobParamTypeFloat,
				DataFlow: openjd.PathDataFlowInOut,
			},
			wantErr: true,
			errPtr:  "/parameterDefinitions/0/dataFlow",
			errMsg:  "PATH",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := mustParse(t, minimalValidYAML())
			tmpl.ParameterDefinitions = []openjd.JobParameter{tc.param}
			errs := openjd.Validate(tmpl)

			if !tc.wantErr {
				if len(errs) != 0 {
					t.Errorf("expected no errors; got: %v", errs)
				}
				return
			}

			if len(errs) == 0 {
				t.Fatalf("expected a validation error; got none")
			}

			found := false
			for _, e := range errs {
				if e.Pointer == tc.errPtr && strings.Contains(e.Message, tc.errMsg) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected error at pointer %q containing %q; got:\n%v",
					tc.errPtr, tc.errMsg, errs)
			}
		})
	}
}

// fileFilters and fileFilterDefault live under `userInterface`, not at the
// parameter root (schema §2.2). sqi decoded them from the root, so a
// conforming template's filters were silently dropped and every validation
// rule for them — label required, the §2.8 pattern grammar, the 20-filter cap
// — was unreachable dead code. Ten conformance fixtures passed validation that
// should have been rejected.
//
// This asserts the filters are actually decoded from the right place; without
// it, a future refactor could quietly reinstate the silent drop.
func TestParse_FileFiltersDecodedFromUserInterface(t *testing.T) {
	tmpl := mustParse(t, `
specificationVersion: jobtemplate-2023-09
name: FileFilterJob
parameterDefinitions:
  - name: MyPath
    type: PATH
    objectType: FILE
    userInterface:
      control: CHOOSE_INPUT_FILE
      fileFilters:
        - label: Images
          patterns: ["*.png", "*.exr"]
      fileFilterDefault:
        label: All
        patterns: ["*"]
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`)
	p := tmpl.ParameterDefinitions[0]
	if len(p.FileFilters) != 1 {
		t.Fatalf("fileFilters not decoded from userInterface: got %d", len(p.FileFilters))
	}
	if got := p.FileFilters[0].Label; got != "Images" {
		t.Errorf("label = %q, want %q", got, "Images")
	}
	if len(p.FileFilters[0].Patterns) != 2 {
		t.Errorf("patterns = %v, want 2 entries", p.FileFilters[0].Patterns)
	}
	if p.FileFilterDefault == nil {
		t.Fatal("fileFilterDefault not decoded from userInterface")
	}
	if got := p.FileFilterDefault.Label; got != "All" {
		t.Errorf("fileFilterDefault label = %q, want %q", got, "All")
	}
	if errs := openjd.Validate(tmpl); len(errs) != 0 {
		t.Errorf("expected a valid template, got %v", errs)
	}
}
