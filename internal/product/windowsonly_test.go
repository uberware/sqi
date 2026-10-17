// SPDX-License-Identifier: AGPL-3.0-or-later

// An external test package: it reads the tier registry through presettest, and
// presettest imports product.
package product_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/presettest"
)

// TestThreeDSMaxRender_IsWindowsOnly pins that 3dsmax-render can only be leased
// by a Windows worker, and that the tier registry says so. 3ds Max ships for
// Windows alone, so a step that omitted the os.family requirement would let a
// Linux worker carrying a hand-set 3dsmax tag lease work it cannot run, and a
// registry that claimed Tier 3 on Linux would claim a run that cannot happen.
func TestThreeDSMaxRender_IsWindowsOnly(t *testing.T) {
	root, err := presettest.RepoRoot()
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "presets", "sqi", "3dsmax-render.yaml"))
	if err != nil {
		t.Fatalf("read preset: %v", err)
	}
	var def struct {
		Template struct {
			Steps []struct {
				Name             string `yaml:"name"`
				HostRequirements struct {
					Attributes []struct {
						Name  string   `yaml:"name"`
						AnyOf []string `yaml:"anyOf"`
					} `yaml:"attributes"`
				} `yaml:"hostRequirements"`
			} `yaml:"steps"`
		} `yaml:"template"`
	}
	if err := yaml.Unmarshal(data, &def); err != nil {
		t.Fatalf("parse preset: %v", err)
	}
	if len(def.Template.Steps) == 0 {
		t.Fatal("preset declares no steps")
	}
	for _, step := range def.Template.Steps {
		windows := false
		for _, attr := range step.HostRequirements.Attributes {
			if attr.Name == "attr.worker.os.family" && slices.Equal(attr.AnyOf, []string{"windows"}) {
				windows = true
			}
		}
		if !windows {
			t.Errorf("step %q does not require attr.worker.os.family anyOf [windows]", step.Name)
		}
	}

	reg, err := presettest.LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	entry, ok := reg.Entry("3dsmax-render")
	if !ok {
		t.Fatal("3dsmax-render has no tier registry entry")
	}
	if entry.Tier3 == nil {
		t.Fatal("3dsmax-render registry entry has no tier3 block")
	}
	if !slices.Equal(entry.Tier3.RequiredOn, []string{"windows"}) {
		t.Errorf("tier3.required_on = %v, want [windows]", entry.Tier3.RequiredOn)
	}
}
