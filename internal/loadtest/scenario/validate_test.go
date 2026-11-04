// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import (
	"strings"
	"testing"
	"time"
)

func TestValidate_ExampleIsValid(t *testing.T) {
	if err := loadExample(t).Validate(TargetManaged); err != nil {
		t.Fatalf("example scenario should validate: %v", err)
	}
}

func TestValidate_RejectsUnsatisfiable(t *testing.T) {
	tests := []struct {
		name   string
		target TargetKind
		mutate func(*Scenario)
		path   string
		msg    string
	}{
		{
			"unknown queue", TargetManaged, func(s *Scenario) { s.Jobs[0].Queue = "comp2" },
			"jobs[0].queue", `"comp2" is not in farm.queues`,
		},
		{
			"undeclared usage pool", TargetManaged, func(s *Scenario) { s.Jobs[0].UsagePool = "nope" },
			"jobs[0].usage_pool", `"nope" is not in farm.usage_pools`,
		},
		{
			"real workers on attach", TargetAttach, func(*Scenario) {},
			"workers[1].real", "managed",
		},
		{
			"too many tasks", TargetManaged, func(s *Scenario) { s.Jobs[0].Steps = 11; s.Jobs[0].TasksPerStep = 100000 },
			"jobs[0].tasks_per_step", "1000000",
		},
		{
			"unsplittable task count", TargetManaged, func(s *Scenario) { s.Jobs[0].TasksPerStep = 1031 },
			"jobs[0].tasks_per_step", "1024",
		},
		{
			"both rate and total", TargetManaged, func(s *Scenario) { s.Jobs[0].Total = 5 },
			"jobs[0].rate", "exactly one",
		},
		{
			"neither rate nor total", TargetManaged, func(s *Scenario) { s.Jobs[1].Total = 0 },
			"jobs[1].rate", "exactly one",
		},
		{
			"negative total", TargetManaged, func(s *Scenario) { s.Jobs[1].Total = -1 },
			"jobs[1].total", "negative",
		},
		{
			"fail_rate above one", TargetManaged, func(s *Scenario) { s.Jobs[0].Task.FailRate = 1.5 },
			"jobs[0].task.fail_rate", "[0, 1]",
		},
		{
			"hang_rate negative", TargetManaged, func(s *Scenario) { s.Jobs[0].Task.HangRate = -0.1 },
			"jobs[0].task.hang_rate", "[0, 1]",
		},
		{
			"unknown store", TargetManaged, func(s *Scenario) { s.Server.Store = "mysql" },
			"server.store", `"mysql"`,
		},
		{
			"job_deps out of range", TargetManaged, func(s *Scenario) { s.JobDeps = 2 },
			"job_deps", "[0, 1]",
		},
		{
			"worker queue not declared", TargetManaged, func(s *Scenario) { s.Workers[0].Queues = []string{"render", "ghost"} },
			"workers[0].queues[1]", `"ghost" is not in farm.queues`,
		},
		{
			"empty worker group", TargetManaged, func(s *Scenario) { s.Workers[1].Real = 0 },
			"workers[1].count", "count + real",
		},
		{
			"duplicate queue", TargetManaged, func(s *Scenario) { s.Farm.Queues[1].Name = "render" },
			"farm.queues[1].name", "duplicate",
		},
		{
			"duplicate worker group", TargetManaged, func(s *Scenario) { s.Workers[1].Name = "linux-render" },
			"workers[1].name", "duplicate",
		},
		{
			"warmup beyond duration", TargetManaged, func(s *Scenario) { s.Warmup = Duration{time.Hour} },
			"warmup", "shorter than duration",
		},
		{
			"zero duration", TargetManaged, func(s *Scenario) { s.Duration = Duration{} },
			"duration", "positive",
		},
		{
			"missing name", TargetManaged, func(s *Scenario) { s.Name = "" },
			"name", "required",
		},
		{
			"zero steps", TargetManaged, func(s *Scenario) { s.Jobs[0].Steps = 0 },
			"jobs[0].steps", "at least 1",
		},
		{
			"negative cores", TargetManaged, func(s *Scenario) { s.Jobs[0].Cores = -1 },
			"jobs[0].cores", "negative",
		},
		{
			"polling without an interval", TargetManaged, func(s *Scenario) { s.Clients.RESTPollers.Interval = Duration{} },
			"clients.rest_pollers.interval", "positive",
		},
		{
			"action after the run ends", TargetManaged, func(s *Scenario) { s.Actions[1].At = Duration{time.Hour} },
			"actions[1].at", "after duration",
		},
		{
			"action does nothing", TargetManaged, func(s *Scenario) { s.Actions[1].RetryFailed = false },
			"actions[1]", "no action",
		},
		{
			"fraction above one", TargetManaged, func(s *Scenario) { s.Actions[0].CancelJobs.Fraction = 1.5 },
			"actions[0].cancel_jobs.fraction", "[0, 1]",
		},
		{
			"pause undeclared queue", TargetManaged, func(s *Scenario) { s.Actions[1].PauseQueue = "ghost" },
			"actions[1].pause_queue", `"ghost" is not in farm.queues`,
		},
		{
			"negative assert", TargetManaged, func(s *Scenario) { s.Assert["p95_submit_to_first_lease"] = Duration{-time.Second} },
			"assert.p95_submit_to_first_lease", "negative",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := loadExample(t)
			tc.mutate(s)
			err := s.Validate(tc.target)
			if err == nil {
				t.Fatal("expected a validation error")
			}
			found := false
			for line := range strings.SplitSeq(err.Error(), "\n") {
				if strings.HasPrefix(line, tc.path+": ") && strings.Contains(line, tc.msg) {
					found = true
				}
			}
			if !found {
				t.Errorf("no line starts with %q and mentions %q; got:\n%s", tc.path+": ", tc.msg, err)
			}
		})
	}
}

func TestValidate_ReportsEveryProblem(t *testing.T) {
	s := loadExample(t)
	s.Jobs[0].Queue = "comp2"
	s.Server.Store = "mysql"
	s.Jobs[0].Task.FailRate = 2
	err := s.Validate(TargetManaged)
	if err == nil {
		t.Fatal("expected errors")
	}
	if n := len(strings.Split(err.Error(), "\n")); n != 3 {
		t.Errorf("got %d errors, want 3:\n%s", n, err)
	}
}

func TestValidate_AttachAllowsSimulatedWorkers(t *testing.T) {
	s := loadExample(t)
	s.Workers = s.Workers[:1]
	if err := s.Validate(TargetAttach); err != nil {
		t.Fatalf("attach with simulated workers only should validate: %v", err)
	}
}
