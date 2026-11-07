// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

var builtinNames = []string{
	"ci-medium", "expr-mix", "jobs-flood", "logs-heavy", "mixed-medium", "progress-heavy", "queues-many",
	"smoke", "soak-steady", "steps-deep", "tasks-wide", "ui-heavy", "workers-churn", "workers-ramp",
}

func TestBuiltins_AllValidate(t *testing.T) {
	names := BuiltinNames()
	if !slices.Equal(names, builtinNames) {
		t.Fatalf("BuiltinNames() = %v, want %v", names, builtinNames)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			s, err := Builtin(name)
			if err != nil {
				t.Fatalf("Builtin(%q): %v", name, err)
			}
			if s.Name != name {
				t.Errorf("name = %q, want it to match the file", s.Name)
			}
			if err := s.Validate(TargetManaged); err != nil {
				t.Errorf("does not validate:\n%v", err)
			}
			if s.Description == "" || strings.Contains(s.Description, "\n") {
				t.Errorf("description %q must be one non-empty line", s.Description)
			}
		})
	}
}

func TestBuiltin_Unknown(t *testing.T) {
	_, err := Builtin("nope")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "nope") || !strings.Contains(err.Error(), "smoke") {
		t.Errorf("error %q should name the request and list the built-ins", err)
	}
}

func TestBuiltin_ReturnsIndependentCopies(t *testing.T) {
	a, err := Builtin("smoke")
	if err != nil {
		t.Fatal(err)
	}
	a.Workers[0].Count = 1
	b, err := Builtin("smoke")
	if err != nil {
		t.Fatal(err)
	}
	if b.Workers[0].Count == 1 {
		t.Error("mutating one Builtin result changed the next")
	}
}

func TestBuiltin_Smoke(t *testing.T) {
	s, err := Builtin("smoke")
	if err != nil {
		t.Fatal(err)
	}
	if s.Duration.Duration != time.Minute {
		t.Errorf("duration = %s, want 1m", s.Duration)
	}
	if len(s.Workers) != 1 || s.Workers[0].Count != 20 {
		t.Fatalf("workers = %+v, want one group of 20", s.Workers)
	}
	c := s.Workers[0].Churn
	if c.CrashPerHour != 3 || c.RestartAfter.Duration != 10*time.Second {
		t.Errorf("churn = %+v, want crash_per_hour 3, restart_after 10s", c)
	}
	if len(s.Jobs) != 1 {
		t.Fatalf("jobs = %+v, want one class", s.Jobs)
	}
	j := s.Jobs[0]
	if j.Total != 50 || j.Steps != 2 || j.TasksPerStep != 10 {
		t.Errorf("job = total %d steps %d tasks_per_step %d, want 50 x 2 x 10", j.Total, j.Steps, j.TasksPerStep)
	}
	if l := j.Task.LogLines; l.Kind == "" || l.Value+l.Median+l.Max == 0 {
		t.Errorf("smoke must write logs, got log_lines %+v", l)
	}
	if s.Clients.WebSocket.Count != 5 {
		t.Errorf("websocket clients = %d, want 5", s.Clients.WebSocket.Count)
	}
}

// TestBuiltin_Sizes pins each scenario's headline number, so a stray edit to a
// library file cannot quietly turn one axis into another.
func TestBuiltin_Sizes(t *testing.T) {
	workers := func(s *Scenario) int {
		n := 0
		for _, w := range s.Workers {
			n += w.Count
		}
		return n
	}
	tests := []struct {
		name  string
		check func(t *testing.T, s *Scenario)
	}{
		{"workers-ramp", func(t *testing.T, s *Scenario) {
			if got := workers(s); got != 10000 {
				t.Errorf("workers = %d, want 10000", got)
			}
		}},
		{"workers-churn", func(t *testing.T, s *Scenario) {
			c := s.Workers[0].Churn
			if workers(s) != 2000 || c.CrashPerHour <= 0 || c.DisconnectPerHour <= 0 || c.RestartAfter.Duration <= 0 {
				t.Errorf("want 2000 workers with crash, disconnect and restart; got %d, %+v", workers(s), c)
			}
		}},
		{"jobs-flood", func(t *testing.T, s *Scenario) {
			j := s.Jobs[0]
			if j.Total != 100000 || j.Steps != 1 || j.TasksPerStep != 1 {
				t.Errorf("job = %+v, want 100000 one-task jobs", j)
			}
		}},
		{"tasks-wide", func(t *testing.T, s *Scenario) {
			j := s.Jobs[0]
			if len(s.Jobs) != 1 || j.Total != 1 || j.Steps != 1 || j.TasksPerStep != 1_000_000 {
				t.Errorf("jobs = %+v, want one job of 1,000,000 tasks", s.Jobs)
			}
		}},
		{"steps-deep", func(t *testing.T, s *Scenario) {
			j := s.Jobs[0]
			if j.Total != 1000 || j.Steps != 20 || s.JobDeps <= 0 {
				t.Errorf("want 1000 jobs x 20 steps with job_deps; got %d x %d, job_deps %v", j.Total, j.Steps, s.JobDeps)
			}
		}},
		{"queues-many", func(t *testing.T, s *Scenario) {
			if len(s.Farm.Queues) != 200 || len(s.Farm.UsagePools) != 50 {
				t.Errorf("queues = %d, pools = %d, want 200 and 50", len(s.Farm.Queues), len(s.Farm.UsagePools))
			}
			caps := map[string]int{}
			for _, q := range s.Farm.Queues {
				caps[q.Name] = q.MaxConcurrentTasks
			}
			limits := map[string]int{}
			for _, p := range s.Farm.UsagePools {
				limits[p.Name] = p.Limit
			}
			usedCaps, usedLimits := map[int]bool{}, map[int]bool{}
			for _, j := range s.Jobs {
				usedCaps[caps[j.Queue]] = true
				usedLimits[limits[j.UsagePool]] = true
			}
			if len(usedCaps) < 5 {
				t.Errorf("job classes use queues with caps %v, want every cap in the cycle (including non-zero ones)", usedCaps)
			}
			if usedCaps[0] && len(usedCaps) < 2 {
				t.Error("every used queue is unlimited, so no queue cap can bite")
			}
			if len(usedLimits) < 2 {
				t.Errorf("job classes use pools with limits %v, want at least two distinct limits", usedLimits)
			}
		}},
		{"logs-heavy", func(t *testing.T, s *Scenario) {
			if got := workers(s); got != 500 {
				t.Errorf("workers = %d, want 500", got)
			}
			j := s.Jobs[0]
			if j.Task.LogLines.Kind != DistConstant || j.Task.Duration.Kind != DistConstant || j.Task.Duration.Value <= 0 {
				t.Fatalf("logs-heavy needs a constant duration and constant log_lines, got %+v and %+v", j.Task.Duration, j.Task.LogLines)
			}
			perMin := j.Task.LogLines.Value * float64(j.Task.LogLineBytes) * 60 / j.Task.Duration.Value
			if perMin < 0.9e6 || perMin > 1.1e6 {
				t.Errorf("each worker streams %.0f B/min, want about 1 MB", perMin)
			}
		}},
		{"progress-heavy", func(t *testing.T, s *Scenario) {
			if got := workers(s); got != 200 {
				t.Errorf("workers = %d, want 200", got)
			}
			j := s.Jobs[0]
			if j.Task.ProgressEvery.Duration != time.Second || j.Task.Duration.Kind != DistConstant || j.Task.Duration.Value != 120 {
				t.Errorf("task = %+v, want 120 s tasks writing progress every 1 s", j.Task)
			}
			capacity := 200 / j.Task.Duration.Value
			if offered := j.Rate.PerSecond * float64(j.TasksPerStep*j.Steps); offered > capacity {
				t.Errorf("offers %.2f tasks/s, more than the %.2f tasks/s the workers run, so the queue never drains", offered, capacity)
			}
		}},
		{"ui-heavy", func(t *testing.T, s *Scenario) {
			if s.Clients.WebSocket.Count != 1000 || s.Clients.RESTPollers.Count != 100 {
				t.Errorf("clients = %+v, want 1000 WS and 100 pollers", s.Clients)
			}
		}},
		{"expr-mix", func(t *testing.T, s *Scenario) {
			if !slices.ContainsFunc(s.Jobs, func(j JobClass) bool { return j.EXPR }) {
				t.Error("no job class sets expr: true")
			}
		}},
		{"ci-medium", func(t *testing.T, s *Scenario) {
			if got := workers(s); got != 300 || s.Duration.Duration != 15*time.Minute {
				t.Errorf("workers = %d, duration = %s, want 300 for 15m", got, s.Duration)
			}
		}},
		{"mixed-medium", func(t *testing.T, s *Scenario) {
			if workers(s) != 1800 || s.Workers[1].Real != 2 || s.Duration.Duration != 30*time.Minute {
				t.Error("want 1800 simulated and 2 real workers for 30m")
			}
		}},
		{"soak-steady", func(t *testing.T, s *Scenario) {
			if s.Duration.Duration != 8*time.Hour {
				t.Errorf("duration = %s, want 8h", s.Duration)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Builtin(tc.name)
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, s)
		})
	}
}

// meanSeconds is the mean of a task duration distribution, in seconds.
func meanSeconds(d Dist) float64 {
	switch d.Kind {
	case DistUniform:
		return (d.Min + d.Max) / 2
	case DistLognormal:
		sigma := math.Log(d.P95/d.Median) / 1.6449
		return d.Median * math.Exp(sigma*sigma/2)
	}
	return d.Value
}

// TestBuiltins_FeasibleLoad pins the sizing of the scenarios run locally
// (make load and make soak): the load they offer must fit in 70% of their own
// capacity, worker cores and usage-pool limits alike, and stay under 70% of
// ci-medium's knee on a 4-CPU server (about 7.4 tasks/s). Otherwise the run
// measures a backlog, and its drain fails the terminal invariant on any
// hardware.
func TestBuiltins_FeasibleLoad(t *testing.T) {
	const (
		maxShare = 0.7
		kneeRate = 7.4 // tasks/s: ci-medium at 0.16/s and 0.10/s on 4 CPUs
	)
	for _, name := range []string{"mixed-medium", "soak-steady"} {
		t.Run(name, func(t *testing.T) {
			s, err := Builtin(name)
			if err != nil {
				t.Fatal(err)
			}
			cores := 0.0
			for _, g := range s.Workers {
				cores += float64(g.Count * g.CPU)
			}
			cpu := s.Workers[0].CPU
			var rate, busyCores float64
			inPool := map[string]float64{}
			for _, j := range s.Jobs {
				tasks := float64(j.Steps * j.TasksPerStep)
				offered := j.Rate.PerSecond * tasks // tasks/s
				if j.Total > 0 {
					offered = float64(j.Total) * tasks / s.Duration.Seconds()
				}
				inFlight := offered * meanSeconds(j.Task.Duration)
				perTask := float64(j.Cores)
				if perTask == 0 {
					perTask = float64(cpu) // a whole worker
				}
				rate += offered
				busyCores += inFlight * perTask
				if j.UsagePool != "" {
					inPool[j.UsagePool] += inFlight
				}
			}
			if share := busyCores / cores; share > maxShare {
				t.Errorf("tasks in flight hold %.0f%% of the workers' cores, want at most %.0f%%", share*100, maxShare*100)
			}
			for _, p := range s.Farm.UsagePools {
				if share := inPool[p.Name] / float64(p.Limit); share > maxShare {
					t.Errorf("pool %s is %.0f%% used (%.0f of %d), want at most %.0f%%", p.Name, share*100, inPool[p.Name], p.Limit, maxShare*100)
				}
			}
			if rate > maxShare*kneeRate {
				t.Errorf("offers %.2f tasks/s, want at most %.2f (70%% of ci-medium's knee)", rate, maxShare*kneeRate)
			}
		})
	}
}
