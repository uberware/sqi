// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import "testing"

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
