// SPDX-License-Identifier: AGPL-3.0-or-later

package presettest_test

import (
	"testing"

	"github.com/uberware/sqi/internal/presettest"
)

func TestLoadRegistry_CoversEveryPresetAndBuiltin(t *testing.T) {
	reg, err := presettest.LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if len(reg.Presets) != 17 {
		t.Fatalf("registry entries = %d, want 17 (14 presets + 3 built-ins)", len(reg.Presets))
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
// registry's skip check -- slices.Contains(nil, anything) is false on every GOOS
// that exists or ever will -- so it would read as verified forever while proving
// nothing. Omitting the block is how an unrunnable tier is recorded.
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
	// The same block WITH a platform is accepted, so the rule rejects the empty
	// list rather than the block.
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
	// And omitting the block entirely stays legal -- that is the documented way
	// to record a tier nothing can run.
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
