// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

// preset_argv_test.go — Tier 1 of the preset validation harness (Phase 4, P1).
//
// For every preset in presets/validation-tiers.yaml, for every case in its
// fixture file, this expands and resolves the template through the real
// production pipeline (internal/presettest) and compares the result to a
// reviewed golden file.
//
// WHAT A GREEN RUN HERE PROVES: the preset expands to exactly the command line
// someone reviewed, and a change to expansion, resolution or the preset itself
// shows up as a golden diff.
//
// WHAT IT DOES NOT PROVE: that the command line is what the vendor's
// application wants. That is documentation-derived per preset, and the caveat
// field in the registry says so. Tier 3 (preset_exec_test.go) proves the path
// runs; only Tier 2 proves the application is happy.
//
// NOT tagged `integration`, deliberately: the CI `test` job runs
// `make test-cover`, which passes no -tags, so a tagged file would compile,
// lint, and never execute.

import (
	"flag"
	"path/filepath"
	"testing"

	"github.com/uberware/sqi/internal/presettest"
	"github.com/uberware/sqi/internal/store"
)

// updateGoldens regenerates every golden this test compares.
//
// Run: go test ./test/integration/ -run TestPresetTier1Argv -preset-update
// then READ THE DIFF. A regenerated golden is not a passing test; the review is
// the test.
var updateGoldens = flag.Bool("preset-update", false, "rewrite preset argv goldens")

const (
	presetCaseDir   = "testdata/preset-cases"
	presetGoldenDir = "testdata/preset-argv"
)

func TestPresetTier1Argv(t *testing.T) {
	reg, err := presettest.LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	for _, entry := range reg.Presets {
		t.Run(entry.Name, func(t *testing.T) {
			casePath := filepath.Join(presetCaseDir, entry.Name+".yaml")
			cases, err := presettest.LoadCases(casePath)
			if err != nil {
				t.Fatalf("LoadCases: %v", err)
			}
			assertRegistryCasesMatchFixture(t, entry, cases, casePath)

			for _, c := range cases {
				t.Run(c.Name, func(t *testing.T) {
					runTier1Case(t, entry, c)
				})
			}
		})
	}
}

// assertRegistryCasesMatchFixture keeps the registry honest in both directions:
// a case claimed but absent, and a case present but unclaimed.
func assertRegistryCasesMatchFixture(t *testing.T, entry presettest.Entry, cases []presettest.Case, casePath string) {
	t.Helper()
	have := make(map[string]bool, len(cases))
	for _, c := range cases {
		have[c.Name] = true
	}
	for _, claimed := range entry.Tier1.Cases {
		if !have[claimed] {
			t.Errorf("registry claims tier-1 case %q but %s does not define it", claimed, casePath)
		}
	}
	claimed := make(map[string]bool, len(entry.Tier1.Cases))
	for _, name := range entry.Tier1.Cases {
		claimed[name] = true
	}
	for _, c := range cases {
		if !claimed[c.Name] {
			t.Errorf("%s defines case %q but the registry does not claim it", casePath, c.Name)
		}
	}
}

func runTier1Case(t *testing.T, entry presettest.Entry, c presettest.Case) {
	t.Helper()

	tmpl, err := presettest.PresetTemplate(entry.Source, entry.Name)
	if err != nil {
		t.Fatalf("PresetTemplate: %v", err)
	}
	if c.Patch != nil {
		tmpl, err = presettest.ApplyChunkPatch(tmpl, *c.Patch)
		if err != nil {
			t.Fatalf("ApplyChunkPatch: %v", err)
		}
	}

	snap, err := presettest.Capture(t.Context(), tmpl, presettest.Options{
		Params: c.Params,
		Format: store.TemplateFormatYAML,
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	snap.Preset, snap.Case, snap.Patch = entry.Name, c.Name, c.Patch

	total := 0
	for _, s := range snap.Steps {
		total += len(s.Tasks)
	}
	if c.ExpectTasks > 0 && total != c.ExpectTasks {
		t.Errorf("expanded to %d tasks, fixture expects %d", total, c.ExpectTasks)
	}

	got := presettest.Render(snap)
	goldenPath := filepath.Join(presetGoldenDir, entry.Name+"--"+c.Name+".golden")
	if *updateGoldens {
		if err := presettest.WriteGolden(goldenPath, got); err != nil {
			t.Fatalf("WriteGolden: %v", err)
		}
		t.Logf("golden updated: %s -- READ THE DIFF before committing", goldenPath)
		return
	}
	if err := presettest.CompareGolden(goldenPath, got); err != nil {
		t.Error(err)
	}
}
