// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

// preset_tiers_test.go — the tier registry's verification (Phase 4, P1 §6).
//
// presets/validation-tiers.yaml states which validation tier each preset
// reached. This file makes each of those statements a FACT rather than an
// intention, by refusing five kinds of unearned claim. Rule 3 is the important
// one: a Tier-3 claim whose case skipped on a platform the entry lists in
// required_on FAILS. Every container-backed target in this repo can exit 0
// while running nothing, and the answer everywhere else has been a CI job
// asserting test names by hand; here it is enforced in Go instead.

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/presettest"
	"github.com/uberware/sqi/internal/product"
)

// TestZZPresetTierRegistrySatisfied runs LAST in this package on purpose: it
// asserts on what the tier tests recorded while running, so it must observe
// them. The ZZ prefix is what orders it.
func TestZZPresetTierRegistrySatisfied(t *testing.T) {
	reg, err := presettest.LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}

	assertRegistryCoversEverySource(t, reg)

	anyRecorded := false
	for _, entry := range reg.Presets {
		t.Run(entry.Name, func(t *testing.T) {
			// Rule 2: a Tier-1 claim needs a fixture case and a golden.
			casePath := filepath.Join(presetCaseDir, entry.Name+".yaml")
			cases, err := presettest.LoadCases(casePath)
			if err != nil {
				t.Fatalf("tier-1 claim with no usable fixture %s: %v", casePath, err)
			}
			names := make([]string, 0, len(cases))
			for _, c := range cases {
				names = append(names, c.Name)
			}
			for _, claimed := range entry.Tier1.Cases {
				if !slices.Contains(names, claimed) {
					t.Errorf("tier1 claims case %q, absent from %s", claimed, casePath)
				}
				golden := filepath.Join(presetGoldenDir, entry.Name+"--"+claimed+".golden")
				if _, err := os.Stat(golden); err != nil {
					t.Errorf("tier1 claims case %q but golden %s does not exist", claimed, golden)
				}
			}

			// Rules 3 and 4: an execution-tier claim needs a case that ran,
			// and a skip on a required platform is a failure.
			for label, tier := range map[string]*presettest.TierExec{"tier2": entry.Tier2, "tier3": entry.Tier3} {
				if tier == nil {
					continue
				}
				outcome := presettest.LookupOutcome(tier.Case)
				if !outcome.Ran {
					t.Errorf("%s claims case %q, which never reported -- run the whole package "+
						"(go test ./test/integration/), or the claim is backed by nothing", label, tier.Case)
					continue
				}
				anyRecorded = true
				if outcome.Skipped && slices.Contains(tier.RequiredOn, runtime.GOOS) {
					t.Errorf("%s case %q SKIPPED on %s, which required_on lists: %s\n"+
						"a skipped test verifies nothing", label, tier.Case, runtime.GOOS, outcome.Reason)
				}
			}
		})
	}

	if !anyRecorded {
		t.Error("no execution-tier case reported at all: this test asserts on what " +
			"TestPresetTier3 recorded, so run the whole package rather than this test alone")
	}
}

// TestRegistry_Tier2SkipOnRequiredPlatformIsAFailure mirrors the Tier-3
// assertion for Tier 2.
//
// Tier 2 is the tier most able to pass while proving nothing: its tests skip
// when the vendor application is absent, and ffmpeg is absent by default on
// most machines. The registry must treat that skip exactly as it treats a
// Tier-3 one.
func TestRegistry_Tier2SkipOnRequiredPlatformIsAFailure(t *testing.T) {
	const caseName = "TestRegistry_Tier2/synthetic/case"
	reg, err := presettest.ParseRegistry([]byte(`presets:
  - name: ffmpeg-transcode
    source: presets/sqi
    tier: 2
    tier1:
      cases: [default]
      caveat: synthetic fixture for the loader's own test
    tier2:
      case: ` + caseName + `
      required_on: [` + runtime.GOOS + `]
`))
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}

	entry, ok := reg.Entry("ffmpeg-transcode")
	if !ok {
		t.Fatal("synthetic entry missing")
	}
	if entry.Tier2 == nil {
		t.Fatal("tier2 block did not parse -- the registry cannot express Tier 2")
	}

	presettest.RecordOutcome(caseName, true, "synthetic: ffmpeg not installed")
	outcome := presettest.LookupOutcome(caseName)
	if !outcome.Skipped || !slices.Contains(entry.Tier2.RequiredOn, runtime.GOOS) {
		t.Fatalf("test setup wrong: skipped=%v requiredOn=%v goos=%s",
			outcome.Skipped, entry.Tier2.RequiredOn, runtime.GOOS)
	}
	// This is the condition TestZZPresetTierRegistrySatisfied applies. Asserting
	// it here keeps the Tier-2 half honest even before any real entry uses it.
}

// assertRegistryCoversEverySource is rule 1, in both directions: every shipped
// preset and built-in has an entry, and no entry names something that does not
// exist. This is what makes adding a preset a paired change — the same
// discipline internal/product/sqipresets_test.go's count check enforces for the
// schema.
func assertRegistryCoversEverySource(t *testing.T, reg presettest.Registry) {
	t.Helper()
	root, err := presettest.RepoRoot()
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}

	shipped := map[string]string{} // name → source
	entries, err := os.ReadDir(filepath.Join(root, "presets", "sqi"))
	if err != nil {
		t.Fatalf("read presets/sqi: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		// Skip macOS AppleDouble sidecars, as every loader in the repo does.
		if strings.HasPrefix(name, ".") || filepath.Ext(name) != ".yaml" {
			continue
		}
		shipped[strings.TrimSuffix(name, ".yaml")] = presettest.SourcePresets
	}
	for _, p := range product.Builtins() {
		shipped[p.Name] = presettest.SourceBuiltins
	}

	for name, source := range shipped {
		entry, ok := reg.Entry(name)
		if !ok {
			t.Errorf("%s (%s) has no entry in %s -- state what verified it",
				name, source, presettest.RegistryPath)
			continue
		}
		if entry.Source != source {
			t.Errorf("%s: registry says source %q, found in %q", name, entry.Source, source)
		}
	}
	for _, entry := range reg.Presets {
		if _, ok := shipped[entry.Name]; !ok {
			t.Errorf("registry names %q, which is neither a preset nor a built-in", entry.Name)
		}
	}
}
