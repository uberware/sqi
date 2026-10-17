// SPDX-License-Identifier: AGPL-3.0-or-later

// An external test package: it reads the preset through presettest, and
// presettest imports product.
package product_test

import (
	"slices"
	"testing"
)

// TestThreeDSMaxRender_IsWindowsOnly pins that 3dsmax-render can only be leased
// by a Windows worker. 3ds Max ships for Windows alone, so a step that omitted
// the os.family requirement would let a Linux worker carrying a hand-set
// 3dsmax tag lease work it cannot run. That the tier registry claims Tier 3 on
// Windows alone follows from this gate, and is held by
// test/integration's TestPresetTier3RequiredOnMatchesOSGate.
func TestThreeDSMaxRender_IsWindowsOnly(t *testing.T) {
	tmpl := parsedPresetTemplate(t, "3dsmax-render")
	if len(tmpl.Steps) == 0 {
		t.Fatal("preset declares no steps")
	}
	for _, step := range tmpl.Steps {
		windows := false
		if step.HostRequirements != nil {
			for _, attr := range step.HostRequirements.Attributes {
				if attr.Name == "attr.worker.os.family" && slices.Equal(attr.AnyOf, []string{"windows"}) {
					windows = true
				}
			}
		}
		if !windows {
			t.Errorf("step %q does not require attr.worker.os.family anyOf [windows]", step.Name)
		}
	}
}
