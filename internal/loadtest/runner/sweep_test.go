// SPDX-License-Identifier: AGPL-3.0-or-later

package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/loadtest/report"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/target"
)

func TestSweepPlan(t *testing.T) {
	sc := func() *scenario.Scenario {
		s, err := scenario.Builtin("smoke")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	cases := []struct {
		name    string
		base    Options
		path    string
		values  []string
		wantErr string
	}{
		{name: "attach refused", base: Options{Scenario: sc(), Attach: &target.AttachConfig{}}, path: "workers[0].count", values: []string{"1"}, wantErr: "cannot attach"},
		{name: "no values", base: Options{Scenario: sc()}, path: "workers[0].count", wantErr: "at least one value"},
		{name: "bad value fails up front", base: Options{Scenario: sc()}, path: "workers[0].count", values: []string{"5", "many"}, wantErr: `"many"`},
		{name: "value that breaks validation", base: Options{Scenario: sc()}, path: "duration", values: []string{"1m", "1s"}, wantErr: "duration=1s"},
		{name: "bad run id", base: Options{Scenario: sc(), RunID: "a.b"}, path: "workers[0].count", values: []string{"1"}, wantErr: "run ID"},
		{name: "ok", base: Options{Scenario: sc()}, path: "workers[0].count", values: []string{"5", "10"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			managed := &target.ManagedConfig{ServerBin: "given"}
			if tc.base.Attach == nil {
				tc.base.Managed = managed
			}
			scs, err := sweepPlan(&tc.base, tc.path, tc.values)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want one mentioning %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(scs) != 2 || scs[0].Workers[0].Count != 5 || scs[1].Workers[0].Count != 10 {
				t.Errorf("points = %d, %d", scs[0].Workers[0].Count, scs[1].Workers[0].Count)
			}
			if tc.base.Scenario.Workers[0].Count != 20 {
				t.Error("the base scenario was changed")
			}
			if tc.base.RunID == "" || tc.base.Seed != SeedFor(tc.base.RunID) || tc.base.OutDir != DefaultOutDir {
				t.Errorf("defaults not filled: %+v", tc.base)
			}
			if tc.base.Managed == managed {
				t.Error("the caller's managed config is shared, so building binaries would change it")
			}
		})
	}
}

func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"500":                   "500",
		"1/s":                   "1_s",
		"{dist: uniform}":       "_dist__uniform_",
		`C:\x`:                  "C__x",
		"0.5":                   "0.5",
		"1.":                    "1_",
		strings.Repeat("a", 50): strings.Repeat("a", maxValueName),
	}
	for in, want := range cases {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSweep_TwoPoints runs a two-point sweep of a tiny scenario against real
// servers and checks the layout it writes.
func TestSweep_TwoPoints(t *testing.T) {
	if testing.Short() {
		t.Skip("runs real sqi binaries twice; skipped with -short")
	}
	sc := smokeScenario(t, "8s")
	sc.Warmup.Duration = 2_000_000_000
	sc.Jobs[0].Total = 4
	sc.Workers[0].Churn = scenario.Churn{}
	sc.Clients = scenario.ClientsSpec{}
	out := t.TempDir()
	pts, err := Sweep(t.Context(), Options{Scenario: sc, RunID: "sweep-test", OutDir: out, Managed: binaries(t), Logger: testLogger(t)},
		"workers[0].count", []string{"2", "4"})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(pts) != 2 {
		t.Fatalf("%d points, want 2", len(pts))
	}
	for i, p := range pts {
		if p.Rec == nil || !p.Rec.Complete || p.Rec.Mode != report.ModeSweepPoint {
			t.Fatalf("point %d: %+v", i, p.Rec)
		}
		if got := p.Rec.Scenario.Workers[0].Count; got != []int{2, 4}[i] {
			t.Errorf("point %d ran %d workers", i, got)
		}
		if p.Rec.RunID != "sweep-test-p"+string(rune('0'+i)) {
			t.Errorf("point %d run ID %q", i, p.Rec.RunID)
		}
	}
	for _, f := range []string{"sweep.csv", "sweep.md", filepath.Join("0-2", "run.json"), filepath.Join("1-4", "summary.md")} {
		if _, err := os.Stat(filepath.Join(out, "sweep-test", f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
