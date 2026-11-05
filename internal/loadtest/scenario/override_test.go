// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import (
	"strings"
	"testing"
	"time"
)

func TestSet(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		value   string
		check   func(*Scenario) bool
		wantErr string
	}{
		{"int", "workers[0].count", "500", func(s *Scenario) bool { return s.Workers[0].Count == 500 }, ""},
		{"string", "server.store", "postgres", func(s *Scenario) bool { return s.Server.Store == "postgres" }, ""},
		{"bool", "server.tls", "true", func(s *Scenario) bool { return s.Server.TLS }, ""},
		{"float in nested struct", "jobs[0].task.fail_rate", "0.05", func(s *Scenario) bool { return s.Jobs[0].Task.FailRate == 0.05 }, ""},
		{"second slice element", "jobs[1].total", "9", func(s *Scenario) bool { return s.Jobs[1].Total == 9 }, ""},
		{"rate", "jobs[0].rate", "120/m", func(s *Scenario) bool { return s.Jobs[0].Rate.PerSecond == 2 }, ""},
		{"worker ramp", "workers[0].ramp", "50/s", func(s *Scenario) bool { return s.Workers[0].Ramp.PerSecond == 50 }, ""},
		{"duration", "duration", "1h", func(s *Scenario) bool { return s.Duration.Duration == time.Hour }, ""},
		{"duration inside a struct", "workers[0].churn.restart_after", "5s", func(s *Scenario) bool { return s.Workers[0].Churn.RestartAfter.Duration == 5*time.Second }, ""},
		{"dist constant", "jobs[0].task.duration", "45s", func(s *Scenario) bool {
			return s.Jobs[0].Task.Duration == Dist{Kind: DistConstant, Value: 45, Unit: "s"}
		}, ""},
		{"dist map", "jobs[0].task.log_lines", "{dist: uniform, min: 1, max: 9}", func(s *Scenario) bool {
			return s.Jobs[0].Task.LogLines == Dist{Kind: DistUniform, Min: 1, Max: 9}
		}, ""},
		{"slice", "workers[0].queues", "[render, comp]", func(s *Scenario) bool { return len(s.Workers[0].Queues) == 2 }, ""},
		{"pointer struct field", "actions[0].cancel_jobs.fraction", "0.5", func(s *Scenario) bool { return s.Actions[0].CancelJobs.Fraction == 0.5 }, ""},
		{"nil pointer is allocated", "actions[1].disable_workers.fraction", "0.25", func(s *Scenario) bool { return s.Actions[1].DisableWorkers.Fraction == 0.25 }, ""},
		{"typed map key", "workers[0].tags.gpu", "a100", func(s *Scenario) bool { return s.Workers[0].Tags["gpu"] == "a100" }, ""},
		{"duration map key", "assert.p95_submit_to_first_lease", "3s", func(s *Scenario) bool { return s.Assert["p95_submit_to_first_lease"].Duration == 3*time.Second }, ""},
		{"new duration map key", "assert.p99_submit_to_first_lease", "9s", func(s *Scenario) bool { return s.Assert["p99_submit_to_first_lease"].Duration == 9*time.Second }, ""},
		{"nested any map", "server.config.http.rate_limit.enabled", "false", func(s *Scenario) bool {
			h, _ := s.Server.Config["http"].(map[string]any) //nolint:errcheck // a missing key fails the check
			r, _ := h["rate_limit"].(map[string]any)         //nolint:errcheck // a missing key fails the check
			return r["enabled"] == false
		}, ""},
		{"empty value zeroes a string", "jobs[0].usage_pool", "", func(s *Scenario) bool { return s.Jobs[0].UsagePool == "" }, ""},

		{"index out of range", "workers[9].count", "1", nil, "index out of range"},
		{"unknown top-level", "nope.x", "1", nil, "nope"},
		{"unknown nested", "workers[0].nope", "1", nil, `"nope"`},
		{"bad int", "workers[0].count", "many", nil, "workers[0].count"},
		{"bad rate", "jobs[0].rate", "fast", nil, "jobs[0].rate"},
		{"slice needs an index", "workers.count", "1", nil, "index"},
		{"index on a scalar", "server.store[0]", "x", nil, "index"},
		{"descend into a scalar", "workers[0].count.x", "1", nil, "count"},
		{"descend into a rate", "jobs[0].rate.PerSecond", "1", nil, "rate"},
		{"empty path", "", "1", nil, "empty"},
		{"malformed index", "workers[x].count", "1", nil, "workers[x]"},
		{"unknown dist key", "jobs[0].task.duration", "{dist: lognormal, median: 1s, p95: 2s, extra: 1}", nil, "extra"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := loadExample(t)
			err := s.Set(tc.path, tc.value)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Set(%q, %q) succeeded, want an error mentioning %q", tc.path, tc.value, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Set(%q, %q): %v", tc.path, tc.value, err)
			}
			if !tc.check(s) {
				t.Errorf("Set(%q, %q) did not take effect", tc.path, tc.value)
			}
		})
	}
}

func TestSet_FailedSetLeavesScenarioUnchanged(t *testing.T) {
	s := loadExample(t)
	if err := s.Set("workers[0].count", "many"); err == nil {
		t.Fatal("expected an error")
	}
	if s.Workers[0].Count != 1800 {
		t.Errorf("count = %d, want it untouched", s.Workers[0].Count)
	}

	// The pointer on the way to the leaf must not be left allocated.
	if err := s.Set("actions[1].disable_workers.fraction", "lots"); err == nil {
		t.Fatal("expected an error")
	}
	if s.Actions[1].DisableWorkers != nil {
		t.Error("a failed Set left disable_workers allocated")
	}
}

func TestSet_DoesNotAliasEarlierCopies(t *testing.T) {
	s := loadExample(t)
	before := *s
	if err := s.Set("workers[0].tags.gpu", "a100"); err != nil {
		t.Fatal(err)
	}
	if _, ok := before.Workers[0].Tags["gpu"]; ok {
		t.Error("Set mutated the map of a previously copied Scenario")
	}
}
