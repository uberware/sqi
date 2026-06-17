// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

// Tests for EmbeddedFile.Type validation.
//
// Covered here:
//   - type "TEXT" passes for step-script embedded files
//   - missing type produces a ValidationError at /steps/<i>/script/embeddedFiles/<j>/type
//   - unsupported type produces a ValidationError with the right message
//   - same three cases for job-environment-script embedded files
//   - same three cases for step-environment-script embedded files

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// ── step-script embedded files ────────────────────────────────────────────────

func TestValidate_EmbeddedFile_StepScript(t *testing.T) {
	type tc struct {
		name    string
		ef      openjd.EmbeddedFile
		wantErr bool
		errPtr  string
		errMsg  string
	}
	cases := []tc{
		{
			name: "type TEXT is valid",
			ef:   openjd.EmbeddedFile{Name: "f", Type: openjd.EmbeddedFileTypeText, Data: "x"},
		},
		{
			name:    "absent type is an error",
			ef:      openjd.EmbeddedFile{Name: "f", Data: "x"},
			wantErr: true,
			errPtr:  "/steps/0/script/embeddedFiles/0/type",
			errMsg:  "required",
		},
		{
			name:    "type BINARY is an error",
			ef:      openjd.EmbeddedFile{Name: "f", Type: "BINARY", Data: "x"},
			wantErr: true,
			errPtr:  "/steps/0/script/embeddedFiles/0/type",
			errMsg:  "unsupported",
		},
		{
			name:    "type text (lowercase) is an error",
			ef:      openjd.EmbeddedFile{Name: "f", Type: "text", Data: "x"},
			wantErr: true,
			errPtr:  "/steps/0/script/embeddedFiles/0/type",
			errMsg:  "unsupported",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := mustParse(t, minimalValidYAML())
			tmpl.Steps[0].Script = &openjd.StepScript{
				EmbeddedFiles: []openjd.EmbeddedFile{tc.ef},
				Actions:       openjd.StepActions{OnRun: openjd.Action{Command: "echo"}},
			}
			errs := openjd.Validate(tmpl)

			if !tc.wantErr {
				if len(errs) != 0 {
					t.Errorf("expected no errors; got: %v", errs)
				}
				return
			}
			for _, e := range errs {
				if e.Pointer == tc.errPtr && strings.Contains(e.Message, tc.errMsg) {
					return
				}
			}
			t.Errorf("expected error at %q containing %q; got: %v", tc.errPtr, tc.errMsg, errs)
		})
	}
}

// ── job-environment-script embedded files ─────────────────────────────────────

func TestValidate_EmbeddedFile_JobEnvScript(t *testing.T) {
	type tc struct {
		name    string
		ef      openjd.EmbeddedFile
		wantErr bool
		errPtr  string
		errMsg  string
	}
	cases := []tc{
		{
			name: "type TEXT is valid",
			ef:   openjd.EmbeddedFile{Name: "f", Type: openjd.EmbeddedFileTypeText, Data: "x"},
		},
		{
			name:    "absent type is an error",
			ef:      openjd.EmbeddedFile{Name: "f", Data: "x"},
			wantErr: true,
			errPtr:  "/jobEnvironments/0/script/embeddedFiles/0/type",
			errMsg:  "required",
		},
		{
			name:    "type BINARY is an error",
			ef:      openjd.EmbeddedFile{Name: "f", Type: "BINARY", Data: "x"},
			wantErr: true,
			errPtr:  "/jobEnvironments/0/script/embeddedFiles/0/type",
			errMsg:  "unsupported",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := mustParse(t, minimalValidYAML())
			tmpl.JobEnvironments = []openjd.Environment{{
				Name: "Env1",
				Script: &openjd.EnvironmentScript{
					EmbeddedFiles: []openjd.EmbeddedFile{tc.ef},
					Actions:       openjd.EnvironmentActions{OnEnter: &openjd.Action{Command: "echo"}},
				},
			}}
			errs := openjd.Validate(tmpl)

			if !tc.wantErr {
				if len(errs) != 0 {
					t.Errorf("expected no errors; got: %v", errs)
				}
				return
			}
			for _, e := range errs {
				if e.Pointer == tc.errPtr && strings.Contains(e.Message, tc.errMsg) {
					return
				}
			}
			t.Errorf("expected error at %q containing %q; got: %v", tc.errPtr, tc.errMsg, errs)
		})
	}
}

// ── step-environment-script embedded files ────────────────────────────────────

func TestValidate_EmbeddedFile_StepEnvScript(t *testing.T) {
	type tc struct {
		name    string
		ef      openjd.EmbeddedFile
		wantErr bool
		errPtr  string
		errMsg  string
	}
	cases := []tc{
		{
			name: "type TEXT is valid",
			ef:   openjd.EmbeddedFile{Name: "f", Type: openjd.EmbeddedFileTypeText, Data: "x"},
		},
		{
			name:    "absent type is an error",
			ef:      openjd.EmbeddedFile{Name: "f", Data: "x"},
			wantErr: true,
			errPtr:  "/steps/0/stepEnvironments/0/script/embeddedFiles/0/type",
			errMsg:  "required",
		},
		{
			name:    "type BINARY is an error",
			ef:      openjd.EmbeddedFile{Name: "f", Type: "BINARY", Data: "x"},
			wantErr: true,
			errPtr:  "/steps/0/stepEnvironments/0/script/embeddedFiles/0/type",
			errMsg:  "unsupported",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := mustParse(t, minimalValidYAML())
			tmpl.Steps[0].StepEnvironments = []openjd.Environment{{
				Name: "StepEnv1",
				Script: &openjd.EnvironmentScript{
					EmbeddedFiles: []openjd.EmbeddedFile{tc.ef},
					Actions:       openjd.EnvironmentActions{OnEnter: &openjd.Action{Command: "echo"}},
				},
			}}
			errs := openjd.Validate(tmpl)

			if !tc.wantErr {
				if len(errs) != 0 {
					t.Errorf("expected no errors; got: %v", errs)
				}
				return
			}
			for _, e := range errs {
				if e.Pointer == tc.errPtr && strings.Contains(e.Message, tc.errMsg) {
					return
				}
			}
			t.Errorf("expected error at %q containing %q; got: %v", tc.errPtr, tc.errMsg, errs)
		})
	}
}

// ── pointer format for second embedded file ───────────────────────────────────

func TestValidate_EmbeddedFile_SecondFileHasCorrectPointerIndex(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	tmpl.Steps[0].Script = &openjd.StepScript{
		EmbeddedFiles: []openjd.EmbeddedFile{
			{Name: "f1", Type: openjd.EmbeddedFileTypeText, Data: "ok"},
			{Name: "f2", Data: "missing type"}, // missing type → error at index 1
		},
		Actions: openjd.StepActions{OnRun: openjd.Action{Command: "echo"}},
	}
	errs := openjd.Validate(tmpl)

	wantPtr := "/steps/0/script/embeddedFiles/1/type"
	for _, e := range errs {
		if e.Pointer == wantPtr {
			return
		}
	}
	t.Errorf("expected error at %q; got: %v", wantPtr, errs)
}
