// SPDX-License-Identifier: AGPL-3.0-or-later

package conformance_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/uberware/sqi/test/conformance"
)

func TestLoadBaseline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.txt")
	content := `# Known-failing conformance tests.
# Comments and blank lines are ignored.

base/job_templates/1.1--broken.yaml
base/env_templates/7.1--env.yaml    # trailing comment
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write baseline: %v", err)
	}

	got, err := conformance.LoadBaseline(path)
	if err != nil {
		t.Fatalf("LoadBaseline: %v", err)
	}
	want := []string{
		"base/job_templates/1.1--broken.yaml",
		"base/env_templates/7.1--env.yaml",
	}
	if len(got) != len(want) {
		t.Fatalf("loaded %d entries, want %d: %v", len(got), len(want), got)
	}
	for _, id := range want {
		if _, ok := got[id]; !ok {
			t.Errorf("missing entry %q", id)
		}
	}
}

func TestLoadBaseline_MissingFileIsEmpty(t *testing.T) {
	got, err := conformance.LoadBaseline(filepath.Join(t.TempDir(), "absent.txt"))
	if err != nil {
		t.Fatalf("LoadBaseline on missing file: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d entries, want 0", len(got))
	}
}

func result(id string, state conformance.State, passed bool) conformance.Result {
	return conformance.Result{
		Case:   conformance.ParseTestCase(id),
		State:  state,
		Passed: passed,
	}
}

func TestDiffBaseline(t *testing.T) {
	tests := []struct {
		name            string
		results         []conformance.Result
		baseline        []string
		wantRegressions []string
		wantStale       []string
		wantOrphaned    []string
	}{
		{
			name:            "unlisted failure is a regression",
			results:         []conformance.Result{result("base/job_templates/a.yaml", conformance.StateLive, false)},
			baseline:        nil,
			wantRegressions: []string{"base/job_templates/a.yaml"},
		},
		{
			name:      "listed failure is expected",
			results:   []conformance.Result{result("base/job_templates/a.yaml", conformance.StateLive, false)},
			baseline:  []string{"base/job_templates/a.yaml"},
			wantStale: nil,
		},
		{
			name:      "listed test that passes is stale",
			results:   []conformance.Result{result("base/job_templates/a.yaml", conformance.StateLive, true)},
			baseline:  []string{"base/job_templates/a.yaml"},
			wantStale: []string{"base/job_templates/a.yaml"},
		},
		{
			name:     "unlisted pass is clean",
			results:  []conformance.Result{result("base/job_templates/a.yaml", conformance.StateLive, true)},
			baseline: nil,
		},
		{
			name:     "not-applicable results are ignored entirely",
			results:  []conformance.Result{result("EXPR/job_templates/a.yaml", conformance.StateNotApplicable, false)},
			baseline: nil,
		},
		{
			// A normal listed-failure entry (the common case, exercised above
			// too) must NOT be reported as orphaned just because it is
			// present in baseline.
			name:            "listed failure is not orphaned",
			results:         []conformance.Result{result("base/job_templates/a.yaml", conformance.StateLive, false)},
			baseline:        []string{"base/job_templates/a.yaml"},
			wantRegressions: nil,
			wantOrphaned:    nil,
		},
		{
			// The fixture was removed or renamed upstream: nothing in
			// results matches the baseline entry at all.
			name:         "baseline entry with no matching result is orphaned",
			results:      []conformance.Result{result("base/job_templates/a.yaml", conformance.StateLive, true)},
			baseline:     []string{"base/job_templates/deleted-upstream.yaml"},
			wantOrphaned: []string{"base/job_templates/deleted-upstream.yaml"},
		},
		{
			// The fixture still exists but was reclassified to
			// StateNotApplicable (e.g. Finding 1's env_templates fix) — its
			// baseline entry is now dead too, even though the ID still
			// matches a result.
			name:         "baseline entry for a now-not-applicable fixture is orphaned",
			results:      []conformance.Result{result("base/env_templates/x.yaml", conformance.StateNotApplicable, false)},
			baseline:     []string{"base/env_templates/x.yaml"},
			wantOrphaned: []string{"base/env_templates/x.yaml"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseline := make(map[string]struct{}, len(tt.baseline))
			for _, id := range tt.baseline {
				baseline[id] = struct{}{}
			}
			gotReg, gotStale, gotOrphaned := conformance.DiffBaseline(tt.results, baseline)
			assertIDs(t, "regressions", gotReg, tt.wantRegressions)
			assertIDs(t, "stale", gotStale, tt.wantStale)
			assertIDs(t, "orphaned", gotOrphaned, tt.wantOrphaned)
		})
	}
}

func assertIDs(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s[%d] = %q, want %q", label, i, got[i], want[i])
		}
	}
}
