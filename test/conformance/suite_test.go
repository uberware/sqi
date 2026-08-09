// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build conformance

package conformance_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// TestConformance_EXPRNotSupported is the drift guard for the
// expression-level scoring path.
//
// That path (conformance.RunExprCase) exists only because internal/openjd
// rejects EXPR templates outright, which makes the template path unable to
// score EXPR fixtures without reporting 180 false passes. EXPR is registered
// (status "in-progress") so the conformance harness can drive it through the
// real parse and validate path, but the moment production marks it
// StatusSupported, the workaround becomes not merely unnecessary but actively
// misleading — two paths scoring the same fixtures by different rules.
// Failing here forces its removal rather than letting it rot.
func TestConformance_EXPRNotSupported(t *testing.T) {
	ext, ok := openjd.LookupExtension("EXPR")
	if !ok {
		t.Fatal("EXPR is no longer in the extension registry.\n" +
			"Sub-project E2 registers it with status \"in-progress\" so this\n" +
			"suite can score EXPR fixtures through the real template path.\n" +
			"Removing the entry breaks that scoring path.")
	}
	if ext.Status == openjd.StatusSupported {
		t.Fatal("internal/openjd now SUPPORTS EXPR.\n" +
			"The expression-level conformance path is obsolete: delete\n" +
			"test/conformance/exprcase.go, exprcase_test.go, baseline-expr.txt,\n" +
			"TestConformance_Expressions, collectEXPRFixtures, this test, and the\n" +
			"exprSuiteDir, exprBaselinePath, minExpectedExprFixtures and\n" +
			"minExpectedPasses constants; also delete runEXPRCase,\n" +
			"discountEXPRGateErrors, isEXPRGateError and\n" +
			"TestEXPRGateFilter_MatchesRealValidateExtensionsOutput (sub-project E2\n" +
			"Task 2's status-gate discount — once EXPR is StatusSupported it always\n" +
			"discounts zero errors, so it becomes dead weight, not a correctness\n" +
			"requirement) and the \"EXPR: a temporary, second scoring path\" section\n" +
			"of docs/openjd-conformance.md; then let TestConformance_Templates score\n" +
			"EXPR/job_templates end to end via plain RunCase.")
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

// runEXPRCase scores one EXPR fixture through the real parse-and-validate
// path (like conformance.RunCase), but discounts the EXPR extension's own
// not-yet-supported rejection first, so a fixture is judged on whatever
// OTHER validation errors sqi finds.
//
// Why this exists: EXPR is registered but StatusInProgress (Task 1), so
// validateExtensions unconditionally rejects every EXPR-declaring template
// with an "/extensions/<i>: extension \"EXPR\" is registered but not yet
// supported" error, regardless of the template's own content. Scoring EXPR
// fixtures through conformance.RunCase unfiltered therefore makes REJECTION
// trivially easy: every one of the 180 fixtures marked ".invalid" would
// "pass" whether or not sqi actually detects the defect the fixture exists
// to test, because the gate error alone already satisfies "must be
// rejected." That is exactly the false-pass mode RunExprCase's own doc
// comment warns naive pass/fail classification into ("would report 180
// passes... for the wrong reason") — this wrapper exists to keep the real
// path from silently reintroducing that failure mode now that EXPR
// fixtures reach RunCase's machinery at all. (Caught in review of sub-project
// E2 Task 2, before the resulting 177/209 score was reported as final —
// see baseline-expr.txt's 2026-08-08 note.)
//
// Deliberately local to the EXPR scoring loop, not a change to RunCase
// itself: every other suite directory depends on RunCase's current,
// unfiltered semantics, and production must keep reporting the gate error
// verbatim (see TestConformance_EXPRNotSupported). Sub-project H's checklist
// (see TestConformance_EXPRNotSupported's failure message) must delete this
// function alongside RunExprCase, exprcase.go and baseline-expr.txt once EXPR
// becomes StatusSupported — at that point discountEXPRGateErrors always
// removes zero errors and TestConformance_Templates scores EXPR/job_templates
// through plain RunCase like every other directory.
func runEXPRCase(tc conformance.TestCase, data []byte) conformance.Result {
	res := conformance.Result{Case: tc, State: conformance.StateLive}

	tmpl, err := openjd.Parse(data, openjd.FormatYAML)
	switch {
	case err != nil:
		res.Reason = fmt.Sprintf("parse rejected: %v", err)
	default:
		// CheckEXPRExpressionsWhileUnsupported is the counterpart of the
		// discount below: production skips the expression walk entirely while
		// EXPR is StatusInProgress (the walk cannot change a verdict the status
		// gate has already decided, and it is expensive), so the suite has to
		// ask for it explicitly or it would score every EXPR fixture on the
		// status-gate error alone -- which this wrapper then discounts, leaving
		// nothing. Sub-project H deletes both when it flips the status.
		errs := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{
			EnforceLimits:                        true,
			CheckEXPRExpressionsWhileUnsupported: true,
		})
		errs = discountEXPRGateErrors(errs, tmpl.Extensions)
		if len(errs) > 0 {
			res.Reason = fmt.Sprintf("validation rejected: %v", errs)
		} else {
			res.Accepted = true
		}
	}

	res.Passed = res.Accepted != tc.Invalid
	if res.Passed {
		res.Reason = ""
	} else if res.Accepted {
		res.Reason = "accepted, but fixture is marked .invalid"
	}
	return res
}

// discountEXPRGateErrors removes every ValidationError produced by
// validateExtensions' registered-but-unsupported branch (Task 1's status
// gate) from errs, given the template's own declared extensions list in
// parse order. Every other error — including a genuinely unsupported or
// unregistered extension, and every non-extension validation failure — is
// left untouched; only the specific gate condition is discounted.
func discountEXPRGateErrors(errs openjd.ValidationErrors, extensions []string) openjd.ValidationErrors {
	out := make(openjd.ValidationErrors, 0, len(errs))
	for _, e := range errs {
		if isEXPRGateError(e, extensions) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// isEXPRGateError reports whether e is validateExtensions' registered-but-
// unsupported rejection for one of the template's own declared extensions.
//
// Matching is structural, keyed off the error's Pointer plus
// openjd.LookupExtension's Status field — NOT message text, so a wording
// change to the gate's message does not silently stop working here.
// TestEXPRGateFilter_MatchesRealValidateExtensionsOutput drives the real
// validateExtensions code path and pins the pointer format
// ("/extensions/<index>", no further nesting) this depends on; if that
// format ever changes, that test fails loudly instead of the EXPR score
// silently drifting because the filter stopped matching.
func isEXPRGateError(e openjd.ValidationError, extensions []string) bool {
	const prefix = "/extensions/"
	if !strings.HasPrefix(e.Pointer, prefix) {
		return false
	}
	idx, err := strconv.Atoi(e.Pointer[len(prefix):])
	if err != nil || idx < 0 || idx >= len(extensions) {
		return false
	}
	entry, known := openjd.LookupExtension(extensions[idx])
	return known && entry.Status != openjd.StatusSupported
}

// TestEXPRGateFilter_MatchesRealValidateExtensionsOutput pins the assumption
// isEXPRGateError depends on against the REAL validateExtensions output (not
// a synthetic ValidationError): that a registered-but-unsupported extension
// is reported at pointer "/extensions/<index>" with no further nesting, and
// that discountEXPRGateErrors removes exactly that error and nothing else
// for a minimal EXPR-only template. If validateExtensions' pointer format or
// gating condition ever changes, this test fails here rather than the EXPR
// score silently drifting.
func TestEXPRGateFilter_MatchesRealValidateExtensionsOutput(t *testing.T) {
	data := []byte(`specificationVersion: jobtemplate-2023-09
name: TestJob
extensions:
- EXPR
steps:
- name: Step1
  script:
    actions:
      onRun:
        command: echo
`)

	tmpl, err := openjd.Parse(data, openjd.FormatYAML)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	errs := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{EnforceLimits: true})
	if len(errs) != 1 {
		t.Fatalf("want exactly 1 validation error (the EXPR gate) for this minimal fixture, got %d: %v",
			len(errs), errs)
	}
	if !isEXPRGateError(errs[0], tmpl.Extensions) {
		t.Fatalf("isEXPRGateError did not recognize validateExtensions' real output: %+v", errs[0])
	}
	if filtered := discountEXPRGateErrors(errs, tmpl.Extensions); len(filtered) != 0 {
		t.Fatalf("discountEXPRGateErrors left %d errors, want 0: %v", len(filtered), filtered)
	}
}

// TestConformance_Expressions scores every EXPR job-template fixture.
//
// Sub-project E2 routed this to the real parse-and-validate path
// TestConformance_Templates uses (conformance.RunCase), instead of the old
// expression-level RunExprCase — see runEXPRCase's doc comment for why a
// thin wrapper around RunCase, not RunCase itself, is called below. It
// remains a separate test function (not folded into TestConformance_Templates)
// because it scores its own directory against its own baseline file,
// exprBaselinePath, rather than baseline.txt.
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
		results = append(results, runEXPRCase(tc, data))
	}

	t.Logf("template-level scoring (parse and validate)\n%s",
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

// TestConformance_B3ProtectedFixtures asserts by NAME that the fixtures
// sub-project B3 protects still pass.
//
// B3 adds comprehension and call syntax and clears no fixture, so its
// conformance contribution is defensive: ten .invalid fixtures pass today only
// because the constructs they use do not parse, and each must keep passing for
// a real reason once they do. The aggregate score cannot see a swap — ten
// regressing while ten others start passing leaves it unchanged — so these are
// checked individually.
func TestConformance_B3ProtectedFixtures(t *testing.T) {
	protected := map[string]string{
		// Rejected by the comprehension shadowing check (section 1.3.7).
		"EXPR/job_templates/3.6--let-comprehension-shadows.invalid.yaml":               "shadowing check",
		"EXPR/job_templates/3.6--let-comprehension-shadows-env.invalid.yaml":           "shadowing check",
		"EXPR/job_templates/3.6--let-comprehension-shadows-step-let.invalid.yaml":      "shadowing check",
		"EXPR/job_templates/3.6--let-comprehension-shadows-script-let.invalid.yaml":    "shadowing check",
		"EXPR/job_templates/3.6--let-comprehension-shadows-simple-action.invalid.yaml": "shadowing check",
		// Rejected by the parser or the lexer: unsupported comprehension forms.
		"EXPR/job_templates/expr1.1--reject-dict-comp.invalid.yaml":       "lexer, no brace token",
		"EXPR/job_templates/expr1.1--reject-set-comp.invalid.yaml":        "lexer, no brace token",
		"EXPR/job_templates/expr1.1--reject-generator-expr.invalid.yaml":  "parser, generator expression",
		"EXPR/job_templates/expr1.1--reject-multi-generator.invalid.yaml": "parser, one for clause",
		"EXPR/job_templates/expr1.1--reject-multi-if-comp.invalid.yaml":   "parser, one if filter",
	}

	results := make(map[string]conformance.Result)
	for _, tc := range collectEXPRFixtures(t) {
		if _, want := protected[tc.Path]; !want {
			continue
		}
		data, err := os.ReadFile(filepath.Join(SuiteRoot, tc.Path))
		if err != nil {
			t.Fatalf("read fixture %s: %v", tc.Path, err)
		}
		results[tc.Path] = conformance.RunExprCase(tc, data)
	}

	for path, why := range protected {
		t.Run(path, func(t *testing.T) {
			res, ok := results[path]
			if !ok {
				t.Fatalf("%s produced no result — has the fixture been renamed or removed? "+
					"It must be rejected by the %s.", path, why)
			}
			if !res.Passed {
				t.Fatalf("%s must pass (rejected by the %s): %s", path, why, res.Reason)
			}
		})
	}
}

// TestConformance_C1ProtectedFixtures asserts by NAME that the fixtures
// sub-project C1 puts at risk still pass.
//
// All sixteen are .invalid and all sixteen pass TODAY for the wrong reason:
// the function they call is not registered, so the expression fails with
// "unknown function" and the template is rejected. C1 registers bool, int,
// float, range_expr, min and max, which removes that accidental rejection —
// each fixture must then be rejected by REAL argument validation instead.
//
// The aggregate score cannot see the difference: sixteen regressing while
// sixteen others start passing leaves it unchanged. Same reason
// TestConformance_B3ProtectedFixtures exists, at a larger scale.
func TestConformance_C1ProtectedFixtures(t *testing.T) {
	protected := map[string]string{
		// bool(): rejects path and list outright, and rejects a string that is
		// not one of RFC 0006's accepted spellings.
		"EXPR/job_templates/expr2.2.1--bool-from-list.invalid.yaml":           "bool() rejects a list",
		"EXPR/job_templates/expr2.2.1--bool-from-path.invalid.yaml":           "bool() rejects a path",
		"EXPR/job_templates/expr2.2.1--bool-from-string-invalid.invalid.yaml": "bool() rejects an unrecognized string",
		// float(): no infinity, no NaN, and a string must parse.
		"EXPR/job_templates/expr2.2.1--float-from-complex.invalid.yaml":        "float() rejects a complex literal",
		"EXPR/job_templates/expr2.2.1--float-from-empty-string.invalid.yaml":   "float() rejects an empty string",
		"EXPR/job_templates/expr2.2.1--float-from-inf.invalid.yaml":            "float() rejects infinity",
		"EXPR/job_templates/expr2.2.1--float-from-nan.invalid.yaml":            "float() rejects NaN",
		"EXPR/job_templates/expr2.2.1--float-from-string-invalid.invalid.yaml": "float() rejects an unparseable string",
		// int(): the conversion must be non-destructive.
		"EXPR/job_templates/expr2.2.1--int-from-empty-string.invalid.yaml":   "int() rejects an empty string",
		"EXPR/job_templates/expr2.2.1--int-from-float-inexact.invalid.yaml":  "int() rejects a fractional float",
		"EXPR/job_templates/expr2.2.1--int-from-float-string.invalid.yaml":   "int() rejects a float-shaped string",
		"EXPR/job_templates/expr2.2.1--int-from-string-invalid.invalid.yaml": "int() rejects an unparseable string",
		// range_expr(): must contain at least one value.
		"EXPR/job_templates/expr2.2.1--range-expr-from-empty-list.invalid.yaml":   "range_expr() rejects an empty list",
		"EXPR/job_templates/expr2.2.1--range-expr-from-empty-string.invalid.yaml": "range_expr() rejects an empty string",
		// min()/max(): an empty list is an error, with RFC 0006's own wording.
		"EXPR/job_templates/expr2.2.2--max-empty-list.invalid.yaml": "max() rejects an empty list",
		"EXPR/job_templates/expr2.2.2--min-empty-list.invalid.yaml": "min() rejects an empty list",
	}

	results := make(map[string]conformance.Result)
	for _, tc := range collectEXPRFixtures(t) {
		if _, want := protected[tc.Path]; !want {
			continue
		}
		data, err := os.ReadFile(filepath.Join(SuiteRoot, tc.Path))
		if err != nil {
			t.Fatalf("read fixture %s: %v", tc.Path, err)
		}
		results[tc.Path] = conformance.RunExprCase(tc, data)
	}

	for path, why := range protected {
		t.Run(path, func(t *testing.T) {
			res, ok := results[path]
			if !ok {
				t.Fatalf("%s produced no result — has the fixture been renamed or removed? "+
					"It must be rejected because %s.", path, why)
			}
			if !res.Passed {
				t.Fatalf("%s must pass (%s): %s", path, why, res.Reason)
			}
		})
	}
}

// TestConformance_C2ProtectedFixtures asserts by NAME that the fixtures
// sub-project C2 puts at risk still pass.
//
// All seven are .invalid and all seven pass TODAY for the wrong reason: the
// string function they call is not registered, so the expression fails with
// "unknown function" and the template is rejected. C2 registers all 31 string
// functions, which removes that accidental rejection — each fixture must then
// be rejected by REAL argument validation instead.
//
// The aggregate score cannot see the difference: seven regressing while seven
// others start passing leaves it unchanged. Same reason
// TestConformance_C1ProtectedFixtures exists.
func TestConformance_C2ProtectedFixtures(t *testing.T) {
	protected := map[string]string{
		// The empty-substring rule: RFC 0006 states it for these five.
		"EXPR/job_templates/expr2.2.4--count-empty-substring.invalid.yaml":  "count() rejects an empty substring",
		"EXPR/job_templates/expr2.2.4--find-empty-substring.invalid.yaml":   "find() rejects an empty substring",
		"EXPR/job_templates/expr2.2.4--replace-empty-old.invalid.yaml":      "replace() rejects an empty old",
		"EXPR/job_templates/expr2.2.4--split-empty-separator.invalid.yaml":  "split() rejects an empty separator",
		"EXPR/job_templates/expr2.2.4--rsplit-empty-separator.invalid.yaml": "rsplit() rejects an empty separator",
		// index/rindex raise when the substring is absent; find/rfind do not.
		"EXPR/job_templates/expr2.2.4--index-not-found.invalid.yaml":  "index() rejects a missing substring",
		"EXPR/job_templates/expr2.2.4--rindex-not-found.invalid.yaml": "rindex() rejects a missing substring",
	}

	results := make(map[string]conformance.Result)
	for _, tc := range collectEXPRFixtures(t) {
		if _, want := protected[tc.Path]; !want {
			continue
		}
		data, err := os.ReadFile(filepath.Join(SuiteRoot, tc.Path))
		if err != nil {
			t.Fatalf("read fixture %s: %v", tc.Path, err)
		}
		results[tc.Path] = conformance.RunExprCase(tc, data)
	}

	for path, why := range protected {
		t.Run(path, func(t *testing.T) {
			res, ok := results[path]
			if !ok {
				t.Fatalf("%s produced no result — has the fixture been renamed or removed? "+
					"It must be rejected because %s.", path, why)
			}
			if !res.Passed {
				t.Fatalf("%s must pass (%s): %s", path, why, res.Reason)
			}
		})
	}
}

// TestConformance_C3ProtectedFixtures asserts by NAME that the fixtures
// sub-project C3 puts at risk still pass.
//
// All sixteen are .invalid and all sixteen pass TODAY for the wrong reason: the
// regex function they call is not registered, so the expression fails with
// "unknown function" and the template is rejected. C3 registers all six regex
// functions, which removes that accidental rejection — each fixture must then
// be rejected for real.
//
// WHAT THIS TEST ACTUALLY GUARANTEES, and what it does not: it asserts
// res.Passed, i.e. that the fixture is STILL rejected overall. It cannot
// assert WHICH mechanism rejected it, because conformance.Result blanks
// Reason on a pass (exprcase.go; pinned by exprcase_test.go's "Reason = ""
// want it empty on a pass" case) — there is no reason string available here
// to assert against for a passing .invalid fixture. So for any entry where a
// SECOND, independent mechanism also rejects the same input, deleting the
// purpose-built check that entry names would NOT turn this test red.
//
// Nine of the sixteen have no such gap: \z (Go's regexp.Compile accepts \z on
// its own — only our scanner's rejection catches it), all four empty-pattern
// fixtures (Go's regexp.Compile("") succeeds), and all four re_sub
// group-reference fixtures (Go's ReplaceAllLiteralString never expands "$1"
// or "\1" — that is the entire point of calling it "Literal" — so nothing but
// rejectGroupReferences catches these). Deleting the check named in any of
// those nine entries' "why" string DOES turn this test red, for the reason
// the string states.
//
// The other seven do not, for two different reasons:
//   - re-backreference, re-lookahead, re-lookbehind, re-named-backref,
//     re-split-invalid-pattern, re-split-maxsplit-invalid-pattern: Go's own
//     regexp.Compile independently rejects all six constructs (measured:
//     "invalid escape sequence", "invalid or unsupported Perl syntax",
//     "invalid named capture", "missing closing ]" respectively). Deleting
//     our purpose-built rejection for any of these six only degrades the
//     diagnostic to Go's own wording — this test would not notice, and does
//     not, by design: see the "lookahead" case in the vacuity-check section
//     of task-9-report.md.
//   - re-backslash-upper-Z is NOT a Go backstop and its "why" string says so:
//     the fixture is byte-for-byte identical to re-backslash-lower-z
//     (confirmed by diff and md5 — task-9-report.md, Fix Round 1) and its
//     content is r"llo\z", lowercase, so it is rejected by the SAME \z rule
//     that protects the lower-z entry, not by any \Z-specific logic. Nothing
//     in the vendored 2023-09 EXPR suite exercises \Z at all; our rejection
//     of \Z is pinned only by TestTranslatePattern_Rejects's "upper Z
//     anchor" case in repattern_internal_test.go.
//
// This is the scanner's real acceptance test for the nine fully-pinned
// entries, and only a rejection floor for the other seven: those seven
// confirm the template is still refused, without confirming refusal still
// comes from the mechanism each entry names.
func TestConformance_C3ProtectedFixtures(t *testing.T) {
	protected := map[string]string{
		// Go's own regexp.Compile independently rejects all four of these —
		// see the docstring above. The scanner refuses them FIRST only so the
		// diagnostic names the construct the spec names, not because nothing
		// else would catch them; deleting the scanner's own check here would
		// NOT turn this test red.
		"EXPR/job_templates/expr2.2.5--re-backreference.invalid.yaml": "backreferences are not supported",
		"EXPR/job_templates/expr2.2.5--re-lookahead.invalid.yaml":     "lookahead is not supported",
		"EXPR/job_templates/expr2.2.5--re-lookbehind.invalid.yaml":    "lookbehind is not supported",
		"EXPR/job_templates/expr2.2.5--re-named-backref.invalid.yaml": "named backreferences are not supported",
		// \z: Go accepts this anchor natively, so only OUR rejection catches
		// it — this entry DOES pin the scanner.
		"EXPR/job_templates/expr2.2.5--re-backslash-lower-z.invalid.yaml": `\z is not in the Python/Rust intersection`,
		// NAMED upper-Z, but byte-for-byte identical to the fixture above:
		// its content is r"llo\z", lowercase, so it tests \z, not \Z, and is
		// rejected by the same \z rule. It does NOT pin \Z-specific
		// rejection — see the docstring above.
		"EXPR/job_templates/expr2.2.5--re-backslash-upper-Z.invalid.yaml": `fixture is a byte-for-byte duplicate of re-backslash-lower-z.invalid.yaml and tests \z, not \Z; rejected by the same \z rule, not by any \Z-specific logic`,
		// The empty-pattern rule, stated once in RFC 0006 and tested four
		// ways. Go's regexp.Compile("") succeeds, so all four DO pin our own
		// check.
		"EXPR/job_templates/expr2.2.5--re-search-empty-pattern.invalid.yaml":         "re_search rejects an empty pattern",
		"EXPR/job_templates/expr2.2.5--re-split-empty-pattern.invalid.yaml":          "re_split rejects an empty pattern",
		"EXPR/job_templates/expr2.2.5--re-split-maxsplit-empty-pattern.invalid.yaml": "re_split with maxsplit rejects an empty pattern",
		"EXPR/job_templates/expr2.2.5--re-sub-empty-pattern.invalid.yaml":            "re_sub rejects an empty pattern",
		// Patterns malformed in any dialect: Go's regexp.Compile rejects "["
		// natively ("missing closing ]"), so these two do NOT pin our own
		// scanner either.
		"EXPR/job_templates/expr2.2.5--re-split-invalid-pattern.invalid.yaml":          "re_split rejects an unparseable pattern",
		"EXPR/job_templates/expr2.2.5--re-split-maxsplit-invalid-pattern.invalid.yaml": "re_split with maxsplit rejects an unparseable pattern",
		// re_sub's replacement is literal text; all four group-reference
		// spellings are errors. Go's ReplaceAllLiteralString never expands
		// "$1" or "\1", so nothing but our own rejectGroupReferences check
		// catches these; all four DO pin it.
		"EXPR/job_templates/expr2.2.5--re-sub-group-ref.invalid.yaml":              `re_sub rejects \1`,
		"EXPR/job_templates/expr2.2.5--re-sub-named-group-ref.invalid.yaml":        `re_sub rejects \g<1>`,
		"EXPR/job_templates/expr2.2.5--re-sub-dollar-group-ref.invalid.yaml":       "re_sub rejects $1",
		"EXPR/job_templates/expr2.2.5--re-sub-dollar-brace-group-ref.invalid.yaml": "re_sub rejects ${1}",
	}

	results := make(map[string]conformance.Result)
	for _, tc := range collectEXPRFixtures(t) {
		if _, want := protected[tc.Path]; !want {
			continue
		}
		data, err := os.ReadFile(filepath.Join(SuiteRoot, tc.Path))
		if err != nil {
			t.Fatalf("read fixture %s: %v", tc.Path, err)
		}
		results[tc.Path] = conformance.RunExprCase(tc, data)
	}

	for path, why := range protected {
		t.Run(path, func(t *testing.T) {
			res, ok := results[path]
			if !ok {
				t.Fatalf("%s produced no result — has the fixture been renamed or removed? "+
					"It must be rejected because %s.", path, why)
			}
			if !res.Passed {
				t.Fatalf("%s must pass (%s): %s", path, why, res.Reason)
			}
		})
	}
}

// TestConformance_C4ProtectedFixtures asserts by NAME that the fixtures
// sub-project C4 puts at risk still pass.
//
// All four are .invalid, and all four passed BEFORE C4 for the wrong reason:
// path() was not registered, so the expression failed with "unknown function"
// and the template was rejected. C4 registers the path constructor, the six
// path properties, the with-functions, relative_to and with_number, which
// removes that accidental rejection — each fixture must then be rejected by
// REAL argument validation instead.
//
// The aggregate score cannot see the difference: four regressing while four
// others start passing leaves it unchanged. Same reason
// TestConformance_C3ProtectedFixtures exists.
//
// WHAT THIS TEST ACTUALLY GUARANTEES, and what it does not — the correction
// C3's docstring had to be given after the fact, applied here up front. It
// asserts res.Passed, i.e. that the fixture is STILL rejected overall; it
// cannot assert WHICH mechanism rejected it, because conformance.Result blanks
// Reason on a pass (exprcase.go, pinned by exprcase_test.go). So an entry pins
// a specific check only where no second, independent mechanism rejects the
// same input. Unlike C3's list, ALL FOUR entries here were verified to have no
// such second mechanism: each named check was weakened on its own, this test
// was run, and it failed naming that one fixture and no other. What differs
// between them is WHOSE code each one pins:
//
//   - relative-to-not-relative and with-number-padding-too-wide pin C4's own
//     code. path("/a/b").relative_to(path("/a")) and
//     path("/out/file_%09d.exr").with_number(1) both evaluate cleanly, so the
//     only things rejecting these two fixtures' inputs are errNotRelative and
//     errPaddingTooWide. Verified by returning args[0] instead of
//     errNotRelative, and by clamping the width instead of returning
//     errPaddingTooWide.
//
//   - method-no-receiver-coercion does NOT pin C4. It pins section 1.2.4's
//     receiver restriction, which is sub-project B3's (matchShapesExactFirst
//     in shape.go, reached from callFunction's methodStyle flag). Measured:
//     the same call written as a plain function,
//     startswith(path("/foo/bar"), "/foo"), evaluates to true — path -> string
//     coercion exists, and only the exact-first rule refuses it on a receiver.
//     Verified by forcing methodStyle false at the call site. C4 is what makes
//     this entry LIVE, not what makes it pass.
//
//   - bool-from-path does NOT pin C4 either. It pins C1's bool() path row
//     ("Cannot convert path to bool"), and it is ALREADY asserted by
//     TestConformance_C1ProtectedFixtures with that row as its stated reason.
//     It is repeated here because C1 listed it while path() was still unknown,
//     which made C1's assertion vacuous until C4 shipped; this entry records
//     that the same assertion is now live. It is covered twice on purpose.
//     Verified by making that row return Bool(true).
//
// Note the floor all four share: deleting C4's path() registration outright
// would restore an "unknown function" rejection and leave every one of them
// passing. This is a rejection floor against C4's ARRIVAL, not a pin on
// path() continuing to exist.
func TestConformance_C4ProtectedFixtures(t *testing.T) {
	protected := map[string]string{
		// C4's own errNotRelative: the same call with a genuinely relative
		// other path evaluates fine, so nothing else rejects this input.
		"EXPR/job_templates/expr2.2.6--relative-to-not-relative.invalid.yaml": "relative_to rejects a path that is not relative to the other path",
		// C4's own errPaddingTooWide: the same call with %09d instead of
		// %099d evaluates fine, so nothing else rejects this one either.
		"EXPR/job_templates/expr2.3.2--with-number-padding-too-wide.invalid.yaml": "with_number rejects a padding width beyond the maximum",
		// Rejected by section 1.2.4's receiver restriction — B3's code, not
		// C4's. See the docstring above.
		"EXPR/job_templates/expr1.2.4--method-no-receiver-coercion.invalid.yaml": "section 1.2.4 forbids coercing a path receiver to string for a string method",
		// Rejected by C1's bool() path row, not by C4's code, and also listed
		// in TestConformance_C1ProtectedFixtures. See the docstring above.
		"EXPR/job_templates/expr2.2.1--bool-from-path.invalid.yaml": "bool() rejects a path",
	}

	results := make(map[string]conformance.Result)
	for _, tc := range collectEXPRFixtures(t) {
		if _, want := protected[tc.Path]; !want {
			continue
		}
		data, err := os.ReadFile(filepath.Join(SuiteRoot, tc.Path))
		if err != nil {
			t.Fatalf("read fixture %s: %v", tc.Path, err)
		}
		results[tc.Path] = conformance.RunExprCase(tc, data)
	}

	for path, why := range protected {
		t.Run(path, func(t *testing.T) {
			res, ok := results[path]
			if !ok {
				t.Fatalf("%s produced no result — has the fixture been renamed or removed? "+
					"It must be rejected because %s.", path, why)
			}
			if !res.Passed {
				t.Fatalf("%s must pass (%s): %s", path, why, res.Reason)
			}
		})
	}
}

// TestConformance_E2ProtectedFixtures asserts by NAME that every EXPR fixture
// sub-project E2 cleared still passes.
//
// E2 changed a path every EXPR template flows through: Task 2 moved
// TestConformance_Expressions off the old expression-only conformance.RunExprCase
// and onto runEXPRCase, a thin wrapper around the REAL parse-and-validate path
// (conformance.RunCase's machinery: openjd.Parse + openjd.ValidateWithOptions)
// that discounts only the EXPR extension's own registered-but-unsupported
// status-gate error (see runEXPRCase's doc comment above). Tasks 3-9 then
// built the scope model itself — a declared-scope symbol table, host-context
// gating, format-string routing through the real expression checker, and
// phase-2 re-checking with concrete parameters — reached for the first time
// once routing made EXPR fixtures hit real validation instead of a synthetic
// "unknown function" or "not a valid dotted identifier" rejection.
//
// Comparing baseline-expr.txt at 111b0b2 (E2's start) against HEAD gives the
// fixture list below: every EXPR/job_templates fixture that was baselined
// (failing) before E2 and is NOT baselined (i.e. genuinely passes) after —
// derived with `comm -23` between the two commits' baselined-fixture sets, 53
// entries. (The reverse direction — 23 fixtures that passed on the OLD
// expression-only path but are now correctly baselined as failing — is not
// this test's concern: those are corrected classifications, recorded in
// baseline-expr.txt's own Task 2 and Task 9 notes, not something E2 "cleared."
// 11 fixtures were baselined before and remain baselined after; those aren't
// here either.)
//
// The aggregate score cannot see one of these 53 regressing while another
// starts passing for an unrelated reason — same motivation as every other
// ProtectedFixtures test in this file, at E2's larger scale.
//
// WHAT THIS TEST ACTUALLY GUARANTEES, and what it does not: it asserts
// res.Passed, i.e. that the fixture is STILL rejected overall (all 53 are
// ".invalid"). It cannot assert WHICH mechanism rejects it, because
// conformance.Result blanks Reason on a pass — there is no reason string
// available here to assert against a passing .invalid fixture. This test does
// NOT attempt C4's per-entry exclusivity proof (weakening each named check in
// isolation and confirming only that one fixture fails) — at 53 entries that
// is not a "minor" per-entry verification the way it was for C4's four, and
// this docstring says so up front rather than letting a later review correct
// an overclaim, as happened to C3's first version. The "why" string on each
// entry below names the rule the fixture's own filename and content describe,
// not a proven-exclusive mechanism.
//
// Two categories, by what became reachable:
//
//   - 12 entries (the 7.3/7.3.1-numbered ones) are the scope model's own
//     rules: a host-context-only function (apply_path_mapping) used outside
//     @fmtstring[host], a submission-time reference (Job.Name, Step.Name,
//     Session.*, Task.Param.*) used in a scope that does not carry it, a
//     circular Job.Name self-reference in the name field, and a complex
//     expression or Job.Name/Step.Name reference that requires the EXPR
//     extension to be declared and appears without it. These are Tasks 6-9's
//     direct contribution.
//   - 40 entries (the 2.9-2.16-numbered ones) plus expr-extension-missing are
//     section 2's job-parameter shape validation (BOOL/RANGE_EXPR default
//     shape, LIST[*] length/item bounds/allowed-values/item-type) and the
//     base "no EXPR extension declared" case. internal/openjd's schema
//     validation already enforced these before E2 — what E2's Task 2 changed
//     is that EXPR fixtures now reach that validation at all, instead of
//     being scored by an expression-only reader that never ran a template's
//     parameter schema. They are listed here because Task 2's routing change
//     is exactly what put them at risk: reverting runEXPRCase to call
//     conformance.RunExprCase instead of the real validate path would regress
//     every one of them silently, the same swap this test exists to catch.
func TestConformance_E2ProtectedFixtures(t *testing.T) {
	protected := map[string]string{
		// --- Scope model (Tasks 6-9): host-context gating and submission-time
		// scope restrictions on Job.Name, Step.Name, Session.*, Task.Param.*,
		// and the "requires the EXPR extension" gate. ---
		"EXPR/job_templates/7.3--apply-path-mapping-in-job-name.invalid.yaml":              "apply_path_mapping is host-context-only; the job name field is not a host context",
		"EXPR/job_templates/7.3--apply-path-mapping-in-timeout.invalid.yaml":               "apply_path_mapping is host-context-only; a plain @fmtstring timeout is not a host context",
		"EXPR/job_templates/7.3--complex-expr-in-env-action-requires-expr.invalid.yaml":    "a complex expression in an environment script action requires the EXPR extension to be declared",
		"EXPR/job_templates/7.3--complex-expr-in-env-variables-requires-expr.invalid.yaml": "a complex expression in environment variables requires the EXPR extension to be declared",
		"EXPR/job_templates/7.3--session-in-host-requirements.invalid.yaml":                "Session.WorkingDirectory is not available in host requirements (submission-time scope)",
		"EXPR/job_templates/7.3--task-param-in-job-name.invalid.yaml":                      "Task.Param is not available in the job name field (submission-time scope)",
		"EXPR/job_templates/7.3.1--job-name-in-job-name-field.invalid.yaml":                "Job.Name cannot reference itself from within the job name field",
		"EXPR/job_templates/7.3.1--job-name-requires-expr.invalid.yaml":                    "Job.Name requires the EXPR extension to be declared",
		"EXPR/job_templates/7.3.1--step-name-in-job-environment-let.invalid.yaml":          "Step.Name is not available in a job environment's let binding — a jobEnvironment has no enclosing Step",
		"EXPR/job_templates/7.3.1--step-name-in-job-environment.invalid.yaml":              "Step.Name is not available in jobEnvironments (job-level, not step-level, scope)",
		"EXPR/job_templates/7.3.1--step-name-in-job-name-field.invalid.yaml":               "Step.Name is not available in the job name field",
		"EXPR/job_templates/7.3.1--step-name-requires-expr.invalid.yaml":                   "Step.Name requires the EXPR extension to be declared",
		"EXPR/job_templates/expr-extension-missing.invalid.yaml":                           "\"{{ }}\" expression syntax requires the EXPR extension to be declared",

		// --- Section 2 parameter-shape validation, reached for the first time
		// once Task 2 routed EXPR fixtures through the real template path. ---
		"EXPR/job_templates/2.9--bool-param-float-invalid.invalid.yaml":                 "BOOL parameter default is an out-of-range float",
		"EXPR/job_templates/2.9--bool-param-int-invalid.invalid.yaml":                   "BOOL parameter default is an out-of-range int",
		"EXPR/job_templates/2.9--bool-param-string-invalid.invalid.yaml":                "BOOL parameter default is an unrecognized string",
		"EXPR/job_templates/2.10--range-expr-param-invalid-default.invalid.yaml":        "RANGE_EXPR parameter default does not parse as a range expression",
		"EXPR/job_templates/2.11--list-string-item-not-in-allowed.invalid.yaml":         "LIST[STRING] item is not in item.allowedValues",
		"EXPR/job_templates/2.11--list-string-item-too-long.invalid.yaml":               "LIST[STRING] item exceeds item.maxLength",
		"EXPR/job_templates/2.11--list-string-item-too-short.invalid.yaml":              "LIST[STRING] item is below item.minLength",
		"EXPR/job_templates/2.11--list-string-scalar-not-list.invalid.yaml":             "LIST[STRING] default is a scalar, not a list",
		"EXPR/job_templates/2.11--list-string-too-long.invalid.yaml":                    "LIST[STRING] default exceeds the parameter's maxLength (list length)",
		"EXPR/job_templates/2.11--list-string-too-short.invalid.yaml":                   "LIST[STRING] default is below the parameter's minLength (list length)",
		"EXPR/job_templates/2.12--list-path-item-too-short.invalid.yaml":                "LIST[PATH] item is below item.minLength",
		"EXPR/job_templates/2.12--list-path-scalar-not-list.invalid.yaml":               "LIST[PATH] default is a scalar, not a list",
		"EXPR/job_templates/2.12--list-path-too-long.invalid.yaml":                      "LIST[PATH] default exceeds the parameter's maxLength (list length)",
		"EXPR/job_templates/2.12--list-path-too-short.invalid.yaml":                     "LIST[PATH] default is below the parameter's minLength (list length)",
		"EXPR/job_templates/2.12--list-path-wrong-item-type.invalid.yaml":               "LIST[PATH] item is not a path-shaped value",
		"EXPR/job_templates/2.13--list-int-item-below-min.invalid.yaml":                 "LIST[INT] item is below item.minValue",
		"EXPR/job_templates/2.13--list-int-item-not-in-allowed.invalid.yaml":            "LIST[INT] item is not in item.allowedValues",
		"EXPR/job_templates/2.13--list-int-scalar-not-list.invalid.yaml":                "LIST[INT] default is a scalar, not a list",
		"EXPR/job_templates/2.13--list-int-too-long.invalid.yaml":                       "LIST[INT] default exceeds the parameter's maxLength (list length)",
		"EXPR/job_templates/2.13--list-int-too-short.invalid.yaml":                      "LIST[INT] default is below the parameter's minLength (list length)",
		"EXPR/job_templates/2.13--list-int-wrong-item-type.invalid.yaml":                "LIST[INT] item is not an integer",
		"EXPR/job_templates/2.14--list-float-item-below-min.invalid.yaml":               "LIST[FLOAT] item is below item.minValue",
		"EXPR/job_templates/2.14--list-float-scalar-not-list.invalid.yaml":              "LIST[FLOAT] default is a scalar, not a list",
		"EXPR/job_templates/2.14--list-float-too-long.invalid.yaml":                     "LIST[FLOAT] default exceeds the parameter's maxLength (list length)",
		"EXPR/job_templates/2.14--list-float-too-short.invalid.yaml":                    "LIST[FLOAT] default is below the parameter's minLength (list length)",
		"EXPR/job_templates/2.14--list-float-wrong-item-type.invalid.yaml":              "LIST[FLOAT] item is not a number",
		"EXPR/job_templates/2.15--list-bool-scalar-not-list.invalid.yaml":               "LIST[BOOL] default is a scalar, not a list",
		"EXPR/job_templates/2.15--list-bool-too-long.invalid.yaml":                      "LIST[BOOL] default exceeds the parameter's maxLength (list length)",
		"EXPR/job_templates/2.15--list-bool-too-short.invalid.yaml":                     "LIST[BOOL] default is below the parameter's minLength (list length)",
		"EXPR/job_templates/2.15--list-bool-wrong-item-type.invalid.yaml":               "LIST[BOOL] item is not a recognized boolean",
		"EXPR/job_templates/2.16--list-list-int-inner-item-above-max.invalid.yaml":      "LIST[LIST[INT]] inner item is above item.maxValue",
		"EXPR/job_templates/2.16--list-list-int-inner-item-below-min.invalid.yaml":      "LIST[LIST[INT]] inner item is below item.minValue",
		"EXPR/job_templates/2.16--list-list-int-inner-item-not-in-allowed.invalid.yaml": "LIST[LIST[INT]] inner item is not in item.allowedValues",
		"EXPR/job_templates/2.16--list-list-int-inner-too-long.invalid.yaml":            "LIST[LIST[INT]] inner list exceeds its length bound",
		"EXPR/job_templates/2.16--list-list-int-inner-too-short.invalid.yaml":           "LIST[LIST[INT]] inner list is below its length bound",
		"EXPR/job_templates/2.16--list-list-int-ragged-scalar-in-outer.invalid.yaml":    "LIST[LIST[INT]] outer list mixes a scalar with nested lists",
		"EXPR/job_templates/2.16--list-list-int-scalar-not-list.invalid.yaml":           "LIST[LIST[INT]] default is a scalar, not a list",
		"EXPR/job_templates/2.16--list-list-int-string-in-inner.invalid.yaml":           "LIST[LIST[INT]] inner list contains a string, not an int",
		"EXPR/job_templates/2.16--list-list-int-too-long.invalid.yaml":                  "LIST[LIST[INT]] outer list exceeds the parameter's maxLength",
		"EXPR/job_templates/2.16--list-list-int-too-short.invalid.yaml":                 "LIST[LIST[INT]] outer list is below the parameter's minLength",
	}

	results := make(map[string]conformance.Result)
	for _, tc := range collectEXPRFixtures(t) {
		if _, want := protected[tc.Path]; !want {
			continue
		}
		data, err := os.ReadFile(filepath.Join(SuiteRoot, tc.Path))
		if err != nil {
			t.Fatalf("read fixture %s: %v", tc.Path, err)
		}
		results[tc.Path] = runEXPRCase(tc, data)
	}

	if len(protected) != 53 {
		t.Fatalf("protected has %d entries, want 53 — update this count alongside the map", len(protected))
	}

	for path, why := range protected {
		t.Run(path, func(t *testing.T) {
			res, ok := results[path]
			if !ok {
				t.Fatalf("%s produced no result — has the fixture been renamed or removed? "+
					"It must be rejected because %s.", path, why)
			}
			if !res.Passed {
				t.Fatalf("%s must pass (%s): %s", path, why, res.Reason)
			}
		})
	}
}
