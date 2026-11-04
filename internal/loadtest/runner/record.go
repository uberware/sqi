// SPDX-License-Identifier: AGPL-3.0-or-later

package runner

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/driver"
	"github.com/uberware/sqi/internal/loadtest/measure"
	"github.com/uberware/sqi/internal/loadtest/report"
	"github.com/uberware/sqi/internal/loadtest/simworker"
	"github.com/uberware/sqi/internal/loadtest/verify"
	"github.com/uberware/sqi/internal/version"
)

// Rules of the findings the runner adds to the report's own.
const (
	// RuleChurnFailed flags churn events whose restart or reconnect failed:
	// the fleet ran short of the workers the scenario asked for.
	RuleChurnFailed = "churn-failed"
	// RuleGeneratorSaturated flags an INVALID run accepted with
	// AllowSaturated.
	RuleGeneratorSaturated = "generator-saturated"
)

// listPage is the page size when the runner lists the farm's jobs.
const listPage = 1000

// record assembles the run's record. Every recording has stopped by now: the
// submitter, the UI clients and the fleet are done, and the observer has
// taken its last sample.
func (r *run) record() *report.Record {
	rec := &report.Record{
		RunID:    r.o.RunID,
		Mode:     r.o.Mode,
		Shard:    r.o.Shard.String(),
		Complete: r.complete,
		Scenario: r.sc,
		Env:      r.env(),
		Started:  r.started,
		Ended:    r.ended,
	}
	if rec.Started.IsZero() { // interrupted before the first submission
		rec.Started, rec.Ended = r.began, time.Now()
	}
	accepted := r.jobs.Accepted()
	ledgerHistograms(r.ledger.Events(), accepted, r.metrics)
	jobHistogram(r.serverJobs, accepted, r.metrics)
	rec.Histograms = histograms(r.metrics)
	if r.uiRunning {
		for k := range rec.Histograms {
			if k == report.HistWSLag || strings.HasPrefix(k, report.HistRESTPrefix) {
				delete(rec.Histograms, k)
			}
		}
		r.unavail("ui_clients", "the WebSocket clients and REST pollers did not stop in time, so their histograms are left out")
	}
	rec.Counters = r.liveCounters()
	switch {
	case r.serverErr != nil:
		r.unavail(report.CounterTasksTerminal, "listing the farm's jobs at the end failed: "+r.serverErr.Error())
	case r.serverJobs != nil:
		rec.Counters[report.CounterTasksTerminal] = terminalTasks(r.serverJobs)
	}
	r.mu.Lock()
	rec.SteadyCounters = completeSteady(r.steady, rec.Counters)
	r.mu.Unlock()
	rec.Series = r.recorder.Series()
	if rec.Series == nil {
		rec.Series = []measure.Series{}
	}
	rec.Invariants = r.invariants
	if rec.Invariants == nil {
		rec.Invariants = []verify.Result{}
		r.unavail("invariants", "the run ended early (interrupted, or a driver failed) before they were checked")
	}
	rec.Valid = true
	if r.obs != nil {
		rec.Valid, rec.InvalidReason = r.obs.SelfHealth().Valid(rec.SteadyFrom(), 0)
	}
	r.unavail(report.HistLeaseReply, "the simulated workers do not time their lease requests")
	rec.Unavailable = r.unavailableList()
	rec.Findings = append(append([]report.Finding{}, report.Findings(rec, rec.SteadyFrom())...), r.findings(rec)...)
	return rec
}

func (r *run) env() report.Env {
	env := report.LocalEnv()
	env.SQIVersion, env.Commit = version.Version, version.Commit
	env.Store = r.sc.Server.Store
	env.Flags = slices.Clone(r.o.Flags)
	if env.Flags == nil {
		env.Flags = []string{}
	}
	return env
}

// unavailableList merges the observer's unavailable measurements with the
// runner's, sorted and without duplicates.
func (r *run) unavailableList() []string {
	out := []string{}
	if r.obs != nil {
		out = append(out, r.obs.Unavailable()...)
	}
	r.mu.Lock()
	out = append(out, r.unavailable...)
	r.mu.Unlock()
	slices.Sort(out)
	return slices.Compact(out)
}

// findings are the runner's own findings: failed churn and an INVALID run
// accepted with AllowSaturated.
func (r *run) findings(rec *report.Record) []report.Finding {
	var out []report.Finding
	if r.fleet != nil {
		if cc := r.fleet.ChurnCounts(); cc.Failures > 0 {
			out = append(out, report.Finding{
				Rule: RuleChurnFailed, Severity: report.SeverityError,
				Message: fmt.Sprintf("%d churn events failed to restart or reconnect their worker (of %d crashes and %d disconnects), so the fleet ran short of the scenario's workers",
					cc.Failures, cc.Crashes, cc.Disconnects),
			})
		}
	}
	if !rec.Valid && r.o.AllowSaturated {
		out = append(out, report.Finding{
			Rule: RuleGeneratorSaturated, Severity: report.SeverityWarn,
			Message: "the generator saturated (" + rec.InvalidReason + "); accepted with --allow-saturated, so its latencies describe the generator as much as the server",
		})
	}
	return out
}

// liveCounters reads every counter the driver keeps. tasks.terminal comes
// from the server and is added by the caller.
func (r *run) liveCounters() map[string]int64 {
	m := r.metrics
	out := map[string]int64{
		report.CounterJobsSubmitted: int64(r.jobs.AcceptedCount()),
		report.CounterWorkersLive:   int64(simulatedCount(r.sc) + realCount(r.sc)),
		report.CounterWSDrops:       m.WSDrops.Load(),
		report.CounterWSEvents:      m.WSEvents.Load(),
		report.CounterWSErrors:      m.WSErrors.Load(),
		report.CounterRESTErrors:    m.RESTErrors.Load(),
	}
	for code, n := range m.StatusCounts() {
		out[report.CounterSubmitStatusPrefix+strconv.Itoa(code)] = n
	}
	return out
}

// completeSteady returns the counters snapshotted at the end of warmup with
// every key of final present: a counter first seen after the snapshot (a
// status code, say) was 0 then. A missing key would read as 0 anyway, but
// the record says so explicitly. No snapshot stays nil.
func completeSteady(steady, final map[string]int64) map[string]int64 {
	if steady == nil {
		return nil
	}
	out := maps.Clone(steady)
	for k := range final {
		if _, ok := out[k]; !ok {
			out[k] = 0
		}
	}
	return out
}

// histograms maps the driver's histograms to the record's keys. The lease
// reply histogram is left out: nothing fills it (see Unavailable).
func histograms(m *driver.Metrics) map[string]*measure.Histogram {
	h := map[string]*measure.Histogram{
		report.HistSubmitLatency: m.SubmitLatency,
		report.HistSubmitLag:     m.SubmitLag,
		report.HistFirstLease:    m.FirstLease,
		report.HistTaskE2E:       m.TaskE2E,
		report.HistJobE2E:        m.JobE2E,
		report.HistWSLag:         m.WSLag,
	}
	for name, rh := range m.REST {
		h[report.HistRESTPrefix+name] = rh
	}
	return h
}

// ledgerHistograms fills FirstLease and TaskE2E from what the simulated
// workers did. FirstLease is a job's submission (SubmittedAt: when the
// request left, since a task can be leased before the server's answer
// arrives) to the first lease of any of its tasks. TaskE2E is one attempt's
// lease to its terminal status: succeeded, failed or canceled; a crash or a
// shutdown give-up does not end a task. Real workers keep no ledger, so work
// only they ran is not counted.
func ledgerHistograms(events []simworker.Event, jobs []driver.SubmittedJob, m *driver.Metrics) {
	firstLease := map[string]time.Time{}
	leasedAt := map[string]time.Time{}
	for _, e := range events {
		switch e.Kind {
		case simworker.KindLeased:
			if t, ok := firstLease[e.JobID]; !ok || e.At.Before(t) {
				firstLease[e.JobID] = e.At
			}
			leasedAt[e.AttemptID] = e.At
		case simworker.KindTerminal, simworker.KindCanceled:
			if e.Kind == simworker.KindTerminal && e.Outcome != simworker.OutcomeSucceeded && e.Outcome != simworker.OutcomeFailed {
				continue
			}
			if t, ok := leasedAt[e.AttemptID]; ok {
				m.TaskE2E.Record(max(e.At.Sub(t), 0))
				delete(leasedAt, e.AttemptID)
			}
		}
	}
	for _, j := range jobs {
		if t, ok := firstLease[j.ID]; ok {
			m.FirstLease.Record(max(t.Sub(j.SubmittedAt), 0))
		}
	}
}

// jobHistogram fills JobE2E: each accepted job's submission to the time the
// server last updated it, for a job the server holds as terminal.
func jobHistogram(server map[string]client.Job, jobs []driver.SubmittedJob, m *driver.Metrics) {
	for _, j := range jobs {
		s, ok := server[j.ID]
		if !ok || !terminalJob(s.Status) {
			continue
		}
		m.JobE2E.Record(max(s.UpdatedAt.Sub(j.SubmittedAt), 0))
	}
}

func terminalJob(status string) bool {
	return status == "completed" || status == "failed" || status == "canceled"
}

// terminalTasks counts the tasks the server holds as succeeded, failed or
// canceled: each task once, whatever its attempts.
func terminalTasks(jobs map[string]client.Job) int64 {
	var n int64
	for _, j := range jobs {
		n += int64(j.TaskCounts["succeeded"] + j.TaskCounts["failed"] + j.TaskCounts["canceled"])
	}
	return n
}

// listFarmJobs reads every job of the farm, a page at a time, by ID.
func listFarmJobs(ctx context.Context, c *client.Client, farmID string) (map[string]client.Job, error) {
	out := map[string]client.Job{}
	for offset := 0; ; {
		page, err := c.ListJobs(ctx, farmID, listPage, offset)
		if err != nil {
			return nil, err
		}
		for _, j := range page.Items {
			out[j.ID] = j
		}
		offset += len(page.Items)
		if len(page.Items) == 0 || offset >= page.Total {
			return out, nil
		}
	}
}
