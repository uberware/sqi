// SPDX-License-Identifier: AGPL-3.0-or-later

package runner

import (
	"cmp"
	"maps"
	"slices"
	"time"

	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/driver"
	"github.com/uberware/sqi/internal/loadtest/report"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/target"
	"github.com/uberware/sqi/internal/worker/protocol"
)

const (
	// maxSampledTasks is about how many tasks the logs and single-attempt
	// invariants read one by one: each sampled task costs a GET, so a large
	// run samples a share of its tasks rather than all of them.
	maxSampledTasks = 2000
	// convergeMargin is added to the server's heartbeat timeout and the
	// slowest worker heartbeat for the online worker count to settle.
	convergeMargin = 10 * time.Second
)

// serverSettings lists the scenario's server settings that differ from a
// default managed server's: the ones an attached run silently cannot apply.
func serverSettings(sc *scenario.Scenario) []string {
	s := sc.Server
	var out []string
	if s.Store != "" && s.Store != "sqlite" {
		out = append(out, "server.store="+s.Store)
	}
	for _, f := range []struct {
		name string
		on   bool
	}{{"server.broker_auth", s.BrokerAuth}, {"server.http_auth", s.HTTPAuth}, {"server.tls", s.TLS}} {
		if f.on {
			out = append(out, f.name+"=true")
		}
	}
	for _, k := range slices.Sorted(maps.Keys(s.Config)) {
		out = append(out, "server.config."+k)
	}
	return out
}

// poolNames maps each scenario usage pool to the name the target knows it by.
func poolNames(t target.Target, sc *scenario.Scenario) map[string]string {
	out := make(map[string]string, len(sc.Farm.UsagePools))
	for _, p := range sc.Farm.UsagePools {
		out[p.Name] = t.PoolName(p.Name)
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
	var slowest time.Duration
	for _, g := range sc.Workers {
		slowest = max(slowest, g.Heartbeat.Duration)
	}
	return heartbeatTimeout(sc) + slowest + convergeMargin
}

// heartbeatTimeout is the server's heartbeat timeout: the scenario's
// scheduler.heartbeat_timeout, else the server default.
func heartbeatTimeout(sc *scenario.Scenario) time.Duration {
	hb := config.DefaultConfig().Scheduler.HeartbeatTimeout
	if sched, ok := sc.Server.Config["scheduler"].(map[string]any); ok {
		if s, ok := sched["heartbeat_timeout"].(string); ok {
			if d, err := time.ParseDuration(s); err == nil && d > 0 {
				hb = d
			}
		}
	}
	return hb
}

// serverSweepInterval is how often the server's heartbeat sweep marks silent
// workers offline (the scheduler's HeartbeatSweepInterval, which the server
// does not make configurable).
const serverSweepInterval = 15 * time.Second

// livenessGrace is how long after its broker connection comes up the server
// marks no worker offline, so that a live worker redialing on its own backoff
// keeps its tasks: internal/server's busReconnectGrace, which is not
// configurable.
func livenessGrace(workerTimeout time.Duration) time.Duration {
	redial := time.Duration(float64(protocol.MaxReconnectBackoff) * (1 + protocol.ReconnectJitterFraction))
	return workerTimeout + redial + protocol.DefaultHeartbeatInterval
}

// chaosConvergeWithin is convergeWithin for a chaos run, measured from now:
// a worker a fault left dead stays online on the server until the server's
// liveness grace after its last connection to the broker has passed and a
// sweep has run. That connection came up when the target started (serverUp)
// and again at each restart of the server by a fault, by the time the fault
// healed (now for one that never healed).
func chaosConvergeWithin(sc *scenario.Scenario, serverUp time.Time, faults []report.FaultRecord, now time.Time) time.Duration {
	last := serverUp
	for _, f := range faults {
		if f.Target != "server" && f.Kind != scenario.FaultPowerCut {
			continue
		}
		at := f.Healed
		if at.IsZero() {
			at = now
		}
		if at.After(last) {
			last = at
		}
	}
	quiet := last.Add(livenessGrace(heartbeatTimeout(sc)) + serverSweepInterval + convergeMargin)
	return max(convergeWithin(sc), quiet.Sub(now))
}

// chaosScenario reports whether a run of sc is a chaos run on a target that
// can inject faults: it has faults, a rework budget, known violations or a
// job class with a real task.
func chaosScenario(sc *scenario.Scenario) bool {
	if len(sc.Faults) > 0 || sc.Expect.MaxReworkAttempts != nil || len(sc.Expect.KnownViolations) > 0 {
		return true
	}
	return slices.ContainsFunc(sc.Jobs, func(j scenario.JobClass) bool { return j.RealTask != nil })
}

// orphanJobs returns the farm's jobs that are not accepted submissions but
// carry the name of a submission the server gave no 2xx answer (a timeout, a
// dropped connection, a 5xx), each as that submission with its ID set; the
// Status stays the one the client saw, so it is still not Accepted. Job
// names are unique within a run (class and sequence number), and every
// submission is in the ledger once the submitter has returned, the ones that
// timed out mid-flight too, so a name maps to one submission. Each
// submission takes the first-created job of its name; a second one, like a
// job matching no submission at all, is left for submission-atomic to report.
// The result is never nil.
func orphanJobs(farm map[string]client.Job, accepted, rejected []driver.SubmittedJob) []driver.SubmittedJob {
	known := make(map[string]bool, len(accepted))
	for _, j := range accepted {
		known[j.ID] = true
	}
	byName := make(map[string]driver.SubmittedJob, len(rejected))
	for _, j := range rejected {
		if _, dup := byName[j.Name]; !dup && j.Name != "" {
			byName[j.Name] = j
		}
	}
	jobs := slices.Collect(maps.Values(farm))
	slices.SortFunc(jobs, func(a, b client.Job) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	out := []driver.SubmittedJob{}
	for _, j := range jobs {
		sub, ok := byName[j.Name]
		if known[j.ID] || !ok {
			continue
		}
		delete(byName, j.Name)
		sub.ID = j.ID
		out = append(out, sub)
	}
	return out
}

// logOverflowExpected reports whether the scenario plans to write more task
// log bytes than the server's log stream keeps (its max_store_mb), in which
// case the broker evicting a task's oldest whole chunks is not a defect. The
// plan is the jobs the classes submit (total, or rate over the duration),
// times their tasks, times the mean log lines per task and the line size. The
// cap is read from the scenario's server config, so against an attached
// server, whose own cap the harness cannot see, a lower cap there turns an
// expected eviction into a logs violation.
func logOverflowExpected(sc *scenario.Scenario) bool {
	var planned float64
	for _, j := range sc.Jobs {
		jobs := float64(j.Total)
		if j.Rate.PerSecond > 0 {
			jobs = j.Rate.PerSecond * sc.Duration.Seconds()
		}
		tasks := jobs * float64(max(j.Steps, 1)*j.TasksPerStep)
		planned += tasks * j.Task.LogLines.Mean() * float64(j.Task.LogLineBytes)
	}
	return planned > report.StreamCapMB(sc)*(1<<20)
}
