// SPDX-License-Identifier: AGPL-3.0-or-later

// Package verify decides whether a load run passed. It compares what the
// driver submitted and what the simulated workers did (the [simworker.Ledger])
// with what the server reports, through the six run invariants of the load
// harness: every job terminal, every task's status the one the simulator
// decided, one live attempt per task at a time, the logs the simulator sent,
// the online worker count, and the queue and usage-pool caps. A chaos run
// adds a seventh, submission-atomic: every job the server holds is one the
// run submitted, and a submission that got no 2xx answer created all of its
// job or none of it. The logs invariant allows lines lost inside a fault
// window, and counts them.
//
// Facts about the server the checks rely on:
//
//   - The attempt ID in an assignment, and so in every ledger event, is the
//     server's attempt row ID. The server's log chunks carry it as
//     [client.LogLine.AttemptID]. GET /tasks/{id}/attempts does not expose it,
//     only the attempt number, which the client returns as
//     [client.Attempt.ID].
//   - Attempt statuses are running, succeeded, failed and canceled. An attempt
//     the server reclaims from a crashed, restarted or silent worker is closed
//     as failed, so a retry and a reclaim both follow a failed attempt, and a
//     canceled task's open attempt is closed as canceled.
//   - GET /tasks/{id}/logs returns only the task's latest attempt. A chunk is
//     several lines joined with "\n", numbered from 1 per attempt (seq_num),
//     and listed in broker order (nats_seq), which is not always seq_num order.
//   - List pages are capped at 1000 items.
package verify

import (
	"cmp"
	"context"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/loadtest/internal/sleep"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/driver"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
	"github.com/uberware/sqi/internal/store"
)

// Invariant names, in the order [Check] returns them.
const (
	NameTerminal        = "terminal"
	NameOutcomes        = "outcomes"
	NameSingleAttempt   = "single-attempt"
	NameLogs            = "logs"
	NameWorkersConverge = "workers-converge"
	NameCaps            = "caps"
	// NameSubmissionAtomic follows caps in a chaos run's results.
	NameSubmissionAtomic = "submission-atomic"
)

// CounterLogsLostInFault is the logs result's counter of the lines the
// sampled tasks lost inside fault windows.
const CounterLogsLostInFault = "logs.lost_in_fault"

// Names returns every invariant name the harness can report, in the order
// scenario.CheckNames lists them (the chaos checks included). The last four
// are reported by package chaos, from the stopped server's store and the real
// tasks' markers.
func Names() []string {
	return []string{
		NameTerminal, NameOutcomes, NameSingleAttempt, NameLogs, NameWorkersConverge, NameCaps,
		NameSubmissionAtomic,
		"executions-recorded", "store-integrity", "no-orphans", "single-writer",
	}
}

const (
	// MaxEvidence is the most evidence entries a Result carries. When there is
	// more, the last entry counts what was left out.
	MaxEvidence = 20
	// DefaultSettle is the settle time when Input.Settle is zero.
	DefaultSettle = 5 * time.Second
	// DefaultLogSettle is the logs settle time when Input.LogSettle is zero.
	// It is longer than DefaultSettle because the log consumer has a backlog
	// of its own that WaitDrained, which waits on job statuses, does not see.
	DefaultLogSettle = 30 * time.Second
	// maxPage is the largest page the server's list endpoints serve.
	maxPage = 1000
	// parallelism bounds the per-job and per-task calls in flight.
	parallelism = 16
	// fewJobs is the most jobs whose statuses are read one GET at a time;
	// more are read from the farm's job list.
	fewJobs = 20
	// settlePoll is the first pause between two re-reads while settling; it
	// doubles after each re-read up to settlePollMax, so a lasting
	// disagreement costs a few re-reads instead of one every 100 ms.
	settlePoll    = 100 * time.Millisecond
	settlePollMax = time.Second
)

// Server statuses, as the REST API spells them.
const (
	jobCompleted = string(store.JobStatusCompleted)
	statusOK     = string(store.TaskStatusSucceeded)
	statusFailed = string(store.TaskStatusFailed)
	statusCancel = string(store.TaskStatusCanceled)
	workerOnline = string(store.WorkerStatusOnline)
	workerOff    = string(store.WorkerStatusDisabled)
)

// Sampling salts.
const (
	saltAttempts = "attempts"
	saltLogs     = "logs"
)

// terminalJob reports whether a job status is one a job cannot leave on its
// own.
func terminalJob(status string) bool { return store.JobStatus(status).IsTerminal() }

// ServerView is the part of the server's REST API the checks read.
// *client.Client satisfies it. The checks do not retry a failed read, so pass
// a client from [client.Client.RetryingReads]: a passing fault is then
// retried, and only one that outlasts the client's retry budget is reported.
type ServerView interface {
	GetJob(ctx context.Context, id string) (client.Job, error)
	ListJobs(ctx context.Context, farmID string, limit, offset int) (client.Page[client.Job], error)
	ListTasks(ctx context.Context, jobID string, limit, offset int) (client.Page[client.Task], error)
	ListAttempts(ctx context.Context, taskID string) ([]client.Attempt, error)
	TaskLogs(ctx context.Context, taskID string, after int64, limit int) ([]client.LogLine, int64, error)
	ListWorkers(ctx context.Context, farmID string, limit, offset int) (client.Page[client.Worker], error)
}

// Input is everything the checks compare.
type Input struct {
	// Jobs are the run's submissions; rejected ones are ignored.
	Jobs []driver.SubmittedJob
	// Ledger is what the simulated workers did. Nil is an empty ledger.
	Ledger *simworker.Ledger
	// Scenario gives each job class's queue, usage pool and log line size,
	// and the farm's caps.
	Scenario *scenario.Scenario
	// PoolNames maps a scenario usage pool name to the server's, for
	// evidence: an attached run's pools are its own, namespaced ones. A
	// missing entry is the scenario's name.
	PoolNames map[string]string
	FarmID    string
	// LiveWorkers is how many workers of the farm are running when Check is
	// called: the simulated fleet's live count plus any real workers.
	LiveWorkers int
	// LogSample is the share of tasks whose logs are compared, 1.0 for all.
	LogSample float64
	// LogOverflowExpected says the scenario overflows the broker's store on
	// purpose, so losing a task's oldest whole chunks is allowed.
	LogOverflowExpected bool
	// AttemptSample is the share of tasks whose attempts are read, 1.0 for
	// all. Each sampled task costs one GET /tasks/{id}/attempts.
	AttemptSample float64
	// Seed picks the sampled tasks: the same seed picks the same tasks.
	Seed uint64
	// CanceledAt is when the first cancel action targeting each job started
	// ([CancelTimes]). A task of one of its jobs may be canceled whatever the
	// simulator did, and caps ends a canceled attempt of one of them no later
	// than that time, when the server may already have freed its slot.
	CanceledAt map[string]time.Time
	// ConvergeWithin is how long the online worker count may take to reach
	// LiveWorkers: the heartbeat timeout plus a margin. Zero checks once.
	ConvergeWithin time.Duration
	// Settle is how long, after the first pass, a task whose outcome
	// disagrees is re-read before the disagreement counts: the ledger records
	// a terminal event just after the server may already have applied it.
	// Zero means DefaultSettle. A disagreeing task is always re-read once.
	Settle time.Duration
	// LogSettle is the same for logs, whose chunks are ingested on a stream of
	// their own. Zero means DefaultLogSettle.
	LogSettle time.Duration
	// Simulated reports whether a worker ID is one of the simulated fleet's.
	// The ledger records only what simulated workers did, so the outcomes and
	// logs checks leave out a task a real sqi-worker ran, and caps does not
	// count its attempts. Nil means every worker is simulated.
	Simulated func(workerID string) bool
	// TaskSeen, when set, is called with every task the checks list, as the
	// server first lists it. Calls come from several goroutines at once.
	TaskSeen func(jobID string, t client.Task)

	// Orphans are the jobs the server created although their submission got
	// no 2xx answer (a timeout, a dropped connection, a 5xx), with ID set to
	// the server's job ID. terminal covers them beside the accepted jobs, and
	// caps counts their attempts.
	Orphans []driver.SubmittedJob
	// FaultWindows are when the run's faults were in effect. A sampled task
	// whose latest attempt overlaps a window that hit its worker may have
	// lost log lines (see the logs invariant).
	FaultWindows []FaultWindow
	// GroupOf names a simulated worker's group. Nil: no worker is in a
	// window's group list, so only a window on every group applies.
	GroupOf func(workerID string) string
}

// chaos reports whether the input is a chaos run's, which adds
// submission-atomic to the results: Orphans or FaultWindows non-nil.
func (in Input) chaos() bool { return in.Orphans != nil || in.FaultWindows != nil }

// Result is one invariant's verdict. Evidence names the job, task, attempt,
// queue or pool of each violation with the observed and expected values; it
// is empty when OK. Counters are what the check measured besides its verdict,
// such as [CounterLogsLostInFault].
type Result struct {
	Name     string
	OK       bool
	Evidence []string
	Counters map[string]int64 `json:"counters,omitempty"`
}

// FaultWindow is when a network, server or power-cut fault was in effect: from
// when it fired to when it healed (or, for one that never healed, to the end
// of the run).
type FaultWindow struct {
	Start, End time.Time
	// Groups are the worker groups the fault hit; empty means every group, as
	// for a server or power-cut fault.
	Groups []string
}

// Check evaluates the run invariants and returns their results in a fixed
// order: terminal, outcomes, single-attempt, logs, workers-converge, caps,
// and for a chaos run (Input.Orphans or Input.FaultWindows non-nil)
// submission-atomic. Call it once [WaitDrained] has returned nil and the
// fleet's churn has stopped, with the fleet still running.
//
// Its server calls: the accepted jobs' and orphans' statuses (one GET each up
// to 20 jobs, otherwise the farm's job list, 1000 a page), every accepted
// job's task list (1000 a page), one attempt list for each task sampled at
// AttemptSample, one log read for each task sampled at LogSample (more for a
// task over 1000 chunks), and the farm's worker list once per poll; in a
// chaos run also the farm's job list and every orphan's task list.
func Check(ctx context.Context, v ServerView, in Input) []Result {
	c := newChecker(v, in)
	c.ledger = indexLedger(in.Ledger)
	workers := make(chan Result, 1)
	go func() { workers <- c.workersConverge(ctx) }()

	statuses, errs := jobStatuses(ctx, v, in.FarmID, append(slices.Clone(c.order), c.orphanIDs...))
	sweep := c.sweepTasks(ctx, statuses)
	results := []Result{
		c.terminal(statuses, errs),
		c.outcomes(ctx, sweep),
		c.singleAttempt(ctx, sweep.attempts),
		c.logs(ctx, sweep.logs),
		<-workers,
		c.caps(),
	}
	if in.chaos() {
		results = append(results, c.submissionAtomic(ctx))
	}
	return results
}

// checker holds what every check looks up.
type checker struct {
	v       ServerView
	in      Input
	jobs    map[string]driver.SubmittedJob // accepted, by ID
	order   []string                       // accepted job IDs, in submission order
	orphans map[string]driver.SubmittedJob // by ID
	// orphanIDs are the orphans' IDs, in Input order, none of them accepted.
	orphanIDs []string
	canceled  map[string]bool
	classes   map[string]scenario.JobClass // by name
	settle    time.Duration
	// logSettle is the logs check's settle time.
	logSettle time.Duration
	ledger    *ledgerIndex

	mu sync.Mutex
	// lost is the log lines each sampled task lost inside a fault window, by
	// task ID, as its latest logs read found.
	lost map[string]int
}

func newChecker(v ServerView, in Input) *checker {
	c := &checker{
		v: v, in: in,
		jobs:      map[string]driver.SubmittedJob{},
		orphans:   map[string]driver.SubmittedJob{},
		canceled:  map[string]bool{},
		classes:   map[string]scenario.JobClass{},
		settle:    cmp.Or(in.Settle, DefaultSettle),
		logSettle: cmp.Or(in.LogSettle, DefaultLogSettle),
		lost:      map[string]int{},
	}
	c.order = acceptedIDs(in.Jobs)
	for _, j := range in.Jobs {
		if j.Accepted() {
			c.jobs[j.ID] = j
		}
	}
	for _, j := range in.Orphans {
		if _, dup := c.orphans[j.ID]; j.ID == "" || dup || c.jobs[j.ID].ID != "" {
			continue
		}
		c.orphans[j.ID] = j
		c.orphanIDs = append(c.orphanIDs, j.ID)
	}
	for id := range in.CanceledAt {
		c.canceled[id] = true
	}
	if in.Scenario != nil {
		for _, jc := range in.Scenario.Jobs {
			c.classes[jc.Name] = jc
		}
	}
	return c
}

// realWorker reports whether the server names a worker outside the
// simulated fleet, whose work the ledger cannot speak for.
func (c *checker) realWorker(workerID string) bool {
	return workerID != "" && c.in.Simulated != nil && !c.in.Simulated(workerID)
}

// classOf returns the scenario class of an accepted job or an orphan.
func (c *checker) classOf(jobID string) (scenario.JobClass, bool) {
	j, ok := c.jobs[jobID]
	if !ok {
		if j, ok = c.orphans[jobID]; !ok {
			return scenario.JobClass{}, false
		}
	}
	jc, ok := c.classes[j.Class]
	return jc, ok
}

// WaitDrained polls the jobs the server holds, the accepted ones and the
// orphans (a submission with no 2xx answer whose ID the caller set from the
// farm's job list), until every one is terminal (completed, failed or
// canceled) and returns nil, or returns an error naming up to 20 of the jobs
// still open, with their statuses, once timeout has elapsed or ctx has ended.
// A submission with no ID is ignored. Up to 20 jobs are read one GET at a
// time; more are read from the farm's job list, 1000 a page.
func WaitDrained(ctx context.Context, v ServerView, farmID string, jobs []driver.SubmittedJob, timeout time.Duration) error {
	pending := heldIDs(jobs)
	total := len(pending)
	deadline := time.Now().Add(timeout)
	poll := pollInterval(timeout)
	last := map[string]string{}
	for {
		statuses, errs := jobStatuses(ctx, v, farmID, pending)
		open := pending[:0:0]
		for _, id := range pending {
			switch {
			case errs[id] != nil:
				last[id] = "unknown: " + errs[id].Error()
				open = append(open, id)
			case !terminalJob(statuses[id]):
				last[id] = statuses[id]
				open = append(open, id)
			}
		}
		pending = open
		if len(pending) == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("verify: %d of %d jobs not terminal: %s: %w", len(pending), total, describeJobs(pending, last), err)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("verify: %d of %d jobs not terminal after %s: %s", len(pending), total, timeout, describeJobs(pending, last))
		}
		sleep.For(ctx, min(poll, time.Until(deadline)))
	}
}

// describeJobs lists up to MaxEvidence jobs as "id (status)".
func describeJobs(ids []string, status map[string]string) string {
	parts := make([]string, 0, min(len(ids), MaxEvidence))
	for _, id := range ids[:min(len(ids), MaxEvidence)] {
		parts = append(parts, fmt.Sprintf("%s (%s)", id, status[id]))
	}
	s := strings.Join(parts, ", ")
	if len(ids) > MaxEvidence {
		s += fmt.Sprintf(" and %d more", len(ids)-MaxEvidence)
	}
	return s
}

// CancelTimes returns, for each job a cancel_jobs action targeted, when the
// first such action started, for Input.CanceledAt.
func CancelTimes(records []driver.ActionRecord) map[string]time.Time {
	out := map[string]time.Time{}
	for _, r := range records {
		if r.Kind != driver.ActionCancelJobs {
			continue
		}
		for _, id := range r.JobIDs {
			if at, ok := out[id]; !ok || r.Started.Before(at) {
				out[id] = r.Started
			}
		}
	}
	return out
}

// acceptedIDs returns the IDs of the accepted jobs, each once, in order.
func acceptedIDs(jobs []driver.SubmittedJob) []string {
	seen := map[string]bool{}
	var ids []string
	for _, j := range jobs {
		if j.Accepted() && !seen[j.ID] {
			seen[j.ID] = true
			ids = append(ids, j.ID)
		}
	}
	return ids
}

// heldIDs returns the IDs of the jobs the server holds, accepted or orphaned,
// each once, in order.
func heldIDs(jobs []driver.SubmittedJob) []string {
	seen := map[string]bool{}
	var ids []string
	for _, j := range jobs {
		if j.ID != "" && !seen[j.ID] {
			seen[j.ID] = true
			ids = append(ids, j.ID)
		}
	}
	return ids
}

// jobStatuses returns the status of every job in ids, or the error that kept
// it from being read. Every ID is in one of the two maps.
func jobStatuses(ctx context.Context, v ServerView, farmID string, ids []string) (statuses map[string]string, errs map[string]error) {
	statuses, errs = map[string]string{}, map[string]error{}
	if len(ids) > fewJobs {
		want := map[string]bool{}
		for _, id := range ids {
			want[id] = true
		}
		err := listFarmJobs(ctx, v, farmID, func(j client.Job) {
			if want[j.ID] {
				statuses[j.ID] = j.Status
			}
		})
		if err != nil {
			for _, id := range ids {
				if _, ok := statuses[id]; !ok {
					errs[id] = err
				}
			}
			return statuses, errs
		}
	}
	var missing []string
	for _, id := range ids {
		if _, ok := statuses[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > fewJobs {
		// Many jobs missing from the list means the farm is wrong; report
		// them instead of reading each one with a GET.
		for _, id := range missing {
			errs[id] = fmt.Errorf("not in the job list of farm %q", farmID)
		}
		return statuses, errs
	}
	for _, id := range missing {
		j, err := v.GetJob(ctx, id)
		if err != nil {
			errs[id] = err
			continue
		}
		statuses[id] = j.Status
	}
	return statuses, errs
}

// listFarmJobs calls fn for every job of the farm, page by page.
func listFarmJobs(ctx context.Context, v ServerView, farmID string, fn func(client.Job)) error {
	err := client.EachPage(maxPage, func(limit, offset int) (client.Page[client.Job], error) {
		return v.ListJobs(ctx, farmID, limit, offset)
	}, func(p client.Page[client.Job]) {
		for _, j := range p.Items {
			fn(j)
		}
	})
	if err != nil {
		return fmt.Errorf("listing jobs of farm %q: %w", farmID, err)
	}
	return nil
}

// evidence collects violations, keeping the first MaxEvidence.
type evidence struct {
	lines []string
	n     int
}

func (e *evidence) add(s string) {
	e.n++
	if len(e.lines) < MaxEvidence {
		e.lines = append(e.lines, s)
	}
}

func (e *evidence) addAll(ss []string) {
	for _, s := range ss {
		if s != "" {
			e.add(s)
		}
	}
}

func (e *evidence) result(name string) Result {
	lines := e.lines
	if e.n > MaxEvidence {
		lines = append(lines[:MaxEvidence-1:MaxEvidence-1], fmt.Sprintf("... and %d more", e.n-(MaxEvidence-1)))
	}
	return Result{Name: name, OK: e.n == 0, Evidence: lines}
}

// sampled reports whether the task id is in the sample of share frac for
// seed. The choice depends only on seed, salt and id, so it does not depend
// on the order tasks are listed in, and different salts sample independently.
func sampled(seed uint64, salt, id string, frac float64) bool {
	if frac >= 1 {
		return true
	}
	if frac <= 0 {
		return false
	}
	h := fnv.New64a()
	h.Write([]byte(salt))
	h.Write([]byte{0})
	h.Write([]byte(id))
	x := splitmix64(h.Sum64() ^ splitmix64(seed))
	return float64(x>>11)/(1<<53) < frac
}

func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// parallelMap returns fn(i) for every i < n, running up to parallelism calls
// at once.
func parallelMap[T any](n int, fn func(i int) T) []T {
	out := make([]T, n)
	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup
	for i := range n {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			out[i] = fn(i)
		})
	}
	wg.Wait()
	return out
}

// pollInterval is how often to re-read something that has d to settle: a
// twentieth of d, between 10 ms and 1 s.
func pollInterval(d time.Duration) time.Duration {
	return min(max(d/20, 10*time.Millisecond), time.Second)
}

// settleLoop calls check on items and keeps re-checking the ones it reports
// (a non-empty message) until none is left or d has elapsed since the first
// pass ended, then returns the messages still reported, in the order of
// items. Whatever the first pass reports is re-checked at least once, so a
// first pass slower than d cannot use up the settle time.
func settleLoop[T any](ctx context.Context, d time.Duration, items []T, check func([]T) []string) []string {
	msgs := check(items)
	deadline := time.Now().Add(d)
	wait := settlePoll
	rechecked := false
	for {
		var keepItems []T
		var keepMsgs []string
		for i, m := range msgs {
			if m != "" {
				keepItems = append(keepItems, items[i])
				keepMsgs = append(keepMsgs, m)
			}
		}
		items, msgs = keepItems, keepMsgs
		if len(items) == 0 || ctx.Err() != nil || (rechecked && !time.Now().Before(deadline)) {
			return msgs
		}
		sleep.For(ctx, min(wait, time.Until(deadline)))
		wait = min(2*wait, settlePollMax)
		msgs = check(items)
		rechecked = true
	}
}

// clip shortens s for evidence.
func clip(s string) string {
	const n = 60
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
