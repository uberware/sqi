// SPDX-License-Identifier: AGPL-3.0-or-later

// Package chaos drives a load run's faults: it fires each of the scenario's
// faults at its time against the managed target and the simulated fleet,
// heals it, and records when it did both.
package chaos

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uberware/sqi/internal/loadtest/internal/sleep"
	"github.com/uberware/sqi/internal/loadtest/report"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/target"
	"github.com/uberware/sqi/internal/loadtest/verify"
)

// goos is the operating system the stall check is made for; tests change it.
var goos = runtime.GOOS

// Fleet is the simulated fleet a power cut takes down with the real processes.
// *simworker.Fleet implements it.
type Fleet interface {
	CrashAll()
	RestartAll(ctx context.Context) error
}

// procKey names a process the controller can stall: the server, or a real
// worker by index.
type procKey struct {
	server bool
	worker int
}

// window is what Windows needs of a fired fault.
type window struct {
	on     bool
	groups []string
}

// Controller fires a scenario's faults and heals them.
type Controller struct {
	faults []scenario.Fault
	groups []string // distinct worker group names, in scenario order
	t      target.Faults
	fleet  Fleet
	logger *slog.Logger
	// lanes holds a laneCtl for each group with a lane, laneNames their names
	// in scenario order.
	lanes     map[string]*laneCtl
	laneNames []string
	// tokens orders the lane faults by start.
	tokens atomic.Uint64

	mu sync.Mutex
	// closed is set by HealAll: no fault fires, and nothing is cut or
	// stalled, after it.
	closed  bool
	records []report.FaultRecord // one per fault; Start is zero until it fires
	windows []window
	// stalled counts the active stalls of each process.
	stalled map[procKey]int
	// procLocks serialize the stalls and resumes of one process, so HealAll
	// cannot resume a process before a stall in progress has stopped it.
	procLocks map[procKey]*sync.Mutex
}

// NewController checks the faults against the target and the operating
// system. t may be nil only when there are no faults; fleet may be nil when
// there is no simulated fleet.
func NewController(faults []scenario.Fault, groups []scenario.WorkerGroup, t target.Faults, fleet Fleet, logger *slog.Logger) (*Controller, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if len(faults) > 0 && t == nil {
		return nil, errors.New("chaos: faults need a target that can inject them")
	}
	c := &Controller{
		faults:    faults,
		t:         t,
		fleet:     fleet,
		logger:    logger,
		lanes:     map[string]*laneCtl{},
		records:   make([]report.FaultRecord, len(faults)),
		windows:   make([]window, len(faults)),
		stalled:   map[procKey]int{},
		procLocks: map[procKey]*sync.Mutex{},
	}
	for _, g := range groups {
		if !slices.Contains(c.groups, g.Name) {
			c.groups = append(c.groups, g.Name)
		}
	}
	if t != nil {
		for _, g := range c.groups {
			if l := t.Lane(g); l != nil {
				c.lanes[g] = newLaneCtl(l)
				c.laneNames = append(c.laneNames, g)
			}
		}
	}
	for i, f := range faults {
		if err := c.check(f); err != nil {
			return nil, fmt.Errorf("chaos: faults[%d]: %w", i, err)
		}
		c.records[i] = report.FaultRecord{Index: i, Kind: f.Kind()}
	}
	return c, nil
}

// check reports why the controller cannot inject f.
func (c *Controller) check(f scenario.Fault) error {
	switch f.Kind() {
	case "":
		return errors.New("sets no fault kind, or several")
	case scenario.FaultStall:
		if goos == "windows" {
			return errors.New("stall needs SIGSTOP, which Windows does not have")
		}
	}
	if spec, _, ok := netFault(f); ok {
		for _, g := range c.resolve(spec) {
			if c.lanes[g] == nil {
				return fmt.Errorf("worker group %s has no broker lane", g)
			}
		}
	}
	return nil
}

// netFault returns a network fault's groups as the scenario names them and
// the state it puts their lanes in. ok is false for any other kind.
func netFault(f scenario.Fault) (groups []string, s laneState, ok bool) {
	switch {
	case f.Partition != nil:
		return f.Partition.Groups, laneState{level: levelCut}, true
	case f.Blackhole != nil:
		return f.Blackhole.Groups, laneState{level: levelBlackhole}, true
	case f.Latency != nil:
		l := f.Latency
		return l.Groups, laneState{level: levelShape, delay: l.Delay.Duration, jitter: l.Jitter.Duration, bps: l.BytesPerSec}, true
	case f.Flap != nil:
		return f.Flap.Groups, laneState{level: levelCut}, true
	}
	return nil, laneState{}, false
}

// resolve turns a network fault's groups into the groups it hits: every group
// when it names none.
func (c *Controller) resolve(spec []string) []string {
	if len(spec) == 0 {
		return slices.Clone(c.groups)
	}
	return slices.Clone(spec)
}

// Run fires each fault at start+At, concurrently, and returns when every
// fault has healed or ctx is done. Errors are recorded per fault and joined.
// When ctx ends, each fault in progress heals what it changed on a lane and
// resumes what it stalled; a process it took down stays down. Run calls
// HealAll before it returns.
func (c *Controller) Run(ctx context.Context, start time.Time) error {
	defer c.HealAll()
	errs := make([]error, len(c.faults))
	var wg sync.WaitGroup
	for i := range c.faults {
		wg.Go(func() { errs[i] = c.runFault(ctx, start, i) })
	}
	wg.Wait()
	return errors.Join(errs...)
}

// runFault waits for fault i's time, fires it and records its error.
func (c *Controller) runFault(ctx context.Context, start time.Time, i int) error {
	f := c.faults[i]
	if !sleep.Until(ctx, start.Add(f.At.Duration)) || c.isClosed() {
		return nil
	}
	err := c.fire(ctx, i)
	if err == nil {
		return nil
	}
	err = fmt.Errorf("chaos: faults[%d] (%s): %w", i, f.Kind(), err)
	c.mu.Lock()
	c.records[i].Error = err.Error()
	c.mu.Unlock()
	c.logger.WarnContext(ctx, "fault failed", slog.Int("fault", i), slog.Any("error", err))
	return err
}

func (c *Controller) fire(ctx context.Context, i int) error {
	f := c.faults[i]
	switch {
	case f.Kill != nil:
		return c.procFault(ctx, i, f.Kill, f.RestartAfter.Duration, c.t.KillServer, c.t.KillWorker)
	case f.Term != nil:
		return c.procFault(ctx, i, f.Term, f.RestartAfter.Duration, c.t.TermServer, c.t.TermWorker)
	case f.Stall != nil:
		return c.stallFault(ctx, i, f.Stall, f.Duration.Duration)
	case f.Flap != nil:
		return c.flapFault(ctx, i, f.Flap, f.Duration.Duration)
	case f.PowerCut != nil:
		return c.powerCut(ctx, i, f.PowerCut)
	}
	spec, s, _ := netFault(f)
	return c.laneFault(ctx, i, spec, s, f.Duration.Duration)
}

func (c *Controller) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// started records that fault i fired, at what, and whether it has a window.
func (c *Controller) started(ctx context.Context, i int, tgt string, windowed bool, groups []string) {
	c.mu.Lock()
	c.records[i].Start = time.Now()
	c.records[i].Target = tgt
	c.windows[i] = window{on: windowed, groups: groups}
	c.mu.Unlock()
	c.logger.InfoContext(ctx, "fault fired", slog.Int("fault", i), slog.String("kind", c.faults[i].Kind()), slog.String("target", tgt))
}

// healed records that fault i healed.
func (c *Controller) healed(ctx context.Context, i int) {
	c.mu.Lock()
	c.records[i].Healed = time.Now()
	c.mu.Unlock()
	c.logger.InfoContext(ctx, "fault healed", slog.Int("fault", i), slog.String("kind", c.faults[i].Kind()))
}

// Records returns the records of the faults fired so far, ordered by Index.
func (c *Controller) Records() []report.FaultRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []report.FaultRecord
	for _, r := range c.records {
		if !r.Start.IsZero() {
			out = append(out, r)
		}
	}
	return out
}

// Windows returns each fired network, server or power-cut fault as a
// verify.FaultWindow, in fault order. One that has not healed ends now.
func (c *Controller) Windows() []verify.FaultWindow {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []verify.FaultWindow
	for i, r := range c.records {
		if r.Start.IsZero() || !c.windows[i].on {
			continue
		}
		end := r.Healed
		if end.IsZero() {
			end = now
		}
		out = append(out, verify.FaultWindow{Start: r.Start, End: end, Groups: slices.Clone(c.windows[i].groups)})
	}
	return out
}

// HealAll heals every lane and resumes every stalled process. Run calls it
// on return; the runner calls it again on an interrupt before teardown. It is
// safe to call at any time, more than once, and while Run is running: no
// fault fires after it, and a fault still running cuts and stalls nothing
// more.
func (c *Controller) HealAll() {
	ctx := context.Background()
	c.mu.Lock()
	c.closed = true
	var keys []procKey
	for k := range c.stalled {
		keys = append(keys, k)
	}
	c.mu.Unlock()
	for _, n := range c.laneNames {
		c.lanes[n].healAll()
	}
	for _, k := range keys {
		l := c.procLock(k)
		l.Lock()
		c.mu.Lock()
		n := c.stalled[k]
		delete(c.stalled, k)
		c.mu.Unlock()
		if n > 0 {
			if err := c.resumeCall(k); err != nil {
				c.logger.WarnContext(ctx, "could not resume a stalled process", slog.Any("error", err))
			}
		}
		l.Unlock()
	}
}

// interrupted is the error of a fault whose heal ctx's end prevented.
func interrupted(ctx context.Context) error {
	return fmt.Errorf("interrupted before it healed: %w", context.Cause(ctx))
}

// unit is one process a kill, term or power cut takes down and brings back.
type unit struct {
	server  bool
	stop    func(context.Context) error
	restart func(context.Context) error
}

func (c *Controller) serverUnit(stop func(context.Context) error) unit {
	return unit{server: true, stop: stop, restart: c.t.RestartServer}
}

func (c *Controller) workerUnits(workers []int, stop func(context.Context, int) error) []unit {
	out := make([]unit, len(workers))
	for j, w := range workers {
		out[j] = unit{
			stop:    func(ctx context.Context) error { return stop(ctx, w) },
			restart: func(ctx context.Context) error { return c.t.RestartWorker(ctx, w) },
		}
	}
	return out
}

// procFault kills or terms the server or some of a group's real workers, and
// restarts them restartAfter later; with restartAfter 0 it leaves them down,
// and the fault never heals.
func (c *Controller) procFault(ctx context.Context, i int, p *scenario.ProcTarget, restartAfter time.Duration,
	stopServer func(context.Context) error, stopWorker func(context.Context, int) error,
) error {
	var units []unit
	if p.Server {
		c.started(ctx, i, "server", true, nil)
		units = []unit{c.serverUnit(stopServer)}
	} else {
		picked := c.pick(p.Group, p.Real)
		c.started(ctx, i, groupTarget(p.Group, picked), false, nil)
		if len(picked) == 0 {
			return fmt.Errorf("worker group %s has no running real worker", p.Group)
		}
		units = c.workerUnits(picked, stopWorker)
	}
	down, errs := stopUnits(ctx, units, nil)
	if restartAfter <= 0 {
		return errors.Join(errs...)
	}
	if !sleep.For(ctx, restartAfter) {
		return errors.Join(append(errs, interrupted(ctx))...)
	}
	errs = append(errs, restartUnits(ctx, down, nil)...)
	if len(errs) == 0 {
		c.healed(ctx, i)
	}
	return errors.Join(errs...)
}

// stopUnits stops every unit, and runs also, all at once. It returns the
// units it stopped and the errors of the others.
func stopUnits(ctx context.Context, units []unit, also func()) ([]unit, []error) {
	errs := make([]error, len(units))
	var wg sync.WaitGroup
	for j, u := range units {
		wg.Go(func() { errs[j] = u.stop(ctx) })
	}
	if also != nil {
		wg.Go(also)
	}
	wg.Wait()
	var down []unit
	for j, u := range units {
		if errs[j] == nil {
			down = append(down, u)
		}
	}
	return down, slices.DeleteFunc(errs, func(err error) bool { return err == nil })
}

// restartUnits restarts every unit, and runs also, all at once, and returns
// the errors.
func restartUnits(ctx context.Context, units []unit, also func(context.Context) error) []error {
	errs := make([]error, len(units)+1)
	var wg sync.WaitGroup
	for j, u := range units {
		wg.Go(func() { errs[j] = u.restart(ctx) })
	}
	if also != nil {
		wg.Go(func() { errs[len(units)] = also(ctx) })
	}
	wg.Wait()
	return slices.DeleteFunc(errs, func(err error) bool { return err == nil })
}

// running reports which real workers are running: those whose latest
// incarnation the harness has not ended.
func (c *Controller) running() map[int]bool {
	latest := map[int]target.Incarnation{}
	for _, inc := range c.t.Incarnations() {
		latest[inc.Worker] = inc
	}
	out := map[int]bool{}
	for w, inc := range latest {
		if inc.Ended.IsZero() {
			out[w] = true
		}
	}
	return out
}

// pick returns the first n (0 = all) of the group's real workers that are
// running.
func (c *Controller) pick(group string, n int) []int {
	running := c.running()
	var out []int
	for _, w := range c.t.RealWorkers(group) {
		if !running[w] {
			continue
		}
		out = append(out, w)
		if n > 0 && len(out) == n {
			break
		}
	}
	return out
}

// groupTarget names the workers a process fault hit, e.g.
// "group render (real 0,1)".
func groupTarget(group string, workers []int) string {
	ids := make([]string, len(workers))
	for j, w := range workers {
		ids[j] = strconv.Itoa(w)
	}
	if len(ids) == 0 {
		ids = []string{"none"}
	}
	return "group " + group + " (real " + strings.Join(ids, ",") + ")"
}

// netTarget names the groups a network fault hit, e.g. "groups [rack-b]".
func netTarget(spec []string) string {
	if len(spec) == 0 {
		return "all"
	}
	return fmt.Sprintf("groups %v", spec)
}

// stallFault stalls the server or some of a group's real workers for d, then
// resumes them, also when ctx ends first.
func (c *Controller) stallFault(ctx context.Context, i int, p *scenario.ProcTarget, d time.Duration) error {
	var keys []procKey
	if p.Server {
		c.started(ctx, i, "server", true, nil)
		keys = []procKey{{server: true}}
	} else {
		picked := c.pick(p.Group, p.Real)
		c.started(ctx, i, groupTarget(p.Group, picked), false, nil)
		if len(picked) == 0 {
			return fmt.Errorf("worker group %s has no running real worker", p.Group)
		}
		for _, w := range picked {
			keys = append(keys, procKey{worker: w})
		}
	}
	held := make([]bool, len(keys))
	errs := make([]error, 2*len(keys))
	var wg sync.WaitGroup
	for j, k := range keys {
		wg.Go(func() { held[j], errs[j] = c.stallProc(k) })
	}
	wg.Wait()
	sleep.For(ctx, d)
	for j, k := range keys {
		if held[j] {
			wg.Go(func() { errs[len(keys)+j] = c.resumeProc(k) })
		}
	}
	wg.Wait()
	err := errors.Join(errs...)
	if err == nil {
		c.healed(ctx, i)
	}
	return err
}

func (c *Controller) procLock(k procKey) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.procLocks[k]
	if !ok {
		l = &sync.Mutex{}
		c.procLocks[k] = l
	}
	return l
}

// stallProc stalls k, unless HealAll has run. held reports whether the stall
// counts, so that a resume must follow; it does even when the stall failed
// part way, since resuming a process that is not stopped does nothing.
func (c *Controller) stallProc(k procKey) (held bool, err error) {
	l := c.procLock(k)
	l.Lock()
	defer l.Unlock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return false, nil
	}
	c.stalled[k]++
	c.mu.Unlock()
	if k.server {
		return true, c.t.StallServer()
	}
	return true, c.t.StallWorker(k.worker)
}

// resumeProc ends one stall of k, and resumes k when no other stall holds it
// and HealAll has not resumed it already.
func (c *Controller) resumeProc(k procKey) error {
	l := c.procLock(k)
	l.Lock()
	defer l.Unlock()
	c.mu.Lock()
	n := c.stalled[k]
	switch {
	case n > 1:
		c.stalled[k] = n - 1
	case n == 1:
		delete(c.stalled, k)
	}
	c.mu.Unlock()
	if n != 1 {
		return nil
	}
	return c.resumeCall(k)
}

func (c *Controller) resumeCall(k procKey) error {
	if k.server {
		return c.t.ResumeServer()
	}
	return c.t.ResumeWorker(k.worker)
}

// laneFault holds the lanes of the groups in state s for d, also healing them
// when ctx ends first.
func (c *Controller) laneFault(ctx context.Context, i int, spec []string, s laneState, d time.Duration) error {
	names := c.resolve(spec)
	c.started(ctx, i, netTarget(spec), true, names)
	tok := c.tokens.Add(1)
	c.setLanes(names, tok, &s)
	sleep.For(ctx, d)
	c.setLanes(names, tok, nil)
	c.healed(ctx, i)
	return nil
}

// setLanes adds fault tok's state s to each named lane, or with a nil s
// removes it.
func (c *Controller) setLanes(names []string, tok uint64, s *laneState) {
	for _, n := range names {
		if s == nil {
			c.lanes[n].remove(tok)
		} else {
			c.lanes[n].add(tok, *s)
		}
	}
}

// flapFault cuts the groups' lanes for half of each period and opens them for
// the other half, until d has passed, and ends with them open.
func (c *Controller) flapFault(ctx context.Context, i int, f *scenario.FlapSpec, d time.Duration) error {
	names := c.resolve(f.Groups)
	c.started(ctx, i, netTarget(f.Groups), true, names)
	period := f.Period.Duration
	half := period / 2
	if half <= 0 {
		half = period
	}
	cut := laneState{level: levelCut}
	cutAt := time.Now()
	deadline := cutAt.Add(d)
	for {
		tok := c.tokens.Add(1)
		c.setLanes(names, tok, &cut)
		live := sleep.Until(ctx, minTime(cutAt.Add(half), deadline))
		c.setLanes(names, tok, nil)
		next := cutAt.Add(period)
		if !live || !next.Before(deadline) {
			if live {
				sleep.Until(ctx, deadline)
			}
			break
		}
		if !sleep.Until(ctx, next) {
			break
		}
		cutAt = next
	}
	c.healed(ctx, i)
	return nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// powerCut kills the server and every running real worker and crashes the
// simulated fleet, all at once; after Down it brings back the order's first
// side, after Gap the second. Workers brought back first boot with no server
// and fail, so once the server is back they are started again, as their
// supervisor would (see [Controller.supervise]).
func (c *Controller) powerCut(ctx context.Context, i int, pc *scenario.PowerCut) error {
	c.started(ctx, i, "all", true, nil)
	var workers []int
	for _, g := range c.groups {
		workers = append(workers, c.pick(g, 0)...)
	}
	units := append([]unit{c.serverUnit(c.t.KillServer)}, c.workerUnits(workers, c.t.KillWorker)...)
	var crash func()
	var restartFleet func(context.Context) error
	if c.fleet != nil {
		crash, restartFleet = c.fleet.CrashAll, c.fleet.RestartAll
	}
	down, errs := stopUnits(ctx, units, crash)
	var server, realWorkers []unit
	for _, u := range down {
		if u.server {
			server = append(server, u)
		} else {
			realWorkers = append(realWorkers, u)
		}
	}
	if !sleep.For(ctx, pc.Down.Duration) {
		return errors.Join(append(errs, interrupted(ctx))...)
	}
	if pc.Order == scenario.PowerCutWorkersFirst {
		// The workers boot with no server to reach. sqi-worker exits when
		// it cannot reach the broker at boot, and its supervisor starts it
		// again (the shipped systemd unit: Restart=on-failure,
		// RestartSec=5s), so these failures are expected, and the
		// supervisor's retries below bring the workers up.
		if boot := restartUnits(ctx, realWorkers, restartFleet); len(boot) > 0 {
			c.logger.InfoContext(ctx, "workers failed to boot with the server down, as expected; retrying them once it is back",
				slog.Int("fault", i), slog.Any("error", errors.Join(boot...)))
		}
	} else {
		errs = append(errs, restartUnits(ctx, server, nil)...)
	}
	if !sleep.For(ctx, pc.Gap.Duration) {
		return errors.Join(append(errs, interrupted(ctx))...)
	}
	if pc.Order == scenario.PowerCutWorkersFirst {
		errs = append(errs, restartUnits(ctx, server, nil)...)
		errs = append(errs, c.supervise(ctx, realWorkers, restartFleet)...)
	} else {
		errs = append(errs, restartUnits(ctx, realWorkers, restartFleet)...)
	}
	if len(errs) == 0 {
		c.healed(ctx, i)
	}
	return errors.Join(errs...)
}

// supervisorEvery and supervisorTries are how often, and how many times, a
// power cut's supervisor tries to start the workers that are not running:
// the shipped systemd unit's RestartSec, for about a minute.
var (
	supervisorEvery = 5 * time.Second
	supervisorTries = 12
)

// supervise restarts every real worker of workers that is not running, and
// runs fleet, which restarts the simulated workers that are not, as a service
// supervisor does: every supervisorEvery until none fails, at most
// supervisorTries times. It returns the last try's errors. A real worker
// that is already running is left alone.
func (c *Controller) supervise(ctx context.Context, workers []unit, fleet func(context.Context) error) []error {
	var errs []error
	for try := 1; try <= supervisorTries; try++ {
		errs = slices.DeleteFunc(restartUnits(ctx, workers, fleet), func(err error) bool {
			return errors.Is(err, target.ErrWorkerRunning)
		})
		if len(errs) == 0 || try == supervisorTries {
			break
		}
		c.logger.InfoContext(ctx, "supervisor: workers still failing to start; retrying",
			slog.Int("try", try), slog.Duration("every", supervisorEvery), slog.Any("error", errors.Join(errs...)))
		if !sleep.For(ctx, supervisorEvery) {
			return append(errs, interrupted(ctx))
		}
	}
	return errs
}
