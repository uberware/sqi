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
