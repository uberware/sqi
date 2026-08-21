// SPDX-License-Identifier: AGPL-3.0-or-later

package conformance_test

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/test/conformance"
)

func TestRollup(t *testing.T) {
	results := []conformance.Result{
		result("base/job_templates/a.yaml", conformance.StateLive, true),
		result("base/job_templates/b.yaml", conformance.StateLive, true),
		result("base/job_templates/c.yaml", conformance.StateLive, false),
		result("base/job_templates/d.yaml", conformance.StateLive, false),
		result("base/env_templates/e.yaml", conformance.StateLive, false),
		result("EXPR/job_templates/f.yaml", conformance.StateNotApplicable, false),
		result("EXPR/job_templates/g.yaml", conformance.StateNotApplicable, false),
	}
	baseline := map[string]struct{}{
		"base/job_templates/c.yaml": {},
	}

	groups := conformance.Rollup(results, baseline)

	// d.yaml fails and is NOT in the baseline, so it is a regression; c.yaml
	// fails and is, so it is adjudicated. A tally that cannot tell them apart
	// is the defect this split exists to fix.
	want := []conformance.Group{
		{Name: "EXPR/job_templates", NotApplicable: 2},
		{Name: "base/env_templates", Regressed: 1},
		{Name: "base/job_templates", Passed: 2, Baselined: 1, Regressed: 1},
	}
	if len(groups) != len(want) {
		t.Fatalf("got %d groups, want %d: %+v", len(groups), len(want), groups)
	}
	for i, w := range want {
		if groups[i] != w {
			t.Errorf("group[%d] = %+v, want %+v", i, groups[i], w)
		}
	}
}

// TestRollup_AgreesWithDiffBaseline is the invariant behind the split: the
// rollup's regression count and the REGRESSION lines the suite prints beneath it
// come from the same judgement, so they can never contradict each other.
//
// They did contradict each other before, which is why this test exists: the
// rollup labeled every failure "baselined" no matter what the diff said, so a
// genuine regression was summarized as an accepted divergence one line above the
// text calling it a regression.
func TestRollup_AgreesWithDiffBaseline(t *testing.T) {
	results := []conformance.Result{
		result("base/job_templates/a.yaml", conformance.StateLive, true),
		result("base/job_templates/b.yaml", conformance.StateLive, false),
		result("base/job_templates/c.yaml", conformance.StateLive, false),
		result("EXPR/job_templates/d.yaml", conformance.StateLive, false),
		result("EXPR/job_templates/e.yaml", conformance.StateNotApplicable, false),
	}
	baseline := map[string]struct{}{
		"base/job_templates/b.yaml": {},
		"EXPR/job_templates/d.yaml": {},
	}

	regressions, _, _ := conformance.DiffBaseline(results, baseline)

	total := 0
	for _, g := range conformance.Rollup(results, baseline) {
		total += g.Regressed
	}
	if total != len(regressions) {
		t.Errorf("rollup counts %d regressions, DiffBaseline reports %d (%v)",
			total, len(regressions), regressions)
	}
}

func TestFormatRollup_ShowsNotApplicableSeparately(t *testing.T) {
	out := conformance.FormatRollup([]conformance.Group{
		{Name: "base/job_templates", Passed: 438, Baselined: 13},
		{Name: "EXPR/job_templates", NotApplicable: 209},
	})

	if !strings.Contains(out, "438/451") {
		t.Errorf("rollup does not show the live pass ratio:\n%s", out)
	}
	if !strings.Contains(out, "209") || !strings.Contains(out, "n/a") {
		t.Errorf("rollup does not report not-applicable tests distinctly:\n%s", out)
	}
	if strings.Contains(out, "209/209") {
		t.Errorf("not-applicable tests are being shown as passes:\n%s", out)
	}
}

// TestFormatRollup_UnbaselinedFailureIsNotCalledBaselined pins the line that
// misled a reader on 2026-08-19: a new upstream fixture regressed and the
// summary read "449/450 pass  1 baselined" while the diff below it correctly
// said REGRESSION. The gate was sound; the sentence was false.
func TestFormatRollup_UnbaselinedFailureIsNotCalledBaselined(t *testing.T) {
	out := conformance.FormatRollup([]conformance.Group{
		{Name: "base/job_templates", Passed: 449, Regressed: 1},
	})

	if strings.Contains(out, "baselined") {
		t.Errorf("an unbaselined failure is being reported as adjudicated:\n%s", out)
	}
	if !strings.Contains(out, "REGRESSION") {
		t.Errorf("rollup does not call out the regression:\n%s", out)
	}
}

func TestFormatRollup_ReportsBothKindsOfFailure(t *testing.T) {
	out := conformance.FormatRollup([]conformance.Group{
		{Name: "EXPR/job_templates", Passed: 206, Baselined: 3, Regressed: 2},
	})

	if !strings.Contains(out, "206/211") {
		t.Errorf("the live ratio must count both kinds of failure:\n%s", out)
	}
	if !strings.Contains(out, "2 REGRESSIONS") {
		t.Errorf("rollup does not report the regressions:\n%s", out)
	}
	if !strings.Contains(out, "3 baselined") {
		t.Errorf("rollup does not report the adjudicated failures:\n%s", out)
	}
}
