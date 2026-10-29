// SPDX-License-Identifier: AGPL-3.0-or-later

package presettest_test

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/presettest"
)

func TestLoadRegistry_CoversEveryPresetAndBuiltin(t *testing.T) {
	reg, err := presettest.LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if len(reg.Presets) != 45 {
		t.Fatalf("registry entries = %d, want 45 (38 presets + 7 built-ins)", len(reg.Presets))
	}
	e, ok := reg.Entry("maya-layer-render")
	if !ok {
		t.Fatal("registry has no entry for maya-layer-render")
	}
	if e.Source != presettest.SourcePresets {
		t.Errorf("source = %q, want %q", e.Source, presettest.SourcePresets)
	}
	if e.Tier1 == nil || len(e.Tier1.Cases) == 0 || e.Tier1.Caveat == "" {
		t.Errorf("maya-layer-render tier1 = %+v, want cases and a caveat", e.Tier1)
	}
	if _, ok := reg.Entry("script"); !ok {
		t.Error("registry has no entry for the script built-in")
	}
}

func TestLoadRegistry_RejectsTier1WithoutCaveat(t *testing.T) {
	_, err := presettest.ParseRegistry([]byte(`
presets:
  - name: demo
    source: presets/sqi
    tier: 1
    tier1:
      cases: [default]
      caveat: ""
`))
	if err == nil {
		t.Fatal("ParseRegistry: want error for a Tier-1 entry with no caveat, got nil")
	}
}

// An execution-tier block with no required platform can never fail the
// registry's skip check, so the loader rejects it. Omitting the block is how an
// unrunnable tier is recorded.
func TestLoadRegistry_RejectsExecTierWithNoRequiredPlatform(t *testing.T) {
	for _, field := range []string{"tier2", "tier3"} {
		t.Run(field, func(t *testing.T) {
			_, err := presettest.ParseRegistry([]byte(`
presets:
  - name: demo
    source: presets/sqi
    tier: 1
    tier1: {cases: [default], caveat: x}
    ` + field + `:
      case: TestPresetTier3/demo/default
      required_on: []
`))
			if err == nil {
				t.Fatalf("ParseRegistry: want error for a %s block with an empty required_on, got nil", field)
			}
		})
	}
	// The same block with a platform is accepted: the rule rejects only the
	// empty list.
	if _, err := presettest.ParseRegistry([]byte(`
presets:
  - name: demo
    source: presets/sqi
    tier: 1
    tier1: {cases: [default], caveat: x}
    tier3:
      case: TestPresetTier3/demo/default
      required_on: [linux]
`)); err != nil {
		t.Errorf("ParseRegistry: a tier3 block naming a platform must parse, got %v", err)
	}
	// Omitting the block stays legal: it records a tier nothing can run.
	if _, err := presettest.ParseRegistry([]byte(`
presets:
  - name: demo
    source: presets/sqi
    tier: 1
    tier1: {cases: [default], caveat: x}
`)); err != nil {
		t.Errorf("ParseRegistry: an entry with no tier3 block must parse, got %v", err)
	}
}

func TestLoadRegistry_RejectsUnknownSourceAndDuplicates(t *testing.T) {
	if _, err := presettest.ParseRegistry([]byte(`
presets:
  - name: demo
    source: somewhere/else
    tier: 1
    tier1: {cases: [default], caveat: x}
`)); err == nil {
		t.Error("ParseRegistry: want error for an unknown source, got nil")
	}
	if _, err := presettest.ParseRegistry([]byte(`
presets:
  - name: demo
    source: presets/sqi
    tier: 1
    tier1: {cases: [default], caveat: x}
  - name: demo
    source: presets/sqi
    tier: 1
    tier1: {cases: [default], caveat: x}
`)); err == nil {
		t.Error("ParseRegistry: want error for a duplicate entry, got nil")
	}
}

func TestOutcomeSink_RecordsAndReports(t *testing.T) {
	presettest.RecordOutcome("TestX/case-a", false, "")
	presettest.RecordOutcome("TestX/case-b", true, "no ffmpeg")

	if got := presettest.LookupOutcome("TestX/case-a"); !got.Ran || got.Skipped {
		t.Errorf("case-a outcome = %+v, want ran and not skipped", got)
	}
	if got := presettest.LookupOutcome("TestX/case-b"); !got.Ran || !got.Skipped || got.Reason != "no ffmpeg" {
		t.Errorf("case-b outcome = %+v, want skipped with a reason", got)
	}
	if got := presettest.LookupOutcome("TestX/never"); got.Ran {
		t.Errorf("unrecorded case outcome = %+v, want zero value", got)
	}
}

// A case that needs a vendor application is required by application, through
// SQI_TEST_REQUIRE_APPS, not by platform: CI cannot install Nuke or Houdini, so
// no platform list could be right for it.
func TestParseRegistry_RequiresApp(t *testing.T) {
	entry := func(tier2 string) []byte {
		return []byte(`
presets:
  - name: demo
    source: presets/sqi
    tier: 2
    tier1: {cases: [default], caveat: x}
    tier2:
` + tier2)
	}
	tests := []struct {
		name    string
		tier2   string
		wantErr string // "" accepts the registry
	}{
		{"known app", "      case: TestPresetDCC_Demo\n      requires_app: blender\n", ""},
		{"unknown app", "      case: TestPresetDCC_Demo\n      requires_app: mayaa\n", "unknown application"},
		{
			"both forms",
			"      case: TestPresetDCC_Demo\n      requires_app: nuke\n      required_on: [linux]\n",
			"both required_on and requires_app",
		},
		{"neither form", "      case: TestPresetDCC_Demo\n", "requires no platform"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := presettest.ParseRegistry(entry(tt.tier2))
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("ParseRegistry: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("ParseRegistry error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseRegistry_Tier3CannotRequireAnApp(t *testing.T) {
	_, err := presettest.ParseRegistry([]byte(`
presets:
  - name: demo
    source: presets/sqi
    tier: 3
    tier1: {cases: [default], caveat: x}
    tier3:
      case: TestPresetTier3/demo/default
      requires_app: blender
`))
	if err == nil || !strings.Contains(err.Error(), "stub") {
		t.Fatalf("ParseRegistry error = %v, want a tier3 requires_app refusal", err)
	}
}

func TestRequiredApps(t *testing.T) {
	got, err := presettest.RequiredApps(" blender, nuke ,,houdini")
	if err != nil || strings.Join(got, ",") != "blender,nuke,houdini" {
		t.Errorf("RequiredApps = %v, %v, want blender,nuke,houdini", got, err)
	}
	if got, err := presettest.RequiredApps(""); err != nil || len(got) != 0 {
		t.Errorf("RequiredApps(\"\") = %v, %v, want none", got, err)
	}
	if _, err := presettest.RequiredApps("blender,nukee"); err == nil || !strings.Contains(err.Error(), "nukee") {
		t.Errorf("RequiredApps with a typo = %v, want an error naming it", err)
	}
}
