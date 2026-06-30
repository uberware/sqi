// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"strings"
	"testing"
)

func TestParse_PathTranslation_AllDeliveries(t *testing.T) {
	src := `
specificationVersion: jobtemplate-2023-09
name: T
extensions: [ SQI_PATH_TRANSLATION ]
SQI_PATH_TRANSLATION:
  deliveries:
    - swap_in_place
    - translation_file
    - command_flags: { pattern: "--remap {src}={dest}" }
    - environment: { variable: "PROJECT_ROOT" }
    - stage_locally
steps:
  - name: S
    script:
      actions:
        onRun: { command: echo }
`
	tmpl, err := Parse([]byte(src), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if tmpl.PathTranslation == nil {
		t.Fatal("PathTranslation is nil")
	}
	got := tmpl.PathTranslation.Deliveries
	if len(got) != 5 {
		t.Fatalf("len(deliveries) = %d, want 5", len(got))
	}
	if got[2].Kind != DeliveryCommandFlags || got[2].Pattern != "--remap {src}={dest}" {
		t.Errorf("command_flags = %+v", got[2])
	}
	if got[3].Kind != DeliveryEnvironment || got[3].Variable != "PROJECT_ROOT" {
		t.Errorf("environment = %+v", got[3])
	}
}

func TestParse_PathTranslation_AbsentIsNil(t *testing.T) {
	src := `
specificationVersion: jobtemplate-2023-09
name: T
steps:
  - name: S
    script: { actions: { onRun: { command: echo } } }
`
	tmpl, err := Parse([]byte(src), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if tmpl.PathTranslation != nil {
		t.Errorf("PathTranslation = %+v, want nil", tmpl.PathTranslation)
	}
}

func TestDefaultPathDeliveries(t *testing.T) {
	got := DefaultPathDeliveries()
	if len(got) != 2 || got[0].Kind != DeliverySwapInPlace || got[1].Kind != DeliveryTranslationFile {
		t.Errorf("DefaultPathDeliveries() = %+v", got)
	}
}

func TestValidate_PathTranslation(t *testing.T) {
	base := func(ext, block string) string {
		return "specificationVersion: jobtemplate-2023-09\nname: T\n" + ext + block +
			"steps:\n  - name: S\n    script: { actions: { onRun: { command: echo } } }\n"
	}
	cases := []struct {
		name    string
		ext     string
		block   string
		wantErr string // substring; "" means valid
	}{
		{
			name:  "valid full set",
			ext:   "extensions: [ SQI_PATH_TRANSLATION ]\n",
			block: "SQI_PATH_TRANSLATION:\n  deliveries: [ swap_in_place, translation_file ]\n",
		},
		{
			name:    "block without extension",
			ext:     "",
			block:   "SQI_PATH_TRANSLATION:\n  deliveries: [ swap_in_place ]\n",
			wantErr: "requires declaring the SQI_PATH_TRANSLATION extension",
		},
		{
			name:    "extension without block",
			ext:     "extensions: [ SQI_PATH_TRANSLATION ]\n",
			block:   "",
			wantErr: "requires a SQI_PATH_TRANSLATION block",
		},
		{
			name:    "empty deliveries",
			ext:     "extensions: [ SQI_PATH_TRANSLATION ]\n",
			block:   "SQI_PATH_TRANSLATION:\n  deliveries: []\n",
			wantErr: "at least one delivery",
		},
		{
			name:    "unknown delivery",
			ext:     "extensions: [ SQI_PATH_TRANSLATION ]\n",
			block:   "SQI_PATH_TRANSLATION:\n  deliveries: [ teleport ]\n",
			wantErr: `unknown delivery "teleport"`,
		},
		{
			name:    "command_flags missing placeholders",
			ext:     "extensions: [ SQI_PATH_TRANSLATION ]\n",
			block:   "SQI_PATH_TRANSLATION:\n  deliveries:\n    - command_flags: { pattern: \"--remap\" }\n",
			wantErr: "must contain {src} and {dest}",
		},
		{
			name:    "environment missing variable",
			ext:     "extensions: [ SQI_PATH_TRANSLATION ]\n",
			block:   "SQI_PATH_TRANSLATION:\n  deliveries:\n    - environment: {}\n",
			wantErr: "requires a non-empty variable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpl, err := Parse([]byte(base(tc.ext, tc.block)), FormatYAML)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			errs := Validate(tmpl)
			if tc.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("want valid, got %v", errs)
				}
				return
			}
			if !containsErr(errs, tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, errs)
			}
		})
	}
}

func containsErr(errs ValidationErrors, sub string) bool {
	for _, e := range errs {
		if strings.Contains(e.Error(), sub) {
			return true
		}
	}
	return false
}
