// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import (
	"errors"
	"fmt"
	"maps"
	"slices"
)

// maxTasksPerJob is sqi's cap on the tasks one job may hold, summed over steps.
const maxTasksPerJob = 1_000_000

// Validate reports everything wrong with the scenario for the given target. The
// error joins one message per problem, each starting with the offending field's
// path, for example `jobs[0].queue: "comp2" is not in farm.queues`.
func (s *Scenario) Validate(target TargetKind) error {
	v := &validator{s: s, target: target, queues: map[string]bool{}, pools: map[string]bool{}}
	v.header()
	v.server()
	v.farm()
	v.workers()
	v.jobs()
	v.clients()
	v.actions()
	v.asserts()
	return errors.Join(v.errs...)
}

type validator struct {
	s      *Scenario
	target TargetKind
	queues map[string]bool
	pools  map[string]bool
	errs   []error
}

func (v *validator) failf(path, format string, args ...any) {
	v.errs = append(v.errs, fmt.Errorf("%s: %s", path, fmt.Sprintf(format, args...)))
}

// inUnit reports whether f is in [0, 1]; NaN is not.
func inUnit(f float64) bool { return f >= 0 && f <= 1 }

func (v *validator) header() {
	s := v.s
	if s.Name == "" {
		v.failf("name", "is required")
	}
	switch {
	case s.Duration.Duration <= 0:
		v.failf("duration", "must be positive")
	case s.Warmup.Duration >= s.Duration.Duration:
		v.failf("warmup", "%s must be shorter than duration %s", s.Warmup, s.Duration)
	}
	if s.Warmup.Duration < 0 {
		v.failf("warmup", "must not be negative")
	}
	if s.DrainTimeout.Duration <= 0 {
		v.failf("drain_timeout", "must be positive")
	}
	if !inUnit(s.JobDeps) {
		v.failf("job_deps", "%v is outside [0, 1]", s.JobDeps)
	}
}

func (v *validator) server() {
	switch v.s.Server.Store {
	case "", "sqlite", "postgres":
	default:
		v.failf("server.store", "%q is not one of sqlite, postgres", v.s.Server.Store)
	}
}

func (v *validator) farm() {
	for i, q := range v.s.Farm.Queues {
		path := fmt.Sprintf("farm.queues[%d]", i)
		switch {
		case q.Name == "":
			v.failf(path+".name", "is required")
		case v.queues[q.Name]:
			v.failf(path+".name", "duplicate queue %q", q.Name)
		}
		if q.Name != "" {
			v.queues[q.Name] = true
		}
		if q.MaxConcurrentTasks < 0 {
			v.failf(path+".max_concurrent_tasks", "must not be negative")
		}
	}
	for i, p := range v.s.Farm.UsagePools {
		path := fmt.Sprintf("farm.usage_pools[%d]", i)
		switch {
		case p.Name == "":
			v.failf(path+".name", "is required")
		case v.pools[p.Name]:
			v.failf(path+".name", "duplicate usage pool %q", p.Name)
		}
		if p.Name != "" {
			v.pools[p.Name] = true
		}
		if p.Limit < 0 {
			v.failf(path+".limit", "must not be negative")
		}
	}
}

func (v *validator) workers() {
	seen := map[string]bool{}
	for i := range v.s.Workers {
		w := &v.s.Workers[i]
		path := fmt.Sprintf("workers[%d]", i)
		if w.Name != "" && seen[w.Name] {
			v.failf(path+".name", "duplicate worker group %q", w.Name)
		}
		seen[w.Name] = true
		v.workerCounts(path, w)
		if w.Heartbeat.Duration <= 0 {
			v.failf(path+".heartbeat", "must be positive")
		}
		for j, q := range w.Queues {
			if !v.queues[q] {
				v.failf(fmt.Sprintf("%s.queues[%d]", path, j), "%q is not in farm.queues", q)
			}
		}
		c := w.Churn
		if c.CrashPerHour < 0 || c.DisconnectPerHour < 0 || c.RestartAfter.Duration < 0 {
			v.failf(path+".churn", "rates and restart_after must not be negative")
		}
	}
}

func (v *validator) workerCounts(path string, w *WorkerGroup) {
	if w.Count < 0 || w.Real < 0 {
		v.failf(path+".count", "count and real must not be negative")
	} else if w.Count+w.Real == 0 {
		v.failf(path+".count", "count + real must be above 0")
	}
	if w.Real > 0 && v.target == TargetAttach {
		v.failf(path+".real", "real workers need the managed target; an attached server has its own workers")
	}
	if w.CPU < 0 || w.RAMMb < 0 {
		v.failf(path+".cpu", "cpu and ram_mb must not be negative")
	}
}

func (v *validator) jobs() {
	seen := map[string]bool{}
	for i := range v.s.Jobs {
		j := &v.s.Jobs[i]
		path := fmt.Sprintf("jobs[%d]", i)
		if j.Name != "" && seen[j.Name] {
			v.failf(path+".name", "duplicate job class %q", j.Name)
		}
		seen[j.Name] = true
		v.jobArrival(path, j)
		v.jobShape(path, j)
		v.jobTask(path+".task", &j.Task)
	}
}

func (v *validator) jobArrival(path string, j *JobClass) {
	if j.Weight < 1 {
		v.failf(path+".weight", "must be at least 1")
	}
	if j.Total < 0 {
		v.failf(path+".total", "must not be negative")
	}
	if (j.Rate.PerSecond > 0) == (j.Total > 0) && j.Total >= 0 {
		v.failf(path+".rate", "set exactly one of rate and total")
	}
}

func (v *validator) jobShape(path string, j *JobClass) {
	if !v.queues[j.Queue] {
		v.failf(path+".queue", "%q is not in farm.queues", j.Queue)
	}
	if j.UsagePool != "" && !v.pools[j.UsagePool] {
		v.failf(path+".usage_pool", "%q is not in farm.usage_pools", j.UsagePool)
	}
	if j.Cores < 0 {
		v.failf(path+".cores", "must not be negative")
	}
	if j.Steps < 1 {
		v.failf(path+".steps", "must be at least 1")
	}
	if j.Chunk < 1 {
		v.failf(path+".chunk", "must be at least 1")
	}
	if j.TasksPerStep < 1 {
		v.failf(path+".tasks_per_step", "must be at least 1")
		return
	}
	if j.Steps >= 1 && int64(j.Steps)*int64(j.TasksPerStep) > maxTasksPerJob {
		v.failf(path+".tasks_per_step", "steps x tasks_per_step = %d exceeds sqi's limit of %d tasks per job",
			int64(j.Steps)*int64(j.TasksPerStep), maxTasksPerJob)
	}
	if j.Chunk >= 1 {
		if _, _, ok := SplitTasks(j.TasksPerStep, j.Chunk); !ok {
			v.failf(path+".tasks_per_step", "%d tasks at chunk %d cannot be spread over two task parameters of at most %d values each; use a count with a divisor near %d",
				j.TasksPerStep, j.Chunk, maxTaskParamValues, maxTaskParamValues/j.Chunk)
		}
	}
}

func (v *validator) jobTask(path string, t *TaskSpec) {
	if !inUnit(t.FailRate) {
		v.failf(path+".fail_rate", "%v is outside [0, 1]", t.FailRate)
	}
	if !inUnit(t.HangRate) {
		v.failf(path+".hang_rate", "%v is outside [0, 1]", t.HangRate)
	}
	if t.LogLineBytes < 1 {
		v.failf(path+".log_line_bytes", "must be at least 1")
	}
	if t.Duration.Kind != "" && t.Duration.Unit != "s" {
		v.failf(path+".duration", "must be a duration like 20s, not a plain number")
	}
	if t.LogLines.Kind != "" && t.LogLines.Unit != "" {
		v.failf(path+".log_lines", "must be a unitless count like 200, not a duration")
	}
}

func (v *validator) clients() {
	c := v.s.Clients
	if c.WebSocket.Count < 0 {
		v.failf("clients.websocket.count", "must not be negative")
	}
	if c.RESTPollers.Count < 0 {
		v.failf("clients.rest_pollers.count", "must not be negative")
	}
	if c.RESTPollers.Count > 0 && c.RESTPollers.Interval.Duration <= 0 {
		v.failf("clients.rest_pollers.interval", "must be positive when there are pollers")
	}
}

func (v *validator) actions() {
	for i := range v.s.Actions {
		a := &v.s.Actions[i]
		path := fmt.Sprintf("actions[%d]", i)
		switch {
		case a.At.Duration < 0:
			v.failf(path+".at", "must not be negative")
		case v.s.Duration.Duration > 0 && a.At.Duration > v.s.Duration.Duration:
			v.failf(path+".at", "%s is after duration %s", a.At, v.s.Duration)
		}
		if a.CancelJobs == nil && !a.RetryFailed && a.PauseQueue == "" && a.ResumeQueue == "" && a.DisableWorkers == nil {
			v.failf(path, "names no action; set one of cancel_jobs, retry_failed, pause_queue, resume_queue, disable_workers")
		}
		v.fraction(path+".cancel_jobs.fraction", a.CancelJobs)
		v.fraction(path+".disable_workers.fraction", a.DisableWorkers)
		if a.PauseQueue != "" && !v.queues[a.PauseQueue] {
			v.failf(path+".pause_queue", "%q is not in farm.queues", a.PauseQueue)
		}
		if a.ResumeQueue != "" && !v.queues[a.ResumeQueue] {
			v.failf(path+".resume_queue", "%q is not in farm.queues", a.ResumeQueue)
		}
	}
}

func (v *validator) fraction(path string, f *Fraction) {
	if f != nil && !inUnit(f.Fraction) {
		v.failf(path, "%v is outside [0, 1]", f.Fraction)
	}
}

func (v *validator) asserts() {
	for _, k := range slices.Sorted(maps.Keys(v.s.Assert)) {
		if v.s.Assert[k].Duration < 0 {
			v.failf("assert."+k, "must not be negative")
		}
	}
}
