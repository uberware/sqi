// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build conformance

package conformance_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uberware/sqi/test/conformance"
)

// SuiteRoot is the 2023-09 conformance-test tree inside the pinned
// openjd-specifications submodule, relative to this package directory.
const SuiteRoot = "../../third_party/openjd-specifications/conformance-tests/2023-09"

// baselinePath lists tests known to fail today. See DiffBaseline.
const baselinePath = "baseline.txt"

// minExpectedTests guards against a truncated or partially-checked-out
// submodule. A run that quietly covers 3 files must not look like a pass.
const minExpectedTests = 700

// TestConformance_SuitePresent fails loudly when the submodule is missing or
// empty. It never skips: this repo has been bitten repeatedly by targets that
// exit 0 without their dependency (make test-ldap, test-oidc, test-isolation
// all do), so a skip here would make an unrun suite look like a passing one.
func TestConformance_SuitePresent(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(SuiteRoot, "base", "job_templates"))
	if err != nil {
		t.Fatalf("conformance suite not found at %s: %v\n"+
			"Run: git submodule update --init --recursive", SuiteRoot, err)
	}
	const wantAtLeast = 400
	if len(entries) < wantAtLeast {
		t.Fatalf("base/job_templates has %d files, want >= %d — submodule looks truncated",
			len(entries), wantAtLeast)
	}
}

// collectTemplateFixtures walks the suite and returns every template-validation
// fixture, relative to SuiteRoot. Job-execution tests (".test.yaml") are
// excluded: they need a live session runtime and land with sub-project E.
func collectTemplateFixtures(t *testing.T) []conformance.TestCase {
	t.Helper()

	var cases []conformance.TestCase
	err := filepath.WalkDir(SuiteRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".yaml") {
			return nil
		}
		rel, err := filepath.Rel(SuiteRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		kind := filepath.Base(filepath.Dir(rel))
		if kind != "job_templates" && kind != "env_templates" {
			return nil
		}
		tc := conformance.ParseTestCase(rel)
		if tc.IsJobTest {
			return nil
		}
		cases = append(cases, tc)
		return nil
	})
	if err != nil {
		t.Fatalf("walk suite: %v", err)
	}
	return cases
}

// TestConformance_Templates is the gate. It runs every template-validation
// fixture, reports a per-directory rollup, and fails on baseline drift in
// either direction.
func TestConformance_Templates(t *testing.T) {
	cases := collectTemplateFixtures(t)
	if len(cases) < minExpectedTests {
		t.Fatalf("collected %d fixtures, want >= %d — suite looks truncated; "+
			"an empty run must never pass", len(cases), minExpectedTests)
	}

	results := make([]conformance.Result, 0, len(cases))
	for _, tc := range cases {
		data, err := os.ReadFile(filepath.Join(SuiteRoot, tc.Path))
		if err != nil {
			t.Fatalf("read fixture %s: %v", tc.Path, err)
		}
		state := conformance.Classify(conformance.ExtensionFor(tc.Path), conformance.KindFor(tc.Path))
		results = append(results, conformance.RunCase(tc, state, data))
	}

	t.Logf("\n%s", conformance.FormatRollup(conformance.Rollup(results)))

	baseline, err := conformance.LoadBaseline(baselinePath)
	if err != nil {
		t.Fatalf("load baseline: %v", err)
	}
	regressions, stale := conformance.DiffBaseline(results, baseline)

	byID := map[string]conformance.Result{}
	for _, r := range results {
		byID[r.ID()] = r
	}
	for _, id := range regressions {
		t.Errorf("REGRESSION %s\n    %s", id, byID[id].Reason)
	}
	for _, id := range stale {
		t.Errorf("STALE BASELINE %s now passes — remove it from %s", id, baselinePath)
	}
}
