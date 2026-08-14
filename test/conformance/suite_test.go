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

// protectedFixtureResults scores every fixture named in protected through the
// same real parse-and-validate path TestConformance_Templates uses, and returns
// the results keyed by fixture path.
//
// It exists because every TestConformance_*ProtectedFixtures test below needs
// the same thing: a handful of named fixtures scored individually, so one
// regressing cannot be hidden by another starting to pass for an unrelated
// reason — the swap the aggregate score structurally cannot see.
//
// Before sub-project H2 flipped EXPR to StatusSupported these tests could not
// use the plain path. They ran through conformance.RunExprCase (an
// expression-only reader that never called openjd.Parse at all) or, for the E2
// and E3 sets, through a local runEXPRCase wrapper that first discounted the
// EXPR extension's own registered-but-unsupported status-gate error, since that
// error alone rejected every EXPR-declaring template and would have scored all
// 180 ".invalid" fixtures as passes whatever else was wrong with them. EXPR is
// supported now, so that gate never fires, both scoring paths are deleted, and
// these fixtures go through conformance.RunCase like every other directory's.
//
// The StateLive assertion is not decoration: conformance.Result.Passed is
// always false for a StateNotApplicable fixture, so a misclassified entry would
// fail every caller below with a confusing message instead of naming the cause.
func protectedFixtureResults(t *testing.T, protected map[string]string) map[string]conformance.Result {
	t.Helper()

	results := make(map[string]conformance.Result, len(protected))
	for _, tc := range collectTemplateFixtures(t) {
		if _, want := protected[tc.Path]; !want {
			continue
		}
		data, err := os.ReadFile(filepath.Join(SuiteRoot, tc.Path))
		if err != nil {
			t.Fatalf("read fixture %s: %v", tc.Path, err)
		}
		state := conformance.Classify(conformance.ExtensionFor(tc.Path), conformance.KindFor(tc.Path))
		if state != conformance.StateLive {
			t.Fatalf("%s is classified %v, not live — a fixture that is never scored "+
				"cannot protect anything", tc.Path, state)
		}
		results[tc.Path] = conformance.RunCase(tc, state, data)
	}
	return results
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
//
// MEASURED AGAIN when sub-project H2 moved this test onto the real template
// path (conformance.RunCase), because the mechanism changed under four of the
// ten. Until then these ran through the expression-only reader, which never
// called openjd.Parse, so the four `let-comprehension-shadows` fixtures below
// were rejected by comp.go's bare-expression shadow check while the real
// template path was rejecting them at PARSE time for their LIST[INT] job
// parameter — a wrong-reason pass recorded at the time in baseline-expr.txt.
// Sub-project F implemented LIST[INT], so on the real path all four are now
// rejected by section 1.3.7's rule for real, at the position that carries it
// (measured: `col 8: the loop variable "x" shadows an existing binding`).
// The fifth is not; its entry says so.
func TestConformance_B3ProtectedFixtures(t *testing.T) {
	protected := map[string]string{
		// Rejected by the comprehension shadowing check (section 1.3.7).
		"EXPR/job_templates/3.6--let-comprehension-shadows.invalid.yaml":            "shadowing check",
		"EXPR/job_templates/3.6--let-comprehension-shadows-env.invalid.yaml":        "shadowing check",
		"EXPR/job_templates/3.6--let-comprehension-shadows-step-let.invalid.yaml":   "shadowing check",
		"EXPR/job_templates/3.6--let-comprehension-shadows-script-let.invalid.yaml": "shadowing check",
		// NOT the shadowing check: this one puts its `let:` on a `bash:`
		// SimpleAction, an unmodeled FEATURE_BUNDLE_1 element, so it is rejected
		// on `/extensions/1: unsupported extension "FEATURE_BUNDLE_1"` and
		// `/steps/0/script: required` before any expression is checked. It stays
		// listed as a rejection floor, with its real reason stated so it is not
		// miscredited to B3.
		"EXPR/job_templates/3.6--let-comprehension-shadows-simple-action.invalid.yaml": "unregistered FEATURE_BUNDLE_1 extension, not the shadowing check",
		// Rejected by the parser or the lexer: unsupported comprehension forms.
		"EXPR/job_templates/expr1.1--reject-dict-comp.invalid.yaml":       "lexer, no brace token",
		"EXPR/job_templates/expr1.1--reject-set-comp.invalid.yaml":        "lexer, no brace token",
		"EXPR/job_templates/expr1.1--reject-generator-expr.invalid.yaml":  "parser, generator expression",
		"EXPR/job_templates/expr1.1--reject-multi-generator.invalid.yaml": "parser, one for clause",
		"EXPR/job_templates/expr1.1--reject-multi-if-comp.invalid.yaml":   "parser, one if filter",
	}

	results := protectedFixtureResults(t, protected)

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

	results := protectedFixtureResults(t, protected)

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

	results := protectedFixtureResults(t, protected)

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
// Reason on a pass (runcase.go; pinned by runcase_test.go) — there is no
// reason string available here
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

	results := protectedFixtureResults(t, protected)

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
// Reason on a pass (runcase.go, pinned by runcase_test.go). So an entry pins
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

	results := protectedFixtureResults(t, protected)

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
// E2 changed a path every EXPR template flows through: its Task 2 moved the
// EXPR score off an expression-only reader and onto the REAL parse-and-validate
// path (conformance.RunCase's machinery: openjd.Parse +
// openjd.ValidateWithOptions), discounting only the EXPR extension's own
// registered-but-unsupported status-gate error. Tasks 3-9 then built the scope
// model itself — a declared-scope symbol table, host-context gating,
// format-string routing through the real expression checker, and phase-2
// re-checking with concrete parameters — reached for the first time once
// routing made EXPR fixtures hit real validation instead of a synthetic
// "unknown function" or "not a valid dotted identifier" rejection. (Sub-project
// H2 removed the discount along with the status gate: EXPR is StatusSupported,
// so these fixtures now go through conformance.RunCase unmodified.)
//
// Comparing the then-separate baseline-expr.txt at 111b0b2 (E2's start) against
// E2's end gives the fixture list below: every EXPR/job_templates fixture that
// was baselined (failing) before E2 and is NOT baselined (i.e. genuinely
// passes) after — derived with `comm -23` between the two commits'
// baselined-fixture sets, 53 entries. (The reverse direction — 23 fixtures that
// passed on the OLD expression-only path but were then correctly baselined as
// failing — is not this test's concern: those are corrected classifications,
// recorded in that file's own Task 2 and Task 9 notes, not something E2
// "cleared." 11 fixtures were baselined before and remained baselined after;
// those aren't here either. baseline-expr.txt itself was deleted by H2 with the
// scoring path it belonged to; its history is in git.)
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
//   - 12 entries are the 7.3/7.3.1-numbered ones. ELEVEN of them are the
//     scope model's own rules: a host-context-only function
//     (apply_path_mapping) used outside @fmtstring[host] in the JOB NAME
//     field, a submission-time reference (Job.Name, Step.Name, Session.*,
//     Task.Param.*) used in a scope that does not carry it, a circular
//     Job.Name self-reference in the name field, and a complex expression or
//     Job.Name/Step.Name reference that requires the EXPR extension to be
//     declared and appears without it. Those eleven are Tasks 6-9's direct
//     contribution. The TWELFTH,
//     7.3--apply-path-mapping-in-timeout.invalid.yaml, is NOT: it is rejected
//     by decodeAction's strict integer parse of "timeout" at parse time, and
//     the scope model never sees it. Its entry below says so. Crediting it to
//     the host-context gate would be wrong twice over — the gate does not fire,
//     and the timeout position it would fire at is unreachable from any real
//     template (exprcheck.go, above checkActionExpressions).
//   - 40 entries (the 2.9-2.16-numbered ones) plus expr-extension-missing are
//     section 2's job-parameter shape validation (BOOL/RANGE_EXPR default
//     shape, LIST[*] length/item bounds/allowed-values/item-type) and the
//     base "no EXPR extension declared" case. internal/openjd's schema
//     validation already enforced these before E2 — what E2's Task 2 changed
//     is that EXPR fixtures now reach that validation at all, instead of
//     being scored by an expression-only reader that never ran a template's
//     parameter schema. They are listed here because Task 2's routing change
//     is exactly what put them at risk: routing EXPR fixtures back off the real
//     validate path would regress every one of them silently, the same swap
//     this test exists to catch.
func TestConformance_E2ProtectedFixtures(t *testing.T) {
	protected := map[string]string{
		// --- Scope model (Tasks 6-9): host-context gating and submission-time
		// scope restrictions on Job.Name, Step.Name, Session.*, Task.Param.*,
		// and the "requires the EXPR extension" gate. ---
		"EXPR/job_templates/7.3--apply-path-mapping-in-job-name.invalid.yaml": "apply_path_mapping is host-context-only; the job name field is not a host context",
		// NOT the scope model: this fixture is rejected by decodeAction
		// (parse.go), which decodes "timeout" with a strict integer parse, so
		// openjd.Parse fails with "timeout must be an integer" before the walk
		// runs at all. checkActionExpressions DOES carry a timeout position at
		// TargetInt, but it is wired-and-unreachable -- see the long PLAIN
		// STATEMENT comment above checkActionExpressions in exprcheck.go. It is
		// protected here because it is one of E2's thirteen target fixtures and
		// its passing state must not silently flip; the reason it passes is
		// incidental to the scope model and is stated so it is not miscredited.
		"EXPR/job_templates/7.3--apply-path-mapping-in-timeout.invalid.yaml":               "rejected at parse time: timeout must be an integer (decodeAction), NOT by the host-context gate",
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

	results := protectedFixtureResults(t, protected)

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

// e3ProtectedReason re-derives, directly and WITHOUT going through
// conformance.Result, the exact validation-error text conformance.RunCase
// would compute for path, given the fixture's own raw bytes.
//
// It exists because conformance.Result blanks Reason to "" the instant Passed
// is true (see RunCase's own doc comment). For an .invalid fixture that is
// correctly rejected, Passed IS true, so a result pulled from the results map
// below can never expose WHICH rule rejected it; that string is UNOBSERVABLE
// through conformance.Result alone, full stop. This function re-runs the
// identical Parse + ValidateWithOptions pipeline on the side, purely to read
// the error text back out before anything blanks it.
//
// Until sub-project H2 flipped EXPR to StatusSupported, this pipeline also had
// to ask for the expression walk explicitly and then discount the extension's
// own registered-but-unsupported status-gate error, which fired on every one of
// these fixtures. It now runs exactly what production runs.
//
// It requires the error set to contain EXACTLY one entry and returns that
// entry's Error() string; a fixture whose real defect now produces zero, or
// more than one, simultaneous errors fails loudly here instead of silently
// degrading to a substring match against an ambiguous message. All eleven
// TestConformance_E3ProtectedFixtures entries satisfy this today (confirmed
// individually by running each one), so every entry gets an exact-match
// reason, not merely a "some error fired" check — this docstring makes no
// claim beyond what was actually confirmed.
func e3ProtectedReason(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(SuiteRoot, path))
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	tmpl, perr := openjd.Parse(data, openjd.FormatYAML)
	if perr != nil {
		t.Fatalf("%s: expected a single validation error, got a parse failure instead: %v", path, perr)
	}
	errs := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{EnforceLimits: true})
	if len(errs) != 1 {
		t.Fatalf("%s: want exactly 1 validation error, got %d: %v", path, len(errs), errs)
	}
	return errs[0].Error()
}

// TestConformance_E3ProtectedFixtures asserts by NAME every EXPR fixture
// sub-project E3 cleared: comparing the then-separate baseline-expr.txt at
// E3's base commit (80d69b1) against E3's end with `comm -23` between the two
// commits' baselined-fixture sets — the same method E2's Task 12 used, at E3's
// smaller scale — gives exactly 11 entries. (The reverse direction, `comm -13`, is empty: E3
// only ever shrinks the EXPR baseline, it never adds an entry, so there is no
// "corrected classification" set to exclude here the way E2 had one.)
//
// E2's ProtectedFixtures test (above) asserts only res.Passed, and the
// whole-branch review of E2 named that as the exact gap that let a fixture
// be credited to the scope model when the YAML decoder was rejecting it for
// an unrelated reason: res.Passed alone cannot distinguish "rejected by the
// rule this fixture exists to test" from "rejected by something else that
// also happens to be true." This test goes further. For every entry — all
// eleven are .invalid — it also calls e3ProtectedReason, which independently
// re-runs the Parse + ValidateWithOptions pipeline conformance.RunCase uses
// and reads the raw error text back out, then asserts it against the map's
// value with EXACT equality, not a substring match. That second call is not
// optional decoration: it is the only way this test can name the mechanism at
// all, because conformance.Result blanks Reason to "" the moment Passed is
// true (see e3ProtectedReason's doc comment), which is always the case for a
// correctly-rejected .invalid fixture. res.Reason from the results map below
// is therefore never asserted against for content, only res.Passed is; this
// docstring says so explicitly rather than letting the two checks blur
// together and look like more coverage than they are.
//
// Both checks are pinned against the byte-for-byte, human-read error strings
// recorded for these eleven fixtures in the (now deleted) baseline-expr.txt's
// Task 2/4/8 notes (each confirmed there "by running each fixture and reading
// the resulting error, not inferred from the filename"): a wording change to
// the rule that rejects a fixture, or a change in WHICH rule rejects it, both
// fail this test, by design — the same design C4's per-entry exclusivity
// proof used, scoped down to an exact-string check because these eleven
// fixtures each produce exactly one validation error rather than needing one
// check per weakened mechanism.
func TestConformance_E3ProtectedFixtures(t *testing.T) {
	protected := map[string]string{
		// --- Tasks 1/2: `let` requires the EXPR extension to be declared. ---
		"EXPR/job_templates/3.6--let-requires-expr.invalid.yaml":                     `/steps/0/let: "let" requires the EXPR extension to be declared`,
		"EXPR/job_templates/3.6--let-in-job-environment-requires-expr.invalid.yaml":  `/jobEnvironments/0/script/let: "let" requires the EXPR extension to be declared`,
		"EXPR/job_templates/3.6--let-in-step-environment-requires-expr.invalid.yaml": `/steps/0/stepEnvironments/0/script/let: "let" requires the EXPR extension to be declared`,

		// --- Task 4: `let` block element-count bounds (section 3.6). ---
		"EXPR/job_templates/3.6--let-empty-list.invalid.yaml": `/steps/0/let: must contain at least one binding when provided`,
		"EXPR/job_templates/3.6--let-too-many.invalid.yaml":   `/steps/0/let: at most 50 let bindings are allowed (got 51)`,

		// --- Task 8: checkLetBindings actually wired into the three real
		// `let` positions, so its own rules (Tasks 1/6/7) run for real. ---
		"EXPR/job_templates/3.6--let-duplicate-name.invalid.yaml":             `/steps/0/let/1: let binding "x" shadows a name already in scope; section 3.6 forbids shadowing an earlier binding in the same block or in any enclosing scope`,
		"EXPR/job_templates/3.6--let-shadow-enclosing.invalid.yaml":           `/steps/0/script/let/0: let binding "x" shadows a name already in scope; section 3.6 forbids shadowing an earlier binding in the same block or in any enclosing scope`,
		"EXPR/job_templates/3.6.1--let-self-reference.invalid.yaml":           `/steps/0/let/0: col 1: unknown symbol "x"`,
		"EXPR/job_templates/3.6.1--let-missing-equals.invalid.yaml":           `/steps/0/let/0: let binding "x": must be of the form "name = expression"`,
		"EXPR/job_templates/3.6.1--let-uppercase-name.invalid.yaml":           `/steps/0/let/0: name "Foo" must start with a lowercase letter or underscore`,
		"EXPR/job_templates/expr1.3.7--loop-var-shadows-binding.invalid.yaml": `/steps/0/script/actions/onRun/args/1: col 8: the loop variable "x" shadows an existing binding`,
	}

	if len(protected) != 11 {
		t.Fatalf("protected has %d entries, want 11 — update this count alongside the map", len(protected))
	}

	results := protectedFixtureResults(t, protected)

	for path, wantReason := range protected {
		t.Run(path, func(t *testing.T) {
			res, ok := results[path]
			if !ok {
				t.Fatalf("%s produced no result — has the fixture been renamed or removed? "+
					"It must be rejected because: %s", path, wantReason)
			}
			if !res.Passed {
				t.Fatalf("%s must pass (rejected because: %s): %s", path, wantReason, res.Reason)
			}
			if got := e3ProtectedReason(t, path); got != wantReason {
				t.Fatalf("%s: rejection reason changed\n  got:  %s\n  want: %s", path, got, wantReason)
			}
		})
	}
}
