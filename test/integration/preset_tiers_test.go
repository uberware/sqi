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
	"fmt"
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
				anyRecorded = anyRecorded || outcome.Ran
				if msg := execTierViolation(label, tier, outcome, runtime.GOOS); msg != "" {
					t.Error(msg)
				}
			}
		})
	}

	if !anyRecorded {
		t.Error("no execution-tier case reported at all: this test asserts on what " +
			"TestPresetTier3 recorded, so run the whole package rather than this test alone")
	}
}

// execTierViolation is rules 3 and 4 for one execution-tier claim: the case
// must have reported, and must not have skipped on a platform required_on
// lists. It returns "" when the claim is earned.
func execTierViolation(label string, tier *presettest.TierExec, outcome presettest.Outcome, goos string) string {
	if !outcome.Ran {
		return fmt.Sprintf("%s claims case %q, which never reported -- run the whole package "+
			"(make test-preset-harness), or the claim is backed by nothing", label, tier.Case)
	}
	if outcome.Skipped && slices.Contains(tier.RequiredOn, goos) {
		return fmt.Sprintf("%s case %q SKIPPED on %s, which required_on lists: %s\n"+
			"a skipped test verifies nothing", label, tier.Case, goos, outcome.Reason)
	}
	return ""
}

// TestExecTierViolation pins rules 3 and 4 directly, for both tiers.
//
// Tier 2 is the tier most able to pass while proving nothing: its tests skip
// when the vendor application is absent, and ffmpeg is absent by default on
// most machines. The registry must treat that skip exactly as it treats a
// Tier-3 one.
func TestExecTierViolation(t *testing.T) {
	tier := &presettest.TierExec{Case: "synthetic", RequiredOn: []string{"linux"}}
	skipped := presettest.Outcome{Ran: true, Skipped: true, Reason: "ffmpeg not on PATH"}
	tests := []struct {
		name          string
		outcome       presettest.Outcome
		goos          string
		wantViolation bool
	}{
		{"never reported", presettest.Outcome{}, "linux", true},
		{"ran", presettest.Outcome{Ran: true}, "linux", false},
		{"skipped on a required platform", skipped, "linux", true},
		{"skipped on an optional platform", skipped, "windows", false},
	}
	for _, tt := range tests {
		for _, label := range []string{"tier2", "tier3"} {
			t.Run(label+"/"+tt.name, func(t *testing.T) {
				got := execTierViolation(label, tier, tt.outcome, tt.goos)
				if (got != "") != tt.wantViolation {
					t.Errorf("execTierViolation = %q, want violation=%v", got, tt.wantViolation)
				}
			})
		}
	}
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
