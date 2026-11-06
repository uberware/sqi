// SPDX-License-Identifier: AGPL-3.0-or-later

// Package simworker is the load harness's simulated worker: a process-light
// stand-in for sqi-worker that speaks the real worker protocol through the
// real worker packages (lease, heartbeat, status, logstreamer, cancel), so a
// protocol change breaks its build rather than skewing its numbers. Only the
// executor is replaced: a [Dispatcher] plays each assignment out from a
// sampled [TaskPlan] instead of starting a process.
package simworker

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/worker/cancel"
	"github.com/uberware/sqi/internal/worker/logstreamer"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/status"
)

// minLineTick is the shortest wait between two rounds of log lines. A plan
// with lines closer together than this emits them in batches, so a heavy log
// plan costs one timer per tick rather than one per line. It is well under the
// log streamer's 500 ms flush interval, so batching adds little delay before
// lines reach the server.
const minLineTick = 100 * time.Millisecond

// simulatedFailure is the failure reason of a task whose plan fails.
const simulatedFailure = "simulated failure"

// timeoutExitCode is the exit code reported for a task killed at its timeout.
const timeoutExitCode = -1

// errClosed is returned by Dispatch once the dispatcher has been shut down.
var errClosed = errors.New("simworker: dispatcher is shut down")

// endReason is why a simulated task stopped.
type endReason int

const (
	endNone     endReason = iota // still running
	endDone                      // the plan ran out; the task is finishing
	endCanceled                  // a cancel signal arrived
	endShutdown                  // a graceful stop gave up waiting for it
	endCrashed                   // the worker crashed or restarted
	endTimedOut                  // the action's timeout elapsed
)

// simTask is one dispatched attempt.
type simTask struct {
	m    *protocol.AssignMsg
	plan TaskPlan
	// stop is closed when reason is first set to anything but endDone.
	stop chan struct{}
	// reason is guarded by Dispatcher.mu. Whoever moves it off endNone
	// decides how the task ends: the task itself (endDone), Cancel, or the
	// worker shutting the dispatcher down. A timeout is another claimant: the
	// timer set by run moves it to endTimedOut the same way Cancel does.
	reason endReason
}

// Dispatcher plays assignments out according to a [Behavior]. It implements
// lease.Dispatcher and the lease loop's Hold, heartbeat.StateSource and
// heartbeat.AttemptSource, and cancel.TaskCanceler.
//
// Lock order: Dispatcher.mu is never held while publishing, while calling the
// ledger or the cancel handler, or while sampling; rngMu is never held with mu.
type Dispatcher struct {
	workerID   string
	instanceID string
	st         *status.Publisher
	logs       *logstreamer.Publisher
	cancels    *cancel.Handler
	behavior   Behavior
	ledger     *Ledger
	logger     *slog.Logger // the worker sets its own; discard otherwise

	// rngMu guards rng, which a Worker shares with every dispatcher it builds.
	// A per-dispatcher mutex suffices because only Dispatch samples, and a
	// worker builds its next dispatcher only after the previous one's lease
	// loop has returned, so two dispatchers never sample at once.
	rngMu sync.Mutex
	rng   *rand.Rand

	mu         sync.Mutex
	held       map[string]struct{} // attempt IDs, from Hold until release
	active     map[string]*simTask // attempt ID -> dispatched attempt; a re-leased task can have two
	lastAssign *time.Time
	closed     bool

	wg sync.WaitGroup // task goroutines
}

// NewDispatcher returns a dispatcher publishing as workerID. cancels may be
// nil, in which case cancel signals are not subscribed (unit tests). r seeds
// every plan this dispatcher samples.
func NewDispatcher(
	workerID string,
	st *status.Publisher,
	logs *logstreamer.Publisher,
	cancels *cancel.Handler,
	b Behavior,
	l *Ledger,
	r *rand.Rand,
) *Dispatcher {
	return &Dispatcher{
		workerID: workerID,
		st:       st,
		logs:     logs,
		cancels:  cancels,
		behavior: b,
		ledger:   l,
		rng:      r,
		logger:   slog.New(slog.DiscardHandler),
		held:     map[string]struct{}{},
		active:   map[string]*simTask{},
	}
}

// Hold reports attemptID as held from now until its terminal status has been
// published. The lease loop calls it for a whole batch before dispatching any.
func (d *Dispatcher) Hold(attemptID string) {
	d.mu.Lock()
	d.held[attemptID] = struct{}{}
	d.mu.Unlock()
}

// Dispatch samples a plan for m and starts playing it out. It returns at once.
func (d *Dispatcher) Dispatch(_ context.Context, m *protocol.AssignMsg) error {
	d.rngMu.Lock()
	plan := d.behavior.Plan(m, d.rng)
	d.rngMu.Unlock()

	t := &simTask{m: m, plan: plan, stop: make(chan struct{})}
	now := time.Now()
	d.mu.Lock()
	if d.closed {
		delete(d.held, m.AttemptID)
		d.mu.Unlock()
		d.record(m, KindLeased, "", 0, now)
		d.record(m, KindCrashed, "", 0, now)
		return errClosed
	}
	d.held[m.AttemptID] = struct{}{}
	d.active[m.AttemptID] = t
	d.lastAssign = &now
	d.wg.Add(1)
	d.mu.Unlock()

	d.record(m, KindLeased, "", 0, time.Time{})
	go d.run(t) //nolint:gosec // G118: a task outlives the lease request that delivered it; shutdown stops it, not the lease context
	return nil
}

// Cancel stops taskID's attempts and reports whether one was running and is
// now being canceled (true again for a repeat cancel). A non-empty attemptID
// limits it to that attempt. A task already finishing, abandoned by a
// shutdown, or running only another attempt reports false.
func (d *Dispatcher) Cancel(taskID, attemptID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	canceled := false
	for _, t := range d.active {
		if t.m.TaskID != taskID || (attemptID != "" && t.m.AttemptID != attemptID) {
			continue
		}
		if t.reason == endNone {
			t.reason = endCanceled
			close(t.stop)
		}
		canceled = canceled || t.reason == endCanceled
	}
	return canceled
}

// ActiveTaskCount returns how many attempts are dispatched and not yet ended.
func (d *Dispatcher) ActiveTaskCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.active)
}

// ActiveTaskIDs returns the task IDs of the dispatched attempts, each once.
func (d *Dispatcher) ActiveTaskIDs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	ids := make([]string, 0, len(d.active))
	for _, t := range d.active {
		if !slices.Contains(ids, t.m.TaskID) {
			ids = append(ids, t.m.TaskID)
		}
	}
	return ids
}

// ActiveAttemptIDs returns the held attempts, sorted.
func (d *Dispatcher) ActiveAttemptIDs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Sorted(maps.Keys(d.held))
}

// LastAssignmentAt returns when the last assignment was dispatched, or nil.
func (d *Dispatcher) LastAssignmentAt() *time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastAssign
}

// run plays one task out. It owns the task until it returns.
func (d *Dispatcher) run(t *simTask) {
	defer d.wg.Done()
	m := t.m
	if d.cancels != nil {
		if err := d.cancels.Register(m.TaskID, m.AttemptID); err != nil {
			d.logger.DebugContext(context.Background(), "simworker: cancel subscription failed", slog.String("task_id", m.TaskID), slog.Any("error", err))
		}
		defer d.cancels.Deregister(m.TaskID, m.AttemptID)
	}
	d.st.Running(context.Background(), m, "", nil, time.Now())
	d.record(m, KindRunning, "", 0, time.Time{})
	if timeout := timeoutOf(m); timeout > 0 {
		timer := time.AfterFunc(timeout, func() { d.expire(t) })
		defer timer.Stop()
	}

	lines := d.emit(t)
	d.finish(t, d.settle(t), lines)
}

// timeoutOf is m's onRun timeout, or zero for none.
func timeoutOf(m *protocol.AssignMsg) time.Duration {
	if m.OnRun == nil || m.OnRun.TimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(m.OnRun.TimeoutSeconds) * time.Second
}

// expire ends t as timed out unless a cancel, a shutdown or the plan's own end
// got there first.
func (d *Dispatcher) expire(t *simTask) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t.reason == endNone {
		t.reason = endTimedOut
		close(t.stop)
	}
}

// emit writes the plan's lines evenly over its duration and returns how many
// it wrote. It returns when the duration is over (a hang keeps waiting) or the
// task is stopped.
func (d *Dispatcher) emit(t *simTask) int {
	p := t.plan
	start := time.Now()
	emitted := 0
	timer := time.NewTimer(0)
	timer.Stop()
	defer timer.Stop()
	for {
		elapsed := time.Since(start)
		for emitted < linesDue(p, elapsed) {
			emitted++
			d.logs.HandleLine(context.Background(), t.m.TaskID, t.m.AttemptID, "", "stdout", LogLine(t.m.TaskID, emitted, p.LineBytes))
		}
		var wait <-chan time.Time
		switch {
		case emitted < p.LogLines:
			// The tick never carries the task past its duration, where the
			// last line is due.
			next := lineTime(p, emitted+1)
			timer.Reset(min(max(next-elapsed, minLineTick), p.Duration-elapsed))
			wait = timer.C
		case elapsed < p.Duration:
			timer.Reset(p.Duration - elapsed)
			wait = timer.C
		case p.Outcome != OutcomeHang:
			return emitted
		}
		// A nil wait (a hang with nothing left to do) blocks until stopped.
		select {
		case <-wait:
		case <-t.stop:
			return emitted
		}
	}
}

// lineTime is when line n (from 1) of p is due: n evenly spaced lines end at
// the duration.
func lineTime(p TaskPlan, n int) time.Duration {
	return time.Duration(float64(p.Duration) * float64(n) / float64(p.LogLines))
}

// linesDue is how many of p's lines are due after elapsed.
func linesDue(p TaskPlan, elapsed time.Duration) int {
	if p.LogLines <= 0 {
		return 0
	}
	if p.Duration <= 0 || elapsed >= p.Duration {
		return p.LogLines
	}
	return min(p.LogLines, int(float64(p.LogLines)*float64(elapsed)/float64(p.Duration)))
}

// settle decides how t ends. A task whose plan ran out claims endDone unless a
// cancel or a shutdown got there first.
func (d *Dispatcher) settle(t *simTask) endReason {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t.reason == endNone {
		t.reason = endDone
	}
	return t.reason
}

// finish publishes how t ended and only then releases its attempt: a heartbeat
// that stopped reporting the attempt before its terminal status landed would
// invite the server to reclaim it.
func (d *Dispatcher) finish(t *simTask, reason endReason, lines int) {
	m := t.m
	// FlushLogs also stops the log publisher's per-attempt goroutine, so it
	// runs on every path, a crash included.
	if err := d.logs.FlushLogs(context.Background(), m.TaskID, m.AttemptID); err != nil {
		d.logger.DebugContext(context.Background(), "simworker: flush logs failed", slog.String("task_id", m.TaskID), slog.Any("error", err))
	}
	ctx := context.Background()
	// The end event is stamped before the publish: once the server has the
	// status it may lease the freed slot to another worker, whose leased event
	// must not sort before this attempt's end.
	now := time.Now()
	switch reason {
	case endDone:
		outcome, code, msg := OutcomeSucceeded, 0, ""
		if t.plan.Outcome == OutcomeFailed {
			outcome, code, msg = OutcomeFailed, 1, simulatedFailure
		}
		d.st.Terminal(ctx, m, "", outcome, &code, msg, nil, now)
		d.record(m, KindTerminal, outcome, lines, now)
	case endCanceled:
		d.st.Terminal(ctx, m, "", "canceled", nil, "", nil, now)
		d.record(m, KindCanceled, "", lines, now)
	case endTimedOut:
		code := timeoutExitCode
		msg := protocol.TimeoutReason(timeoutOf(m))
		d.st.Terminal(ctx, m, "", OutcomeFailed, &code, msg, nil, now)
		d.record(m, KindTerminal, OutcomeFailed, lines, now)
	case endShutdown:
		d.st.Terminal(ctx, m, "", "failed", nil, protocol.MessageWorkerShutdown, nil, now)
		d.record(m, KindTerminal, protocol.MessageWorkerShutdown, lines, now)
	default: // endCrashed: nothing is published
		d.record(m, KindCrashed, "", lines, now)
	}
	d.release(t)
}

// release drops t's attempt from the held set and t from the active set.
func (d *Dispatcher) release(t *simTask) {
	d.mu.Lock()
	delete(d.held, t.m.AttemptID)
	if d.active[t.m.AttemptID] == t {
		delete(d.active, t.m.AttemptID)
	}
	d.mu.Unlock()
}

// record adds an event for m to the ledger. A zero at is stamped with the
// current time.
func (d *Dispatcher) record(m *protocol.AssignMsg, kind, outcome string, lines int, at time.Time) {
	d.ledger.Record(Event{
		At:       at,
		WorkerID: d.workerID, InstanceID: d.instanceID,
		TaskID: m.TaskID, AttemptID: m.AttemptID, JobID: m.JobID,
		Kind: kind, Outcome: outcome, LogLines: lines,
	})
}

// shutdown refuses further dispatches and ends every task that is not already
// finishing with reason (endShutdown or endCrashed). It does not wait.
func (d *Dispatcher) shutdown(reason endReason) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	for _, t := range d.active {
		if t.reason == endNone {
			t.reason = reason
			close(t.stop)
		}
	}
}

// wait blocks until every task goroutine has returned or ctx is done, and
// reports whether they all returned. Call it only once no Dispatch can start.
func (d *Dispatcher) wait(ctx context.Context) bool {
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}
