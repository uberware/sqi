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
		result("base/env_templates/d.yaml", conformance.StateLive, false),
		result("EXPR/job_templates/e.yaml", conformance.StateNotApplicable, false),
		result("EXPR/job_templates/f.yaml", conformance.StateNotApplicable, false),
	}

	groups := conformance.Rollup(results)

	want := []conformance.Group{
		{Name: "EXPR/job_templates", NotApplicable: 2},
		{Name: "base/env_templates", Failed: 1},
		{Name: "base/job_templates", Passed: 2, Failed: 1},
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

func TestFormatRollup_ShowsNotApplicableSeparately(t *testing.T) {
	out := conformance.FormatRollup([]conformance.Group{
		{Name: "base/job_templates", Passed: 438, Failed: 13},
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
