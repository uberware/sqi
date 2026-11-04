// SPDX-License-Identifier: AGPL-3.0-or-later

// Package verify decides whether a load run passed. It compares what the
// driver submitted and what the simulated workers did (the [simworker.Ledger])
// with what the server reports, through the six run invariants of the load
// harness: every job terminal, every task's status the one the simulator
// decided, one live attempt per task at a time, the logs the simulator sent,
// the online worker count, and the queue and usage-pool caps.
//
// Facts about the server the checks rely on:
//
//   - The attempt ID in an assignment, and so in every ledger event, is the
//     server's attempt row ID. The server's log chunks carry it as
//     [client.LogLine.AttemptID]; GET /tasks/{id}/attempts does not expose it,
//     only the attempt number, so attempts are matched to the ledger by order.
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
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/driver"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
)

// Invariant names, in the order [Check] returns them.
const (
	NameTerminal        = "terminal"
	NameOutcomes        = "outcomes"
	NameSingleAttempt   = "single-attempt"
	NameLogs            = "logs"
	NameWorkersConverge = "workers-converge"
	NameCaps            = "caps"
)

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
	// doubles after each re-read up to settlePollMax, so a disagreement that
	// is real costs a handful of re-reads rather than one every 100 ms.
	settlePoll    = 100 * time.Millisecond
	settlePollMax = time.Second
)

// retryWaits are the pauses before each try of a server call; a call is tried
// once per entry. Only a transport error, a 429 or a 5xx is retried.
var retryWaits = []time.Duration{0, 100 * time.Millisecond, 400 * time.Millisecond}

// Server statuses.
const (
	jobCompleted = "completed"
	statusOK     = "succeeded"
	statusFailed = "failed"
	statusCancel = "canceled"
	workerOnline = "online"
	workerOff    = "disabled"
	saltAttempts = "attempts"
	saltLogs     = "logs"
)

// terminalJob holds the job statuses a job cannot leave on its own.
var terminalJob = map[string]bool{jobCompleted: true, statusFailed: true, statusCancel: true}

// ServerView is the part of the server's REST API the checks read.
// *client.Client satisfies it.
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
	FarmID   string
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
	// CanceledJobs are the jobs the run's actions canceled ([CanceledJobs]).
	// A task of one of them may be canceled whatever the simulator did.
	CanceledJobs []string
	// CanceledAt is when the first cancel action targeting each job started
	// ([CancelTimes]). Its jobs count as canceled too, and caps ends a
	// canceled attempt of one of them no later than that time, when the
	// server may already have freed its slot.
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
}

// Result is one invariant's verdict. Evidence names the job, task, attempt,
// queue or pool of each violation with the observed and expected values; it
// is empty when OK.
type Result struct {
	Name     string
	OK       bool
	Evidence []string
}

// Check evaluates the six run invariants and returns their results in a fixed
// order: terminal, outcomes, single-attempt, logs, workers-converge, caps.
// Call it once [WaitDrained] has returned nil and the fleet's churn has
// stopped, with the fleet still running.
//
// Its server calls: the accepted jobs' statuses (one GET each up to 20 jobs,
// otherwise the farm's job list, 1000 a page), every job's task list (1000 a
// page), one attempt list for each task sampled at AttemptSample, one log
// read for each task sampled at LogSample (more for a task over 1000 chunks),
// and the farm's worker list once per poll.
func Check(ctx context.Context, v ServerView, in Input) []Result {
	c := newChecker(v, in)
	workers := make(chan Result, 1)
	go func() { workers <- c.workersConverge(ctx) }()

	statuses, errs := jobStatuses(ctx, v, in.FarmID, c.order)
	sweep := c.sweepTasks(ctx, statuses)
	return []Result{
		c.terminal(statuses, errs),
		c.outcomes(ctx, sweep),
		c.singleAttempt(ctx, sweep.attempts),
		c.logs(ctx, sweep.logs),
		<-workers,
		c.caps(),
	}
}

// checker holds what every check looks up.
type checker struct {
	v        ServerView
	in       Input
	jobs     map[string]driver.SubmittedJob // accepted, by ID
	order    []string                       // accepted job IDs, in submission order
	canceled map[string]bool
	classes  map[string]scenario.JobClass // by name
	settle   time.Duration
	// logSettle is the logs check's settle time.
	logSettle time.Duration
}

func newChecker(v ServerView, in Input) *checker {
	c := &checker{
		v: v, in: in,
		jobs:      map[string]driver.SubmittedJob{},
		canceled:  map[string]bool{},
		classes:   map[string]scenario.JobClass{},
		settle:    cmp.Or(in.Settle, DefaultSettle),
		logSettle: cmp.Or(in.LogSettle, DefaultLogSettle),
	}
	c.order = acceptedIDs(in.Jobs)
	for _, j := range in.Jobs {
		if j.Accepted() {
			c.jobs[j.ID] = j
		}
	}
	for _, id := range in.CanceledJobs {
		c.canceled[id] = true
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

// classOf returns the scenario class of an accepted job.
func (c *checker) classOf(jobID string) (scenario.JobClass, bool) {
	j, ok := c.jobs[jobID]
	if !ok {
		return scenario.JobClass{}, false
	}
	jc, ok := c.classes[j.Class]
	return jc, ok
}

// WaitDrained polls the accepted jobs until every one is terminal (completed,
// failed or canceled) and returns nil, or returns an error naming up to 20 of
// the jobs still open, with their statuses, once timeout has elapsed or ctx
// has ended. Up to 20 jobs are read one GET at a time; more are read from the
// farm's job list, 1000 a page.
func WaitDrained(ctx context.Context, v ServerView, farmID string, jobs []driver.SubmittedJob, timeout time.Duration) error {
	pending := acceptedIDs(jobs)
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
			case !terminalJob[statuses[id]]:
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
		sleepCtx(ctx, min(poll, time.Until(deadline)))
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

// CanceledJobs returns the jobs the run's cancel_jobs actions targeted, each
// once, for Input.CanceledJobs.
func CanceledJobs(records []driver.ActionRecord) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range records {
		if r.Kind != driver.ActionCancelJobs {
			continue
		}
		for _, id := range r.JobIDs {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
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
		// Not one GET each for a listing that lacks many: the farm is wrong.
		for _, id := range missing {
			errs[id] = fmt.Errorf("not in the job list of farm %q", farmID)
		}
		return statuses, errs
	}
	for _, id := range missing {
		var j client.Job
		err := withRetry(ctx, func() error {
			var err error
			j, err = v.GetJob(ctx, id)
			return err
		})
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
	for offset := 0; ; {
		var p client.Page[client.Job]
		err := withRetry(ctx, func() error {
			var err error
			p, err = v.ListJobs(ctx, farmID, maxPage, offset)
			return err
		})
		if err != nil {
			return fmt.Errorf("listing jobs of farm %q: %w", farmID, err)
		}
		for _, j := range p.Items {
			fn(j)
		}
		offset += len(p.Items)
		if len(p.Items) == 0 || offset >= p.Total {
			return nil
		}
	}
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

// withRetry calls fn up to len(retryWaits) times while it fails with a
// transport error, a 429 or a 5xx and ctx is live.
func withRetry(ctx context.Context, fn func() error) error {
	var err error
	for i, wait := range retryWaits {
		if i > 0 && !sleepCtx(ctx, wait) {
			return err
		}
		if err = fn(); err == nil || !retryable(ctx, err) {
			return err
		}
	}
	return err
}

func retryable(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if ae, ok := errors.AsType[*client.APIError](err); ok {
		return ae.Status == 429 || ae.Status >= 500
	}
	return true
}

// pollInterval is how often to re-read something that has d to settle: a
// twentieth of d, between 10 ms and 1 s.
func pollInterval(d time.Duration) time.Duration {
	return min(max(d/20, 10*time.Millisecond), time.Second)
}

// sleepCtx sleeps for d and reports whether ctx is still live.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
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
		sleepCtx(ctx, min(wait, time.Until(deadline)))
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
