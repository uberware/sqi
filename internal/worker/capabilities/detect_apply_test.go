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
	if _, ok := caps.Tags["inhouse"]; !ok {
		t.Errorf("custom detector tag not applied: %v", caps.Tags)
	}
	if caps.Tags["maya"] != "override" {
		t.Errorf("manual tag should win: got %q", caps.Tags["maya"])
	}
	if _, ok := caps.Tags["os"]; !ok {
		t.Errorf("base os tag missing")
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
