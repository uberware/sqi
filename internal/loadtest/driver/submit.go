// SPDX-License-Identifier: AGPL-3.0-or-later

package driver

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/target"
)

const (
	// defaultConcurrency is how many submissions a burst class (total) keeps
	// in flight.
	defaultConcurrency = 32
	// maxPacedInFlight bounds the submissions of one paced class awaiting an
	// answer. A server that stops answering fills it, after which the pacer's
	// lag grows, which is the measurement.
	maxPacedInFlight = 256
)

// finishTimeout bounds how long a submission still unanswered when the run
// ends waits for its answer, so the ledger accounts for it. A submission in
// a live run has no time limit: creating a million-task job takes the server
// over a minute, and abandoning it would leave a job the ledger never saw. A
// variable so a test can shorten it.
var finishTimeout = 10 * time.Second

// Submitter submits the scenario's jobs to the server and records what the
// server answers. Every attempt lands in the ledger and in Metrics.StatusCounts:
// a rejection (422, 429, 503) or a transport failure is counted, never retried
// and never hidden.
//
// Each job class runs on its own: a class with a rate is paced open-loop, and
// a class with a total is submitted as fast as the server accepts, with a
// bounded number in flight, until total attempts have been made, rejected ones
// included. A class's weight does not pick between classes, because every
// class already has its own arrival process; it is recorded in the ledger only.
type Submitter struct {
	c      *client.Client
	t      target.Target
	sc     *scenario.Scenario
	ledger *JobLedger
	m      *Metrics
	rng    *syncRand

	prefix      string
	concurrency int
}

// SubmitterOption configures NewSubmitter.
type SubmitterOption func(*Submitter)

// WithNamePrefix puts prefix and a dash in front of every job name, so
// several submitters (shards) never collide: "<prefix>-<class>-<seq>".
func WithNamePrefix(prefix string) SubmitterOption {
	return func(s *Submitter) { s.prefix = prefix }
}

// WithConcurrency sets how many submissions a burst class keeps in flight
// (default 32).
func WithConcurrency(n int) SubmitterOption {
	return func(s *Submitter) {
		if n > 0 {
			s.concurrency = n
		}
	}
}

// NewSubmitter returns a Submitter for sc against t. r seeds every random
// choice (which earlier job a dependent job waits on); a nil r uses a fixed
// seed.
func NewSubmitter(c *client.Client, t target.Target, sc *scenario.Scenario, l *JobLedger, m *Metrics, r *rand.Rand, opts ...SubmitterOption) *Submitter {
	s := &Submitter{
		c: c, t: t, sc: sc, ledger: l, m: m,
		rng:         newSyncRand(r),
		concurrency: defaultConcurrency,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// classRun is one job class being submitted.
type classRun struct {
	jc      scenario.JobClass
	queueID string
	seq     atomic.Int64
}

// Run submits until ctx ends or, for a class with a total, until that many
// attempts are made. It waits for every submission already sent. A rejected
// submission is not an error; Run fails only when the harness cannot build a
// template or a class names a queue the target does not have.
func (s *Submitter) Run(ctx context.Context) error {
	queues := s.t.QueueIDs()
	runs := make([]*classRun, 0, len(s.sc.Jobs))
	for _, jc := range s.sc.Jobs {
		qid, ok := queues[jc.Queue]
		if !ok {
			return fmt.Errorf("driver: job class %q names queue %q, which the target does not have", jc.Name, jc.Queue)
		}
		if jc.Rate.PerSecond <= 0 && jc.Total <= 0 {
			return fmt.Errorf("driver: job class %q has neither a rate nor a total", jc.Name)
		}
		runs = append(runs, &classRun{jc: jc, queueID: qid})
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		failOnce sync.Once
		runErr   error
	)
	fail := func(err error) {
		failOnce.Do(func() {
			runErr = err
			cancel()
		})
	}

	var wg sync.WaitGroup
	for _, cr := range runs {
		wg.Go(func() {
			if cr.jc.Rate.PerSecond > 0 {
				s.runPaced(ctx, cr, fail)
			} else {
				s.runBurst(ctx, cr, fail)
			}
		})
	}
	wg.Wait()
	return runErr
}

// runPaced submits cr at its rate on an open-loop schedule. A submission that
// cannot leave on time leaves late, and the delay is recorded in SubmitLag;
// none is skipped.
func (s *Submitter) runPaced(ctx context.Context, cr *classRun, fail func(error)) {
	pacer := NewPacer(cr.jc.Rate.PerSecond, time.Now())
	sem := make(chan struct{}, maxPacedInFlight)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		due := pacer.Next()
		if !sleepUntil(ctx, due) {
			return
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		s.m.SubmitLag.Record(time.Since(due))
		wg.Go(func() {
			defer func() { <-sem }()
			if err := s.submit(ctx, cr); err != nil {
				fail(err)
			}
		})
	}
}

// runBurst submits cr as fast as the server accepts, with s.concurrency in
// flight, until Total attempts have been made.
func (s *Submitter) runBurst(ctx context.Context, cr *classRun, fail func(error)) {
	var attempted atomic.Int64
	total := int64(cr.jc.Total)
	var wg sync.WaitGroup
	for range s.concurrency {
		wg.Go(func() {
			for ctx.Err() == nil {
				if attempted.Add(1) > total {
					return
				}
				if err := s.submit(ctx, cr); err != nil {
					fail(err)
					return
				}
			}
		})
	}
	wg.Wait()
}

// submit makes one attempt and records it. The request outlives a canceled
// ctx for up to finishTimeout so the ledger accounts for a job the server may
// already hold. It errors only when the template cannot be built.
func (s *Submitter) submit(ctx context.Context, cr *classRun) error {
	name := jobName(s.prefix, cr.jc.Name, cr.seq.Add(1))
	tmpl, err := BuildTemplate(cr.jc, name, s.t.NoopCommand())
	if err != nil {
		return fmt.Errorf("driver: build template for %s: %w", name, err)
	}
	req := client.SubmitRequest{FarmID: s.t.FarmID(), QueueID: cr.queueID, Template: tmpl}
	if dep := s.pickDependency(); dep != "" {
		req.DependsOn = []string{dep}
	}

	rctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	// While ctx is live the request is bounded only by the server; once it
	// ends, the request gets finishTimeout more.
	stop := context.AfterFunc(ctx, func() {
		timer := time.NewTimer(finishTimeout)
		defer timer.Stop()
		select {
		case <-timer.C:
			cancel()
		case <-rctx.Done():
		}
	})
	defer stop()
	sent := time.Now()
	id, status, err := s.c.SubmitJob(rctx, req)
	answered := time.Now()

	if err != nil && status >= http.StatusOK && status < http.StatusMultipleChoices {
		status = StatusUndecodable
	}
	s.m.SubmitLatency.Record(answered.Sub(sent))
	s.m.Status(status).Add(1)
	if status == http.StatusTooManyRequests {
		s.m.RateLimited.Store(true)
	}
	job := SubmittedJob{Name: name, Class: cr.jc.Name, Weight: cr.jc.Weight, SubmittedAt: sent, Status: status}
	if err == nil {
		job.ID = id
		job.AcceptedAt = answered
	}
	s.ledger.Record(job)
	return nil
}

// pickDependency returns the id of a random earlier accepted job with
// probability job_deps, and "" otherwise or when none has been accepted yet.
func (s *Submitter) pickDependency() string {
	if s.sc.JobDeps <= 0 || s.rng.Float64() >= s.sc.JobDeps {
		return ""
	}
	j, ok := s.ledger.Pick(s.rng)
	if !ok {
		return ""
	}
	return j.ID
}

// jobName is "<class>-<seq>", or "<prefix>-<class>-<seq>" with a prefix.
func jobName(prefix, class string, seq int64) string {
	name := class + "-" + strconv.FormatInt(seq, 10)
	if prefix != "" {
		return prefix + "-" + name
	}
	return name
}
