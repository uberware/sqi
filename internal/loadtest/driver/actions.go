// SPDX-License-Identifier: AGPL-3.0-or-later

package driver

import (
	"context"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
	"github.com/uberware/sqi/internal/loadtest/target"
)

const (
	// actionParallelism bounds the REST calls one action keeps in flight.
	actionParallelism = 16
	// actionPageSize is the page size when an action lists jobs or workers.
	actionPageSize = 200
	// fractionEpsilon keeps floor(fraction x n) from losing one to binary
	// rounding (0.29 x 100 is 28.999999999999996).
	fractionEpsilon = 1e-9
	// jobStatusFailed is the server's status of a failed job.
	jobStatusFailed = "failed"
)

// ActionCancelJobs is the [ActionRecord.Kind] of a cancel_jobs action.
const ActionCancelJobs = "cancel_jobs"

// ActionRecord is what one step of a timed action did.
type ActionRecord struct {
	// Kind is cancel_jobs, retry_failed, pause_queue, resume_queue or
	// disable_workers.
	Kind string
	// At is the action's offset in the scenario; Started is when it began.
	At      time.Duration
	Started time.Time
	// Targeted is how many REST calls it set out to make, Succeeded how many
	// the server accepted, and Failed how many it refused or that errored.
	Targeted, Succeeded, Failed int
	// Detail names the queue, or the first error.
	Detail string
	// JobIDs are the jobs a cancel_jobs action set out to cancel, whether or
	// not the server accepted the call: one that errored may still have landed.
	JobIDs []string
}

// Actions fires the scenario's timed events at their offsets from the run's
// start: it cancels a fraction of the jobs submitted so far, retries failed
// jobs, pauses or resumes a queue, disables a fraction of the workers. Each
// step is recorded; a call the server refuses is counted in the record, not
// fatal. A queue left paused is resumed by ResumePaused.
type Actions struct {
	c     *client.Client
	t     target.Target
	acts  []scenario.Action
	jobs  *JobLedger
	fleet *simworker.Fleet
	rng   *syncRand

	mu      sync.Mutex
	records []ActionRecord
	// paused are the queues a pause_queue action targeted and no later
	// resume_queue resumed, by name.
	paused map[string]bool
}

// ActionsOption configures NewActions.
type ActionsOption func(*Actions)

// WithActionsRand seeds the random choice of which jobs and workers an action
// hits; without it a fixed seed is used.
func WithActionsRand(r *rand.Rand) ActionsOption {
	return func(a *Actions) { a.rng = newSyncRand(r) }
}

// NewActions returns Actions for acts against t. fleet may be nil; when set,
// disable_workers hits only workers the fleet runs.
func NewActions(c *client.Client, t target.Target, acts []scenario.Action, jobs *JobLedger, fleet *simworker.Fleet, opts ...ActionsOption) *Actions {
	a := &Actions{c: c, t: t, acts: acts, jobs: jobs, fleet: fleet, rng: newSyncRand(nil)}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Records returns what the actions did so far, in the order they finished.
func (a *Actions) Records() []ActionRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.records)
}

func (a *Actions) record(r ActionRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.records = append(a.records, r)
}

// Run fires each action at start + its At, and returns once ctx has ended or
// every action has run. It fails up front for a pause_queue naming a queue the
// target does not have.
func (a *Actions) Run(ctx context.Context, start time.Time) error {
	for _, act := range a.acts {
		for _, q := range []string{act.PauseQueue, act.ResumeQueue} {
			if _, ok := a.t.QueueIDs()[q]; q != "" && !ok {
				return fmt.Errorf("driver: action at %s names queue %q, which the target does not have", act.At.Duration, q)
			}
		}
	}
	// Each action gets its own child generator, drawn in order.
	rngs := make([]*rand.Rand, len(a.acts))
	for i := range a.acts {
		rngs[i] = a.rng.child()
	}
	var wg sync.WaitGroup
	for i, act := range a.acts {
		wg.Go(func() {
			if sleepUntil(ctx, start.Add(act.At.Duration)) {
				a.fire(ctx, act, rngs[i])
			}
		})
	}
	wg.Wait()
	return nil
}

// fire does everything act asks for, in a fixed order.
func (a *Actions) fire(ctx context.Context, act scenario.Action, rng *rand.Rand) {
	if act.CancelJobs != nil {
		a.cancelJobs(ctx, act, rng)
	}
	if act.RetryFailed {
		a.retryFailed(ctx, act)
	}
	if act.PauseQueue != "" {
		a.pauseQueue(ctx, act)
	}
	if act.ResumeQueue != "" {
		a.resumeQueue(ctx, act, act.ResumeQueue, "")
	}
	if act.DisableWorkers != nil {
		a.disableWorkers(ctx, act, rng)
	}
}

// share is floor(fraction x n), robust against binary rounding.
func share(fraction float64, n int) int {
	return min(n, max(0, int(math.Floor(fraction*float64(n)+fractionEpsilon))))
}

// sample returns k distinct elements of ids chosen uniformly.
func sample(ids []string, k int, rng *rand.Rand) []string {
	perm := rng.Perm(len(ids))
	out := make([]string, k)
	for i := range k {
		out[i] = ids[perm[i]]
	}
	return out
}

func (a *Actions) cancelJobs(ctx context.Context, act scenario.Action, rng *rand.Rand) {
	accepted := a.jobs.Accepted()
	ids := make([]string, len(accepted))
	for i, j := range accepted {
		ids[i] = j.ID
	}
	chosen := sample(ids, share(act.CancelJobs.Fraction, len(ids)), rng)
	rec := apply(ctx, ActionCancelJobs, act, chosen, a.c.CancelJob)
	rec.JobIDs = chosen
	rec.Detail = joinDetail(rec.Detail, fmt.Sprintf("fraction %g of %d accepted jobs", act.CancelJobs.Fraction, len(ids)))
	a.record(rec)
}

func (a *Actions) retryFailed(ctx context.Context, act scenario.Action) {
	statuses, err := a.jobStatuses(ctx)
	var failed []string
	for _, j := range a.jobs.Accepted() {
		if statuses[j.ID] == jobStatusFailed {
			failed = append(failed, j.ID)
		}
	}
	rec := apply(ctx, "retry_failed", act, failed, a.c.RetryJob)
	if err != nil {
		rec.Detail = joinDetail(rec.Detail, "listing jobs: "+err.Error())
	}
	a.record(rec)
}

// jobStatuses returns the status of every job of the run's farm. On a listing
// error it returns what it had read so far with the error.
func (a *Actions) jobStatuses(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	for offset := 0; ; offset += actionPageSize {
		page, err := a.c.ListJobs(ctx, a.t.FarmID(), actionPageSize, offset)
		if err != nil {
			return out, err
		}
		for _, j := range page.Items {
			out[j.ID] = j.Status
		}
		if len(page.Items) < actionPageSize || offset+len(page.Items) >= page.Total {
			return out, nil
		}
	}
}

func (a *Actions) pauseQueue(ctx context.Context, act scenario.Action) {
	id := a.t.QueueIDs()[act.PauseQueue]
	// Marked before the call: one that errors may still have landed.
	a.setPaused(act.PauseQueue, true)
	rec := apply(ctx, "pause_queue", act, []string{id}, func(ctx context.Context, id string) error {
		return a.c.PauseQueue(ctx, id, true)
	})
	rec.Detail = joinDetail(rec.Detail, "queue "+act.PauseQueue)
	a.record(rec)
}

// resumeQueue resumes queue name and records it, with note added to the
// record's detail.
func (a *Actions) resumeQueue(ctx context.Context, act scenario.Action, name, note string) {
	id := a.t.QueueIDs()[name]
	rec := apply(ctx, "resume_queue", act, []string{id}, func(ctx context.Context, id string) error {
		return a.c.PauseQueue(ctx, id, false)
	})
	if rec.Failed == 0 && rec.Succeeded == 1 {
		a.setPaused(name, false)
	}
	rec.Detail = joinDetail(joinDetail(rec.Detail, "queue "+name), note)
	a.record(rec)
}

func (a *Actions) setPaused(name string, paused bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.paused == nil {
		a.paused = map[string]bool{}
	}
	if paused {
		a.paused[name] = true
	} else {
		delete(a.paused, name)
	}
}

// ResumePaused resumes every queue a pause_queue action paused and no later
// resume_queue action resumed, and returns their names. The runner calls it
// once submissions stop, before it waits for the jobs to finish: a paused
// queue leases nothing, so its jobs would otherwise sit until the drain
// timeout and read as stranded by the server.
func (a *Actions) ResumePaused(ctx context.Context) []string {
	a.mu.Lock()
	names := slices.Sorted(maps.Keys(a.paused))
	a.mu.Unlock()
	for _, name := range names {
		a.resumeQueue(ctx, scenario.Action{}, name, "automatic, before the drain")
	}
	return names
}

func (a *Actions) disableWorkers(ctx context.Context, act scenario.Action, rng *rand.Rand) {
	ids, err := a.workerIDs(ctx)
	chosen := sample(ids, share(act.DisableWorkers.Fraction, len(ids)), rng)
	rec := apply(ctx, "disable_workers", act, chosen, func(ctx context.Context, id string) error {
		return a.c.SetWorkerDisabled(ctx, id, true)
	})
	rec.Detail = joinDetail(rec.Detail, fmt.Sprintf("fraction %g of %d workers", act.DisableWorkers.Fraction, len(ids)))
	if err != nil {
		rec.Detail = joinDetail(rec.Detail, "listing workers: "+err.Error())
	}
	a.record(rec)
}

// workerIDs returns the ids of the workers the server knows in the run's
// farm, restricted to the fleet's when there is one. Sorted, so a seeded
// choice does not depend on the server's listing order.
func (a *Actions) workerIDs(ctx context.Context) ([]string, error) {
	var ids []string
	var listErr error
	for offset := 0; ; offset += actionPageSize {
		page, err := a.c.ListWorkers(ctx, a.t.FarmID(), actionPageSize, offset)
		if err != nil {
			listErr = err
			break
		}
		for _, w := range page.Items {
			ids = append(ids, w.ID)
		}
		if len(page.Items) < actionPageSize || offset+len(page.Items) >= page.Total {
			break
		}
	}
	if a.fleet != nil {
		mine := map[string]bool{}
		for _, s := range a.fleet.Specs() {
			mine[s.ID] = true
		}
		ids = slices.DeleteFunc(ids, func(id string) bool { return !mine[id] })
	}
	slices.Sort(ids)
	return ids, listErr
}

// apply calls fn on every id, actionParallelism at a time, and returns the
// tally. It stops starting calls once ctx ends; those never started count as
// neither succeeded nor failed.
func apply(ctx context.Context, kind string, act scenario.Action, ids []string, fn func(context.Context, string) error) ActionRecord {
	rec := ActionRecord{Kind: kind, At: act.At.Duration, Started: time.Now(), Targeted: len(ids)}
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	sem := make(chan struct{}, actionParallelism)
loop:
	for _, id := range ids {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break loop
		}
		wg.Go(func() {
			defer func() { <-sem }()
			err := fn(ctx, id)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				rec.Failed++
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			rec.Succeeded++
		})
	}
	wg.Wait()
	if firstErr != nil {
		rec.Detail = "first error: " + firstErr.Error()
	}
	return rec
}

func joinDetail(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}
