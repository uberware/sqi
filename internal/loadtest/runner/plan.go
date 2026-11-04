// SPDX-License-Identifier: AGPL-3.0-or-later

package runner

import (
	"math"
	"time"

	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/loadtest/driver"
	"github.com/uberware/sqi/internal/loadtest/report"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/target"
)

const (
	// maxSampledTasks is about how many tasks the logs and single-attempt
	// invariants read one by one: each sampled task costs a GET, so a large
	// run samples a share of its tasks rather than all of them.
	maxSampledTasks = 2000
	// convergeMargin is added to the server's heartbeat timeout and the
	// slowest worker heartbeat for the online worker count to settle.
	convergeMargin = 10 * time.Second
	// lognormalP95Z is the standard normal's 95th percentile.
	lognormalP95Z = 1.6449
)

// poolNames maps each scenario usage pool to the name the target knows it by.
func poolNames(t target.Target, sc *scenario.Scenario) map[string]string {
	out := make(map[string]string, len(sc.Farm.UsagePools))
	for _, p := range sc.Farm.UsagePools {
		out[p.Name] = target.PoolName(t, p.Name)
	}
	return out
}

// taskSpecs maps each job class to the task behavior its workers simulate.
func taskSpecs(sc *scenario.Scenario) map[string]scenario.TaskSpec {
	out := make(map[string]scenario.TaskSpec, len(sc.Jobs))
	for _, j := range sc.Jobs {
		out[j.Name] = j.Task
	}
	return out
}

// simulatedCount is the size of the simulated fleet.
func simulatedCount(sc *scenario.Scenario) int {
	n := 0
	for _, g := range sc.Workers {
		n += g.Count
	}
	return n
}

// realCount is how many real sqi-worker processes the scenario runs.
func realCount(sc *scenario.Scenario) int {
	n := 0
	for _, g := range sc.Workers {
		n += g.Real
	}
	return n
}

// sampleShare is the share of tasks the logs and attempts checks read: all of
// them up to maxSampledTasks, and that many of a larger run.
func sampleShare(sc *scenario.Scenario, jobs []driver.SubmittedJob) float64 {
	perJob := make(map[string]int, len(sc.Jobs))
	for _, j := range sc.Jobs {
		perJob[j.Name] = max(j.Steps, 1) * j.TasksPerStep
	}
	tasks := 0
	for _, j := range jobs {
		tasks += perJob[j.Class]
	}
	if tasks <= maxSampledTasks {
		return 1
	}
	return float64(maxSampledTasks) / float64(tasks)
}

// convergeWithin is how long the online worker count may take to match the
// live fleet: the server's heartbeat timeout (the scenario's
// scheduler.heartbeat_timeout, else the server default), plus the slowest
// worker group's heartbeat, plus a margin. An attached server's own timeout
// is not visible to the harness, so the default is assumed there.
func convergeWithin(sc *scenario.Scenario) time.Duration {
	hb := config.DefaultConfig().Scheduler.HeartbeatTimeout
	if sched, ok := sc.Server.Config["scheduler"].(map[string]any); ok {
		if s, ok := sched["heartbeat_timeout"].(string); ok {
			if d, err := time.ParseDuration(s); err == nil && d > 0 {
				hb = d
			}
		}
	}
	var slowest time.Duration
	for _, g := range sc.Workers {
		slowest = max(slowest, g.Heartbeat.Duration)
	}
	return hb + slowest + convergeMargin
}

// logOverflowExpected reports whether the scenario plans to write more task
// log bytes than the server's log stream keeps (its max_store_mb), in which
// case the broker evicting a task's oldest whole chunks is not a defect. The
// plan is the jobs the classes submit (total, or rate over the duration),
// times their tasks, times the mean log lines per task and the line size.
func logOverflowExpected(sc *scenario.Scenario) bool {
	var planned float64
	for _, j := range sc.Jobs {
		jobs := float64(j.Total)
		if j.Rate.PerSecond > 0 {
			jobs = j.Rate.PerSecond * sc.Duration.Seconds()
		}
		tasks := jobs * float64(max(j.Steps, 1)*j.TasksPerStep)
		planned += tasks * distMean(j.Task.LogLines) * float64(j.Task.LogLineBytes)
	}
	return planned > report.StreamCapMB(sc)*(1<<20)
}

// distMean is the mean of a distribution.
func distMean(d scenario.Dist) float64 {
	switch d.Kind {
	case scenario.DistConstant:
		return d.Value
	case scenario.DistUniform:
		return (d.Min + d.Max) / 2
	case scenario.DistLognormal:
		sigma := math.Log(d.P95/d.Median) / lognormalP95Z
		return d.Median * math.Exp(sigma*sigma/2)
	}
	return 0
}
