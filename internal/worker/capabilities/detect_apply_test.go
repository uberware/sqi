// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import (
	"testing"
)

func TestBuildWorkerCapabilities_MergeAndPrecedence(t *testing.T) {
	cfg := CapabilitiesConfig{
		Detect:  []Detector{{Tag: "inhouse", Checks: []Check{{Env: EnvCheck{Name: "INHOUSE"}}}}},
		Disable: []string{"blender"},
	}
	env := fakeEnv{goos: "linux", envs: map[string]string{"INHOUSE": "1"}}

	caps, err := BuildWorkerCapabilities(cfg, []string{"maya=override"}, env)
	if err != nil {
		t.Fatalf("BuildWorkerCapabilities: %v", err)
	}
	if caps.Tags["inhouse"] != "true" {
		t.Errorf("custom detector tag not applied with value %q: got %q", "true", caps.Tags["inhouse"])
	}
	if caps.Tags["maya"] != "override" {
		t.Errorf("manual tag should win: got %q", caps.Tags["maya"])
	}
	if _, ok := caps.Tags["os"]; !ok {
		t.Errorf("base os tag missing")
	}
}

func TestApplyDetectors_SetsValueTrueForFreshTag(t *testing.T) {
	caps := Capabilities{}
	det := Detector{Tag: "inhouse", Checks: []Check{{Env: EnvCheck{Name: "INHOUSE"}}}}
	env := fakeEnv{goos: "linux", envs: map[string]string{"INHOUSE": "1"}}

	caps.ApplyDetectors([]Detector{det}, env)

	if caps.Tags["inhouse"] != "true" {
		t.Errorf("ApplyDetectors should record value %q for a freshly detected tag: got %q", "true", caps.Tags["inhouse"])
	}
}

func TestApplyDetectors_DoesNotOverwriteExistingKey(t *testing.T) {
	caps := Capabilities{Tags: map[string]string{"inhouse": "keep"}}
	det := Detector{Tag: "inhouse", Checks: []Check{{Env: EnvCheck{Name: "INHOUSE"}}}}
	env := fakeEnv{goos: "linux", envs: map[string]string{"INHOUSE": "1"}}

	caps.ApplyDetectors([]Detector{det}, env)

	if caps.Tags["inhouse"] != "keep" {
		t.Errorf("ApplyDetectors overwrote existing tag: got %q, want %q", caps.Tags["inhouse"], "keep")
	}
}

func TestLoadDetectors_RejectsInvalidCustomDetector(t *testing.T) {
	_, err := LoadDetectors(CapabilitiesConfig{Detect: []Detector{{Tag: "x"}}})
	if err == nil {
		t.Fatal("expected error for custom detector with no checks, got nil")
	}
}

func TestLoadDetectors_DisableRemovesBuiltin(t *testing.T) {
	ds, err := LoadDetectors(CapabilitiesConfig{Disable: []string{"maya"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		if d.Tag == "maya" {
			t.Errorf("disabled built-in maya still present")
		}
	}
}
