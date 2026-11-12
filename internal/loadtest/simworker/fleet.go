// SPDX-License-Identifier: AGPL-3.0-or-later

package simworker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uberware/sqi/internal/loadtest/internal/sleep"

	"github.com/uberware/sqi/internal/loadtest/scenario"
)

const (
	// startConcurrency bounds how many workers connect and register at once,
	// whatever the ramp rate, so 10,000 starts behind a fast ramp do not open
	// 10,000 connections together.
	startConcurrency = 256
	// stopTimeout bounds the graceful part of Fleet.Stop: past it, in-flight
	// tasks end failed with "worker_shutdown" and the rest are crashed.
	stopTimeout = 30 * time.Second
	// stopErrorsShown is how many individual Stop errors the joined error names.
	stopErrorsShown = 5
	// restartTimeout bounds one churn restart's connect and registration.
	restartTimeout = time.Minute
	// disconnectTimeout bounds one churn disconnect; Worker.Disconnect bounds
	// itself a little lower.
	disconnectTimeout = disconnectBound + 5*time.Second
	// churnWindow is how far ahead RunChurn generates events at a time.
	churnWindow = time.Hour
	// defaultIDPrefix names the workers of a fleet configured with no prefix.
	defaultIDPrefix = "sim"
	// goldenGamma is the 64-bit golden-ratio increment splitmix64 steps by.
	goldenGamma = 0x9e3779b97f4a7c15
)

// FleetConfig describes a fleet of simulated workers.
type FleetConfig struct {
	RunID   string
	FarmID  string
	NATSURL string
	// NATSURLFor, when set, gives each group's broker URL (its lane); nil means
	// NATSURL for every group.
	NATSURLFor func(group string) string
	IDPrefix   string
	// Groups are the worker groups. Each group's Count simulated workers start;
	// its Real count belongs to the managed target and is ignored here.
	Groups []scenario.WorkerGroup
	// QueueIDs maps a queue name to its ID. A group's queue that is not a key
	// is taken to be an ID already. Nil means every group queue is an ID.
	QueueIDs map[string]string
	// Creds supplies each worker's broker credential. Nil means none
	// ([NoCredentials]).
	Creds Credentials
	// TLS is the broker TLS material, passed to every worker.
	TLS TLSFiles
	// Seed derives every random choice: each worker's behavior seed and the
	// churn schedule.
	Seed int64
}

// Fleet is a set of simulated workers: it ramps them up at each group's rate,
// shuts them down, and drives their crashes and disconnects.
type Fleet struct {
	cfg      FleetConfig
	behavior Behavior
	ledger   *Ledger
	logger   *slog.Logger

	members []*member
	groups  []groupRange
	ids     map[string]string // every member's worker ID, to its group name

	started  atomic.Bool
	stopOnce sync.Once
	stopCh   chan struct{} // closed when Stop begins

	crashes, restarts, disconnects, skipped, failures atomic.Int64

	// stopHook, when set, replaces Worker.Stop in Fleet.Stop. A test hook.
	stopHook func(ctx context.Context, w *Worker) error
}

// errFleetStopped is what a start that lost a race with Stop returns.
var errFleetStopped = errors.New("simworker: fleet is stopping")

// member is one worker slot. The Worker exists from the start of its Start
// attempt on, once its credential has been fetched.
type member struct {
	spec   WorkerSpec
	group  int // index into Fleet.groups
	worker atomic.Pointer[Worker]
	// evMu serializes this worker's churn events, and Stop's final sweep
	// against them.
	evMu sync.Mutex
}

// groupRange is the slice of Fleet.members one group occupies.
type groupRange struct {
	first, count int
	spec         scenario.WorkerGroup
}

// NewFleet returns a fleet that has started nothing. Worker IDs are
// "<IDPrefix>-<group>-<n>" and hostnames "sim-<RunID>-<group>-<n>", n from 1
// within the group. A nil logger discards.
func NewFleet(cfg FleetConfig, b Behavior, l *Ledger, logger *slog.Logger) *Fleet {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if cfg.IDPrefix == "" {
		cfg.IDPrefix = defaultIDPrefix
	}
	f := &Fleet{cfg: cfg, behavior: b, ledger: l, logger: logger, stopCh: make(chan struct{})}
	for gi, g := range cfg.Groups {
		f.groups = append(f.groups, groupRange{first: len(f.members), count: g.Count, spec: g})
		for n := 1; n <= g.Count; n++ {
			idx := len(f.members)
			f.members = append(f.members, &member{group: gi, spec: f.specFor(g, n, idx)})
		}
	}
	f.ids = make(map[string]string, len(f.members))
	for _, m := range f.members {
		f.ids[m.spec.ID] = m.spec.Group.Name
	}
	return f
}

// Simulates reports whether workerID is one of the fleet's workers.
func (f *Fleet) Simulates(workerID string) bool {
	_, ok := f.ids[workerID]
	return ok
}

// GroupOf returns the group name of one of the fleet's workers, "" otherwise.
func (f *Fleet) GroupOf(workerID string) string { return f.ids[workerID] }

func (f *Fleet) specFor(g scenario.WorkerGroup, n, global int) WorkerSpec {
	g.Queues = f.resolveQueues(g.Queues)
	natsURL := f.cfg.NATSURL
	if f.cfg.NATSURLFor != nil {
		natsURL = f.cfg.NATSURLFor(g.Name)
	}
	return WorkerSpec{
		ID:       f.cfg.IDPrefix + "-" + g.Name + "-" + strconv.Itoa(n),
		Hostname: "sim-" + f.cfg.RunID + "-" + g.Name + "-" + strconv.Itoa(n),
		FarmID:   f.cfg.FarmID,
		Group:    g,
		NATSURL:  natsURL,
		TLS:      f.cfg.TLS,
		RandSeed: workerSeed(f.cfg.Seed, global),
	}
}

func (f *Fleet) resolveQueues(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	out := make([]string, len(names))
	for i, q := range names {
		if id, ok := f.cfg.QueueIDs[q]; ok {
			out[i] = id
		} else {
			out[i] = q
		}
	}
	return out
}

// Specs returns the spec of every worker the fleet runs, without credentials
// (those are fetched by Start), in group then index order.
func (f *Fleet) Specs() []WorkerSpec {
	out := make([]WorkerSpec, len(f.members))
	for i, m := range f.members {
		out[i] = m.spec
	}
	return out
}

// workerSeed derives a worker's behavior seed from the run seed and the
// worker's index across the whole fleet (splitmix64, so neighboring indexes
// and seeds give unrelated streams).
func workerSeed(seed int64, global int) uint64 {
	return splitmix64(bits(seed) + bits(int64(global+1))*goldenGamma)
}

// bits reinterprets a signed seed as the unsigned value the generators take.
func bits(seed int64) uint64 { return uint64(seed) } //nolint:gosec // G115: a seed is a bit pattern, wrapping is intended

// signed is the inverse of bits.
func signed(u uint64) int64 { return int64(u) } //nolint:gosec // G115: a seed is a bit pattern, wrapping is intended

func splitmix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

// Live returns how many workers currently have a running process. It does not
// wait for a worker that is mid-way through a lifecycle operation.
func (f *Fleet) Live() int {
	n := 0
	for _, m := range f.members {
		if w := m.worker.Load(); w != nil && w.alive() {
			n++
		}
	}
	return n
}

// Start ramps every group up at its Ramp.PerSecond (all at once when 0) and
// returns once every worker is registered. Groups ramp concurrently; within a
// group worker k is due k/rate seconds after Start was called, so a slow
// connect does not slow the schedule (at most [startConcurrency] workers are
// connecting at any moment, which does).
//
// The first worker to fail aborts the fleet: Start stops launching, waits for
// the starts in flight, and returns that error naming the worker. A scenario
// with workers missing is not the scenario asked for. Workers that did start
// stay up; the caller still calls Stop. Start may be called once.
func (f *Fleet) Start(ctx context.Context) error {
	if !f.started.CompareAndSwap(false, true) {
		return errors.New("simworker: fleet already started")
	}
	if err := f.checkUnique(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		firstErr  error
		errOnce   sync.Once
		pacers    sync.WaitGroup
		starts    sync.WaitGroup
		sem       = make(chan struct{}, startConcurrency)
		t0        = time.Now()
		recordErr = func(err error) {
			errOnce.Do(func() {
				firstErr = err
				cancel()
			})
		}
	)
	for gi := range f.groups {
		pacers.Go(func() { f.pace(ctx, gi, t0, sem, &starts, recordErr) })
	}
	pacers.Wait()
	starts.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

// pace launches one group's workers on its ramp schedule.
func (f *Fleet) pace(ctx context.Context, gi int, t0 time.Time, sem chan struct{}, starts *sync.WaitGroup, fail func(error)) {
	g := f.groups[gi]
	rate := g.spec.Ramp.PerSecond
	for k := range g.count {
		if rate > 0 {
			due := t0.Add(time.Duration(float64(k) / rate * float64(time.Second)))
			if !sleep.Until(ctx, due) {
				return
			}
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		m := f.members[g.first+k]
		starts.Go(func() {
			defer func() { <-sem }()
			if err := f.startMember(ctx, m); err != nil {
				fail(fmt.Errorf("simworker: start worker %s: %w", m.spec.ID, err))
			}
		})
	}
}

func (f *Fleet) startMember(ctx context.Context, m *member) error {
	if f.stopping() {
		return errFleetStopped
	}
	spec := m.spec
	if f.cfg.Creds != nil {
		seed, pub, err := f.cfg.Creds.For(ctx, spec.ID)
		if err != nil {
			return err
		}
		spec.Seed, spec.PublicKey = seed, pub
	}
	w := NewWorker(spec, f.behavior, f.ledger, f.logger)
	m.worker.Store(w)
	if err := w.Start(ctx); err != nil {
		return err
	}
	// Stop may have passed this member before the worker was stored; it will
	// not stop a worker it never saw.
	if f.stopping() {
		w.Crash()
		return errFleetStopped
	}
	return nil
}

func (f *Fleet) checkUnique() error {
	ids := make(map[string]struct{}, len(f.members))
	for _, m := range f.members {
		if _, dup := ids[m.spec.ID]; dup {
			return fmt.Errorf("simworker: worker ID %s is used twice (group names must be unique)", m.spec.ID)
		}
		ids[m.spec.ID] = struct{}{}
	}
	return nil
}

// Stop shuts the fleet down: every worker stops gracefully at once (see
// [Worker.Stop]), with in-flight tasks given [stopTimeout] (or ctx, if sooner)
// to finish; whatever then still runs is crashed. It also ends churn: events
// due after Stop begins do nothing. Stop is safe to call more than once. The
// returned error joins the first few worker errors.
func (f *Fleet) Stop(ctx context.Context) error {
	f.stopOnce.Do(func() { close(f.stopCh) })
	sctx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()

	var (
		errs errList
		wg   sync.WaitGroup
	)
	// Every worker stops at once, with no concurrency bound: a worker waiting
	// its turn would keep leasing work that then ends "worker_shutdown", and
	// would find the shared deadline spent by the workers ahead of it.
	for _, m := range f.members {
		w := m.worker.Load()
		if w == nil {
			continue
		}
		wg.Go(func() {
			if err := f.stopWorker(sctx, w); err != nil {
				errs.add(fmt.Errorf("stop worker %s: %w", m.spec.ID, err))
			}
		})
	}
	wg.Wait()

	f.sweepCrash()
	return errs.join("failed to stop")
}

// errList collects errors from concurrent goroutines, keeping the first
// [stopErrorsShown] and counting the rest.
type errList struct {
	mu    sync.Mutex
	errs  []error
	total int
}

func (e *errList) add(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.total++
	if len(e.errs) < stopErrorsShown {
		e.errs = append(e.errs, err)
	}
}

// join returns the kept errors, joined, with a line counting the ones left out
// ("N more workers <what>").
func (e *errList) join(what string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	errs := e.errs
	if e.total > len(errs) {
		errs = append(slices.Clip(errs), fmt.Errorf("simworker: %d more workers %s", e.total-len(errs), what))
	}
	return errors.Join(errs...)
}

// stopWorker is Worker.Stop, replaceable by a test.
func (f *Fleet) stopWorker(ctx context.Context, w *Worker) error {
	if f.stopHook != nil {
		return f.stopHook(ctx, w)
	}
	return w.Stop(ctx)
}

// sweepCrash crashes any worker still running after the graceful stop: a churn
// restart that raced it. It takes each member's event lock, so no churn event
// restarts a worker afterwards (an event checks for Stop under that lock).
func (f *Fleet) sweepCrash() {
	for _, m := range f.members {
		w := m.worker.Load()
		if w == nil {
			continue
		}
		m.evMu.Lock()
		w.Crash()
		m.evMu.Unlock()
	}
}

func (f *Fleet) stopping() bool {
	select {
	case <-f.stopCh:
		return true
	default:
		return false
	}
}

// ── Power cuts ───────────────────────────────────────────────────────────────

// CrashAll crashes every running worker at once, with no deregistration (see
// [Worker.Crash]); it returns when they are all down. Each worker's crash
// waits for that worker's churn event in progress, if any, so it never splits
// one. Nothing is restarted: [Fleet.RestartAll] brings them back. Workers
// crashed here count in [ChurnCounts].Crashes.
func (f *Fleet) CrashAll() {
	var wg sync.WaitGroup
	for _, m := range f.members {
		wg.Go(func() { f.crashMember(m) })
	}
	wg.Wait()
}

func (f *Fleet) crashMember(m *member) {
	m.evMu.Lock()
	defer m.evMu.Unlock()
	w := m.worker.Load()
	if f.stopping() || w == nil || !w.alive() {
		return
	}
	w.Crash()
	if f.stopping() {
		return // Stop may have ended the worker first, making the crash a no-op
	}
	f.crashes.Add(1)
}

// RestartAll restarts every crashed worker as a new process on its worker ID
// (a new instance ID, see [Worker.Restart]), at most [startConcurrency] at a
// time, and returns the joined errors of those that failed. A worker that is
// running, or was never started, is left alone, and nothing restarts once Stop
// has begun. ctx bounds the whole call; each restart is also bounded by
// [restartTimeout]. Workers restarted here count in [ChurnCounts].Restarts.
func (f *Fleet) RestartAll(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("simworker: restart all: %w", err)
	}
	var (
		errs errList
		wg   sync.WaitGroup
		sem  = make(chan struct{}, startConcurrency)
	)
launch:
	for _, m := range f.members {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			errs.add(fmt.Errorf("simworker: restart all: %w", ctx.Err()))
			break launch
		}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := f.restartMember(ctx, m); err != nil {
				errs.add(fmt.Errorf("restart worker %s: %w", m.spec.ID, err))
			}
		})
	}
	wg.Wait()
	return errs.join("failed to restart")
}

func (f *Fleet) restartMember(ctx context.Context, m *member) error {
	m.evMu.Lock()
	defer m.evMu.Unlock()
	w := m.worker.Load()
	if f.stopping() || w == nil || w.alive() {
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, restartTimeout)
	defer cancel()
	if err := w.Restart(rctx); err != nil {
		return err
	}
	f.restarts.Add(1)
	return nil
}

// ── Churn ────────────────────────────────────────────────────────────────────

type churnKind int

const (
	churnCrash churnKind = iota
	churnDisconnect
)

// churnEvent is one crash or disconnect of the group's worker number Worker
// (from 0), At after the schedule's start.
type churnEvent struct {
	Worker int
	At     time.Duration
	Kind   churnKind
}

func compareChurn(a, b churnEvent) int {
	return cmp.Or(cmp.Compare(a.At, b.At), cmp.Compare(a.Worker, b.Worker), cmp.Compare(a.Kind, b.Kind))
}

// churnSchedule returns the crashes and disconnects of n workers of group over
// [0, horizon), sorted by time. Each worker's crashes and disconnects are
// independent Poisson processes at the group's hourly rates (exponentially
// distributed gaps), each drawn from its own generator seeded by (seed, worker,
// kind), so a worker's events depend on neither the group size nor the other
// kind's rate. It is pure: a seed gives one schedule.
func churnSchedule(seed int64, group scenario.WorkerGroup, n int, horizon time.Duration) []churnEvent {
	var out []churnEvent
	for w := range n {
		out = poisson(out, seed, w, churnCrash, group.Churn.CrashPerHour, horizon)
		out = poisson(out, seed, w, churnDisconnect, group.Churn.DisconnectPerHour, horizon)
	}
	slices.SortFunc(out, compareChurn)
	return out
}

func poisson(out []churnEvent, seed int64, worker int, kind churnKind, perHour float64, horizon time.Duration) []churnEvent {
	if perHour <= 0 || math.IsNaN(perHour) || math.IsInf(perHour, 0) {
		return out
	}
	r := rand.New(rand.NewPCG(bits(seed), uint64(worker)*2+uint64(kind))) //nolint:gosec // G404: the schedule must be reproducible from the run seed
	perSec := perHour / 3600
	limit := horizon.Seconds()
	for t := r.ExpFloat64() / perSec; t < limit; t += r.ExpFloat64() / perSec {
		out = append(out, churnEvent{Worker: worker, At: time.Duration(t * float64(time.Second)), Kind: kind})
	}
	return out
}

// windowSeed is the seed of churn window w of a group; window 0 uses the
// group's seed itself.
func windowSeed(groupSeed int64, w int) int64 {
	if w == 0 {
		return groupSeed
	}
	return signed(splitmix64(bits(groupSeed) + bits(int64(w))*goldenGamma))
}

func (f *Fleet) groupSeed(gi int) int64 {
	return signed(splitmix64(bits(f.cfg.Seed) ^ bits(int64(gi+1))*goldenGamma))
}

// ChurnCounts is what RunChurn has done so far.
type ChurnCounts struct {
	// Crashes and Disconnects are events carried out; Crashes include those of
	// [Fleet.CrashAll].
	Crashes, Disconnects int64
	// Restarts are workers brought back after a crash, by churn or by
	// [Fleet.RestartAll].
	Restarts int64
	// Skipped are events due for a worker that was not running.
	Skipped int64
	// Failures are events whose restart, or reconnect and re-registration,
	// failed.
	Failures int64
}

// ChurnCounts returns the churn events carried out so far.
func (f *Fleet) ChurnCounts() ChurnCounts {
	return ChurnCounts{
		Crashes: f.crashes.Load(), Restarts: f.restarts.Load(), Disconnects: f.disconnects.Load(),
		Skipped: f.skipped.Load(), Failures: f.failures.Load(),
	}
}

// RunChurn carries out the groups' churn against wall time, from the moment it
// is called until ctx ends or Stop begins, then returns once the events in
// flight have finished. Events are generated an hour at a time (the first
// window is [churnSchedule] of the group's seed, so a ctx deadline shorter than
// an hour sees a prefix of it), each window from its own derived seed.
//
// A crash is [Worker.Crash], a wait of the group's RestartAfter, then
// [Worker.Restart] on the then-crashed worker: never a restart of a live,
// busy one. If ctx ends or Stop begins during the wait, a ctx end restarts the
// worker at once (a drain needs it up) and a Stop does not. A disconnect is
// [Worker.Disconnect]. Events for one worker never overlap: a later one waits
// for the earlier to finish. An event for a worker that is not running is
// skipped.
func (f *Fleet) RunChurn(ctx context.Context) {
	start := time.Now()
	var wg sync.WaitGroup
	for gi, g := range f.groups {
		if g.spec.Churn.CrashPerHour <= 0 && g.spec.Churn.DisconnectPerHour <= 0 {
			continue
		}
		wg.Go(func() { f.churnGroup(ctx, start, gi) })
	}
	wg.Wait()
}

func (f *Fleet) churnGroup(ctx context.Context, start time.Time, gi int) {
	g := f.groups[gi]
	var events sync.WaitGroup
	defer events.Wait()
	for w := 0; ; w++ {
		winStart := time.Duration(w) * churnWindow
		horizon := churnWindow
		if dl, ok := ctx.Deadline(); ok {
			left := dl.Sub(start) - winStart
			if left <= 0 {
				return
			}
			horizon = min(horizon, left)
		}
		for _, e := range churnSchedule(windowSeed(f.groupSeed(gi), w), g.spec, g.count, horizon) {
			if !f.waitChurn(ctx, start.Add(winStart+e.At)) {
				return
			}
			m := f.members[g.first+e.Worker]
			events.Go(func() { f.runEvent(ctx, m, e.Kind) })
		}
		if !f.waitChurn(ctx, start.Add(winStart+horizon)) {
			return
		}
	}
}

// waitChurn waits for t and reports whether churn should go on: ctx live and
// the fleet not stopping.
func (f *Fleet) waitChurn(ctx context.Context, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err() == nil && !f.stopping()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-f.stopCh:
		return false
	case <-timer.C:
		return true
	}
}

func (f *Fleet) runEvent(ctx context.Context, m *member, kind churnKind) {
	m.evMu.Lock()
	defer m.evMu.Unlock()
	w := m.worker.Load()
	if f.stopping() || w == nil || !w.alive() {
		f.skipped.Add(1)
		return
	}
	switch kind {
	case churnCrash:
		f.crashAndRestart(ctx, m, w)
	case churnDisconnect:
		f.disconnect(ctx, m, w)
	}
}

func (f *Fleet) crashAndRestart(ctx context.Context, m *member, w *Worker) {
	f.logger.InfoContext(ctx, "fleet: crashing worker", slog.String("worker_id", m.spec.ID))
	w.Crash()
	if f.stopping() {
		return // Stop may have ended the worker first, making the crash a no-op
	}
	f.crashes.Add(1)
	select {
	case <-time.After(f.groups[m.group].spec.Churn.RestartAfter.Duration):
	case <-f.stopCh:
		return
	case <-ctx.Done():
	}
	if f.stopping() {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restartTimeout)
	defer cancel()
	if err := w.Restart(rctx); err != nil {
		f.failures.Add(1)
		f.logger.WarnContext(ctx, "fleet: restart failed", slog.String("worker_id", m.spec.ID), slog.Any("error", err))
		return
	}
	f.restarts.Add(1)
}

func (f *Fleet) disconnect(ctx context.Context, m *member, w *Worker) {
	f.logger.InfoContext(ctx, "fleet: disconnecting worker", slog.String("worker_id", m.spec.ID))
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), disconnectTimeout)
	defer cancel()
	err := w.Disconnect(dctx)
	if f.stopping() {
		return // a Stop that landed first makes Disconnect fail with errNotRunning
	}
	f.disconnects.Add(1)
	if err != nil {
		f.failures.Add(1)
		f.logger.WarnContext(ctx, "fleet: disconnect failed", slog.String("worker_id", m.spec.ID), slog.Any("error", err))
	}
}
