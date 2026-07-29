// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build conformance

package conformance_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
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
		if d.IsDir() || !conformance.IsFixtureFile(d.Name()) {
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
	regressions, stale, orphaned := conformance.DiffBaseline(results, baseline)

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
	for _, id := range orphaned {
		t.Errorf("ORPHANED BASELINE %s matches no live result — remove it from %s", id, baselinePath)
	}
}

// exprSuiteDir is the fixture directory scored by the expression-level path.
const exprSuiteDir = "EXPR/job_templates"

// exprBaselinePath lists EXPR fixtures whose expressions do not yet parse. It
// is separate from baselinePath because the same fixture IDs appear in both
// scoring paths, and an EXPR entry in baseline.txt would be reported as
// orphaned forever — the template path never scores EXPR at all.
const exprBaselinePath = "baseline-expr.txt"

// minExpectedExprFixtures guards against a truncated submodule, exactly as
// minExpectedTests does for the template path.
const minExpectedExprFixtures = 200

// TestConformance_EXPRNotRegistered is the drift guard for the
// expression-level scoring path.
//
// That path (conformance.RunExprCase) exists only because internal/openjd
// rejects EXPR templates outright, which makes the template path unable to
// score EXPR fixtures without reporting 180 false passes. The moment
// production registers EXPR, the workaround becomes not merely unnecessary but
// actively misleading — two paths scoring the same fixtures by different
// rules. Failing here forces its removal rather than letting it rot.
func TestConformance_EXPRNotRegistered(t *testing.T) {
	if _, ok := openjd.LookupExtension("EXPR"); ok {
		t.Fatal("internal/openjd now registers EXPR.\n" +
			"The expression-level conformance path is obsolete: delete\n" +
			"test/conformance/exprcase.go, exprcase_test.go, baseline-expr.txt,\n" +
			"TestConformance_Expressions, collectEXPRFixtures, this test, and the\n" +
			"exprSuiteDir, exprBaselinePath, minExpectedExprFixtures and\n" +
			"minExpectedPasses constants; also delete the \"EXPR: a temporary,\n" +
			"second scoring path\" section of docs/openjd-conformance.md; then let\n" +
			"TestConformance_Templates score EXPR/job_templates end to end.")
	}
}

// collectEXPRFixtures returns every EXPR job-template fixture, relative to
// SuiteRoot. Job-execution tests (".test.yaml") are excluded, as in the
// template path.
func collectEXPRFixtures(t *testing.T) []conformance.TestCase {
	t.Helper()

	dir := filepath.Join(SuiteRoot, exprSuiteDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v\nRun: git submodule update --init --recursive", dir, err)
	}

	var cases []conformance.TestCase
	for _, entry := range entries {
		if entry.IsDir() || !conformance.IsFixtureFile(entry.Name()) {
			continue
		}
		tc := conformance.ParseTestCase(exprSuiteDir + "/" + entry.Name())
		if tc.IsJobTest {
			continue
		}
		cases = append(cases, tc)
	}
	return cases
}

// TestConformance_Expressions scores every EXPR job-template fixture by
// parsing the expressions it embeds. See RunExprCase for why this is a
// separate path from TestConformance_Templates.
func TestConformance_Expressions(t *testing.T) {
	cases := collectEXPRFixtures(t)
	if len(cases) < minExpectedExprFixtures {
		t.Fatalf("collected %d EXPR fixtures, want >= %d — suite looks truncated; "+
			"an empty run must never pass", len(cases), minExpectedExprFixtures)
	}

	results := make([]conformance.Result, 0, len(cases))
	for _, tc := range cases {
		data, err := os.ReadFile(filepath.Join(SuiteRoot, tc.Path))
		if err != nil {
			t.Fatalf("read fixture %s: %v", tc.Path, err)
		}
		results = append(results, conformance.RunExprCase(tc, data))
	}

	t.Logf("expression-level scoring (parse and evaluate)\n%s",
		conformance.FormatRollup(conformance.Rollup(results)))

	// A run in which almost nothing passes means the reader is broken, not
	// that the fixtures are hard: the suite's 29 expr1.1--reject-* fixtures
	// plus the unclosed, syntax-error and leading-zeros cases are rejected by
	// grammar alone, and the arithmetic, contextual-keyword and JSON-alias
	// fixtures parse. The comparison and conditional fixtures each also
	// contain a list literal, which now parses and evaluates too, so both
	// fixtures pass outright rather than being baselined.
	passed := 0
	for _, r := range results {
		if r.Passed {
			passed++
		}
	}
	const minExpectedPasses = 25
	if passed < minExpectedPasses {
		t.Errorf("only %d/%d EXPR fixtures pass, want >= %d — the expression "+
			"reader is likely broken rather than the fixtures being hard",
			passed, len(results), minExpectedPasses)
	}

	baseline, err := conformance.LoadBaseline(exprBaselinePath)
	if err != nil {
		t.Fatalf("load baseline: %v", err)
	}
	regressions, stale, orphaned := conformance.DiffBaseline(results, baseline)

	byID := map[string]conformance.Result{}
	for _, r := range results {
		byID[r.ID()] = r
	}
	for _, id := range regressions {
		t.Errorf("REGRESSION %s\n    %s", id, byID[id].Reason)
	}
	for _, id := range stale {
		t.Errorf("STALE BASELINE %s now passes — remove it from %s", id, exprBaselinePath)
	}
	for _, id := range orphaned {
		t.Errorf("ORPHANED BASELINE %s matches no live result — remove it from %s", id, exprBaselinePath)
	}
}
