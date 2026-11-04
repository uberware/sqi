// SPDX-License-Identifier: AGPL-3.0-or-later

// Package runner runs one load scenario end to end against an sqi-server and
// writes its record. It wires the harness together in a fixed order: start
// the target, ramp the simulated fleet, start the observer and the UI
// clients, submit jobs (with the scenario's actions and churn) for the
// scenario's duration, wait for the jobs to drain, check the invariants while
// the fleet is still up, stop the fleet and the clients, take the observer's
// last sample, close the target, and write run.json, summary.md and
// series.csv.
//
// A run interrupted through its context still tears everything down and
// writes its record, marked incomplete.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/driver"
	"github.com/uberware/sqi/internal/loadtest/measure"
	"github.com/uberware/sqi/internal/loadtest/observe"
	"github.com/uberware/sqi/internal/loadtest/report"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
	"github.com/uberware/sqi/internal/loadtest/target"
	"github.com/uberware/sqi/internal/loadtest/verify"
)

const (
	// DefaultOutDir is where run directories go when Options.OutDir is "".
	DefaultOutDir = "runs"
	// DefaultSample is the observer's sampling period when Options.Sample is 0.
	DefaultSample = 5 * time.Second
	// DefaultSoakProfileEvery is a soak's heap and goroutine profile period
	// when Options.ProfileEvery is 0. A load run takes them only at its start
	// and end.
	DefaultSoakProfileEvery = 15 * time.Minute

	// cpuProfileLength is the longest CPU profile taken at peak load.
	cpuProfileLength = 30 * time.Second
	// minCPUProfile is the shortest CPU profile worth taking.
	minCPUProfile = 2 * time.Second

	// The teardown bounds. Each teardown step gets its own context, cut short
	// by Options.Abort.
	uiStopTimeout        = time.Minute
	fleetStopTimeout     = time.Minute
	interruptedFleetStop = 5 * time.Second
	observerStopTimeout  = time.Minute
	serverQueryTimeout   = 2 * time.Minute
	// closeTimeout covers a managed target's 30 s worker and 30 s server
	// grace, and an attached one's wait for its workers to go offline.
	closeTimeout = 5 * time.Minute
	// emergencyStop bounds each step of the teardown after a panic.
	emergencyStop = 10 * time.Second

	// checkTimeout bounds the invariant checks.
	checkTimeout = 30 * time.Minute
	// drainSlack is added to the scenario's drain timeout for the context of
	// WaitDrained, which applies the timeout itself.
	drainSlack = time.Minute
)

// Options configures a run.
type Options struct {
	// Scenario is the resolved scenario: overrides applied, defaults filled
	// and validated (Run validates it again for its target).
	Scenario *scenario.Scenario
	// Mode is report.ModeLoad (the default), report.ModeSoak or
	// report.ModeSweepPoint.
	Mode string
	// RunID names the run, its directory and its farm; "" generates one with
	// NewRunID. The run directory is OutDir/RunID, or OutDir/RunID-s<i> for a
	// shard of several, unless Dir names it.
	RunID, OutDir string
	Dir           string
	// Seed seeds every random choice of the run; 0 derives it from RunID
	// (SeedFor). A shard adds its index, so shards differ.
	Seed int64
	// Sample is the observer's sampling period (DefaultSample when 0).
	// ProfileEvery is the period of heap and goroutine profiles: 0 means
	// DefaultSoakProfileEvery in a soak and start and end only otherwise; a
	// negative value means start and end only.
	Sample, ProfileEvery time.Duration
	// Shard runs one slice of the scenario (see ShardSpec.Apply).
	Shard ShardSpec
	// AllowSaturated keeps an INVALID run (a saturated generator) from
	// counting against it: the record stays marked INVALID and gains a
	// warning finding. The CLI uses it to choose its exit status.
	AllowSaturated bool
	// Managed starts the run's own sqi-server; Attach drives one somebody
	// else runs. Neither set means a managed target with defaults. Run fills
	// RunDir, RunID, Scenario, RealWorkers and Logger itself; an empty
	// ServerBin builds the binaries into OutDir/.bin.
	Managed *target.ManagedConfig
	Attach  *target.AttachConfig
	// PostgresDSN, for an attached target, is the server's database, so the
	// observer can measure its size. A managed target supplies its own.
	PostgresDSN string
	// Flags is the command line, for the record's environment.
	Flags  []string
	Logger *slog.Logger
	// Abort, when closed, cuts the teardown short: processes are killed
	// rather than waited for. The CLI closes it on a second interrupt.
	Abort <-chan struct{}
	// BeforeVerify, when set, may change the verifier's input just before
	// the invariants are checked. A test hook.
	BeforeVerify func(*verify.Input)
	// OnTarget, when set, is called with the target once it has started. A
	// test hook.
	OnTarget func(target.Target)
}

// Directory is the run directory the options name.
func (o *Options) Directory() string {
	if o.Dir != "" {
		return o.Dir
	}
	name := o.RunID
	if o.Shard.active() {
		name += "-" + o.Shard.Prefix()
	}
	return filepath.Join(o.OutDir, name)
}

// Run runs the scenario once and writes its record into the run directory.
//
// It returns the record and nil for a run that went to its end, and for one
// interrupted through ctx (the record says Complete: false). It returns nil
// and an error when the run could not be set up (bad options, a target or
// fleet that would not start), after stopping whatever it had started. It
// returns the record and an error when something failed after setup: a
// driver error (the run is then incomplete), the target's teardown, or
// writing the record.
//
// A panic in the goroutine calling Run stops the fleet and the target before
// it continues. A panic on another goroutine ends the process at once; the
// managed target's processes then die with it on Linux and Windows (parent
// death signal, job object) but not on macOS.
func Run(ctx context.Context, o Options) (*report.Record, error) {
	r, err := newRun(o)
	if err != nil {
		return nil, err
	}
	return r.execute(ctx)
}

// run is the state of one Run.
type run struct {
	o      Options
	sc     *scenario.Scenario // this shard's scenario
	dir    string
	seed   int64
	logger *slog.Logger

	tgt      target.Target
	c        *client.Client
	ledger   *simworker.Ledger
	jobs     *driver.JobLedger
	metrics  *driver.Metrics
	recorder *measure.Recorder
	fleet    *simworker.Fleet
	obs      *observe.Observer
	actions  *driver.Actions

	uiCancel, obsCancel context.CancelFunc
	uiDone, obsDone     chan struct{}

	began, started, ended time.Time
	complete              bool
	tornDown              bool
	uiRunning             bool // the UI clients did not stop in time
	invariants            []verify.Result
	serverJobs            map[string]client.Job
	serverErr             error

	mu          sync.Mutex // guards the fields below
	steady      map[string]int64
	unavailable []string
	errs        []error
}

func newRun(o Options) (*run, error) {
	if o.Scenario == nil {
		return nil, errors.New("runner: no scenario")
	}
	kind := scenario.TargetManaged
	switch {
	case o.Managed != nil && o.Attach != nil:
		return nil, errors.New("runner: a run is either managed or attached, not both")
	case o.Attach != nil:
		kind = scenario.TargetAttach
	case o.Managed == nil:
		o.Managed = &target.ManagedConfig{}
	}
	if err := o.Scenario.Validate(kind); err != nil {
		return nil, fmt.Errorf("runner: scenario %s: %w", o.Scenario.Name, err)
	}
	if err := o.defaults(); err != nil {
		return nil, err
	}
	logger := o.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &run{
		o:        o,
		sc:       o.Shard.Apply(o.Scenario),
		dir:      o.Directory(),
		seed:     o.Seed + int64(o.Shard.Index),
		logger:   logger,
		ledger:   simworker.NewLedger(),
		jobs:     driver.NewJobLedger(),
		metrics:  driver.NewMetrics(o.Scenario.Clients.RESTPollers.Endpoints),
		recorder: measure.NewRecorder(),
	}, nil
}

// defaults fills and checks the options Run may fill itself.
func (o *Options) defaults() error {
	switch o.Mode {
	case "":
		o.Mode = report.ModeLoad
	case report.ModeLoad, report.ModeSoak, report.ModeSweepPoint:
	default:
		return fmt.Errorf("runner: unknown mode %q", o.Mode)
	}
	if o.RunID == "" {
		o.RunID = NewRunID(time.Now())
	}
	if err := ValidRunID(o.RunID); err != nil {
		return fmt.Errorf("runner: %w", err)
	}
	if o.Shard.active() && (o.Shard.Index < 0 || o.Shard.Index >= o.Shard.Of) {
		return fmt.Errorf("runner: shard %d/%d is out of range", o.Shard.Index, o.Shard.Of)
	}
	if o.OutDir == "" {
		o.OutDir = DefaultOutDir
	}
	if o.Seed == 0 {
		o.Seed = SeedFor(o.RunID)
	}
	if o.Sample <= 0 {
		o.Sample = DefaultSample
	}
	switch {
	case o.ProfileEvery < 0:
		o.ProfileEvery = 0
	case o.ProfileEvery == 0 && o.Mode == report.ModeSoak:
		o.ProfileEvery = DefaultSoakProfileEvery
	}
	return nil
}

// targetRunID is the run ID the target and fleet are given: a shard's is
// suffixed with its prefix, so the shards' farms and hostnames differ.
func (r *run) targetRunID() string {
	if r.o.Shard.active() {
		return r.o.RunID + "-" + r.o.Shard.Prefix()
	}
	return r.o.RunID
}

// rand returns the run's random stream number stream.
func (r *run) rand(stream uint64) *rand.Rand {
	return rand.New(rand.NewPCG(uint64(r.seed), stream)) //nolint:gosec // G115: a seed is a bit pattern; G404: load simulation, not security
}

func (r *run) execute(ctx context.Context) (*report.Record, error) {
	defer func() {
		if p := recover(); p != nil {
			r.emergencyStop(ctx)
			panic(p)
		}
	}()
	r.began = time.Now()
	r.logger.InfoContext(ctx, "starting run", slog.String("run_id", r.o.RunID), slog.Int64("seed", r.o.Seed),
		slog.String("scenario", r.sc.Name), slog.String("dir", r.dir))
	if err := r.setup(ctx); err != nil {
		if ctx.Err() == nil {
			r.teardown(ctx)
			return nil, errors.Join(err, r.err())
		}
		r.logger.WarnContext(ctx, "run interrupted during setup", slog.Any("error", err))
	} else {
		r.measure(ctx)
	}
	r.teardown(ctx)
	rec := r.record()
	if err := rec.Write(r.dir); err != nil {
		r.fail(fmt.Errorf("runner: write the record: %w", err))
	}
	r.logger.InfoContext(ctx, "run finished", slog.String("dir", r.dir), slog.Bool("complete", rec.Complete), slog.Bool("valid", rec.Valid))
	return rec, r.err()
}

// fail records an error. One recorded while jobs are submitted ends the
// measured part of the run early, leaving it incomplete.
func (r *run) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, err)
}

func (r *run) failed() bool { return r.err() != nil }

// err joins the errors recorded so far.
func (r *run) err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return errors.Join(r.errs...)
}

// unavail records a measurement the run could not take, as
// "<measurement>: <reason>".
func (r *run) unavail(measurement, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unavailable = append(r.unavailable, measurement+": "+reason)
}

// setup starts the target, ramps the fleet, and starts the observer and the
// UI clients.
func (r *run) setup(ctx context.Context) error {
	if err := os.MkdirAll(r.dir, 0o750); err != nil {
		return fmt.Errorf("runner: create the run directory: %w", err)
	}
	tgt, err := r.startTarget(ctx)
	if err != nil {
		return err
	}
	r.tgt = tgt
	if r.o.OnTarget != nil {
		r.o.OnTarget(tgt)
	}
	if r.c, err = target.NewClient(tgt); err != nil {
		return err
	}
	r.fleet = simworker.NewFleet(simworker.FleetConfig{
		RunID:    r.targetRunID(),
		FarmID:   tgt.FarmID(),
		NATSURL:  tgt.NATSURL(),
		IDPrefix: r.targetRunID(),
		Groups:   r.sc.Workers,
		QueueIDs: tgt.QueueIDs(),
		Creds:    tgt.Credentials(),
		TLS:      tgt.TLS(),
		Seed:     r.seed,
	}, simworker.ScenarioBehavior(taskSpecs(r.sc)), r.ledger, withLevelFloor(r.logger, fleetLogLevel))
	r.logger.InfoContext(ctx, "ramping the simulated fleet", slog.Int("workers", simulatedCount(r.sc)))
	if err := r.fleet.Start(ctx); err != nil {
		return fmt.Errorf("runner: start the simulated fleet: %w", err)
	}
	r.startObserver(ctx)
	r.startUI(ctx)
	return nil
}

// startTarget starts a managed target, building the binaries it was not
// given, or attaches to a running server.
func (r *run) startTarget(ctx context.Context) (target.Target, error) {
	if r.o.Attach != nil {
		cfg := *r.o.Attach
		cfg.Scenario, cfg.RunID, cfg.Logger = r.sc, r.targetRunID(), r.logger
		return target.Attach(ctx, cfg)
	}
	cfg := *r.o.Managed
	cfg.RunDir = filepath.Join(r.dir, "target")
	cfg.RunID, cfg.Scenario, cfg.RealWorkers, cfg.Logger = r.targetRunID(), r.sc, r.sc.Workers, r.logger
	if cfg.ServerBin == "" || (realCount(r.sc) > 0 && (cfg.WorkerBin == "" || cfg.NoopBin == "")) {
		r.logger.InfoContext(ctx, "building sqi binaries")
		server, worker, noop, err := target.BuildBinaries(ctx, filepath.Join(r.o.OutDir, ".bin"))
		if err != nil {
			return nil, err
		}
		cfg.ServerBin = cmpOr(cfg.ServerBin, server)
		cfg.WorkerBin = cmpOr(cfg.WorkerBin, worker)
		cfg.NoopBin = cmpOr(cfg.NoopBin, noop)
	}
	r.logger.InfoContext(ctx, "starting sqi-server", slog.String("store", r.sc.Server.Store))
	return target.StartManaged(ctx, cfg)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// startObserver samples the target until teardown stops it. Its context is
// not the run's, so an interrupt does not take the last sample before the
// fleet has stopped.
func (r *run) startObserver(ctx context.Context) {
	dsn := r.o.PostgresDSN
	if r.o.Attach == nil {
		dsn = target.PostgresDSN(r.tgt)
	}
	r.obs = observe.New(r.tgt, r.recorder, observe.Config{
		Interval:     r.o.Sample,
		ProfileEvery: r.o.ProfileEvery,
		ProfileDir:   filepath.Join(r.dir, "profiles"),
		PostgresDSN:  dsn,
	}, r.logger)
	octx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r.obsCancel, r.obsDone = cancel, make(chan struct{})
	go func() {
		defer close(r.obsDone)
		if err := r.obs.Run(octx); err != nil {
			r.unavail("observer", err.Error())
		}
	}()
}

// startUI runs the WebSocket clients and REST pollers until teardown.
func (r *run) startUI(ctx context.Context) {
	ui := driver.NewUIClients(r.tgt.HTTPURL(), r.tgt.APIKey(), r.sc.Clients, r.jobs, r.metrics,
		driver.WithUITarget(r.tgt), driver.WithUIRand(r.rand(3)))
	uctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r.uiCancel, r.uiDone = cancel, make(chan struct{})
	go func() {
		defer close(r.uiDone)
		if err := ui.Run(uctx); err != nil {
			r.fail(fmt.Errorf("runner: UI clients: %w", err))
		}
	}()
}

// measure submits for the scenario's duration with the actions and churn
// running, then waits for the jobs to drain and checks the invariants. An
// interrupt, or a driver error, ends it early, leaving the run incomplete.
func (r *run) measure(ctx context.Context) {
	sc := r.sc
	r.started = time.Now()
	steadyFrom := r.started.Add(sc.Warmup.Duration)
	end := r.started.Add(sc.Duration.Duration)
	subCtx, cancelSub := context.WithDeadline(ctx, end)
	defer cancelSub()

	var opts []driver.SubmitterOption
	if r.o.Shard.active() {
		opts = append(opts, driver.WithNamePrefix(r.o.Shard.Prefix()))
	}
	sub := driver.NewSubmitter(r.c, r.tgt, sc, r.jobs, r.metrics, r.rand(1), opts...)
	r.actions = driver.NewActions(r.c, r.tgt, sc.Actions, r.jobs, r.fleet, driver.WithActionsRand(r.rand(2)))
	r.logger.InfoContext(ctx, "submitting", slog.Duration("duration", sc.Duration.Duration), slog.Duration("warmup", sc.Warmup.Duration))

	var wg sync.WaitGroup
	wg.Go(func() {
		if err := sub.Run(subCtx); err != nil {
			r.fail(fmt.Errorf("runner: submitter: %w", err))
			cancelSub()
		}
	})
	wg.Go(func() {
		if err := r.actions.Run(subCtx, r.started); err != nil {
			r.fail(fmt.Errorf("runner: actions: %w", err))
			cancelSub()
		}
	})
	wg.Go(func() { r.fleet.RunChurn(subCtx) })
	wg.Go(func() { r.snapshotSteady(subCtx, steadyFrom) })
	wg.Go(func() { r.profileCPU(subCtx, steadyFrom, end) })
	<-subCtx.Done()
	wg.Wait()
	r.ended = time.Now()
	if ctx.Err() != nil || r.failed() {
		return
	}

	r.logger.InfoContext(ctx, "submissions stopped; waiting for the jobs to finish",
		slog.Int("jobs", r.jobs.AcceptedCount()), slog.Duration("drain_timeout", sc.DrainTimeout.Duration))
	dctx, cancel := context.WithTimeout(ctx, sc.DrainTimeout.Duration+drainSlack)
	err := verify.WaitDrained(dctx, r.c, r.tgt.FarmID(), r.jobs.Accepted(), sc.DrainTimeout.Duration)
	cancel()
	r.ended = time.Now()
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		// The terminal invariant reports the jobs left open.
		r.logger.WarnContext(ctx, "not every job finished", slog.Any("error", err))
	}
	r.check(ctx)
}

// snapshotSteady takes the counters at the end of warmup, the base of every
// steady-window rate.
func (r *run) snapshotSteady(ctx context.Context, at time.Time) {
	if !sleepUntil(ctx, at) {
		return
	}
	counters := r.liveCounters()
	jobs, err := listFarmJobs(ctx, r.c, r.tgt.FarmID())
	if err != nil {
		if ctx.Err() == nil {
			r.unavail("steady_counters", "listing the farm's jobs at the end of warmup failed, so rates are run averages: "+err.Error())
		}
		return
	}
	counters[report.CounterTasksTerminal] = terminalTasks(jobs)
	r.mu.Lock()
	r.steady = counters
	r.mu.Unlock()
}

// profileCPU takes one CPU profile of the server centered on the middle of
// the steady window, at most cpuProfileLength long.
func (r *run) profileCPU(ctx context.Context, steadyFrom, end time.Time) {
	window := end.Sub(steadyFrom)
	d := min(cpuProfileLength, window/2)
	if d < minCPUProfile {
		r.unavail("cpu_profile", fmt.Sprintf("the steady window (%s) is too short for a CPU profile", window))
		return
	}
	if !sleepUntil(ctx, steadyFrom.Add(window/2-d/2)) {
		return
	}
	if err := r.obs.CPUProfile(ctx, d); err != nil && ctx.Err() == nil {
		r.logger.WarnContext(ctx, "CPU profile failed", slog.Any("error", err))
	}
}

// check evaluates the invariants with the fleet still up. An interrupt
// during the checks discards their results.
func (r *run) check(ctx context.Context) {
	accepted := r.jobs.Accepted()
	records := r.actions.Records()
	share := sampleShare(r.sc, accepted)
	in := verify.Input{
		Jobs:                accepted,
		Ledger:              r.ledger,
		Scenario:            r.sc,
		FarmID:              r.tgt.FarmID(),
		LiveWorkers:         r.fleet.Live() + realCount(r.sc),
		LogSample:           share,
		LogOverflowExpected: logOverflowExpected(r.sc),
		AttemptSample:       share,
		Seed:                uint64(r.seed), //nolint:gosec // G115: a seed is a bit pattern
		CanceledJobs:        verify.CanceledJobs(records),
		CanceledAt:          verify.CancelTimes(records),
		ConvergeWithin:      convergeWithin(r.sc),
	}
	if r.o.BeforeVerify != nil {
		r.o.BeforeVerify(&in)
	}
	r.logger.InfoContext(ctx, "checking the invariants", slog.Int("jobs", len(accepted)), slog.Float64("sample", share))
	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	results := verify.Check(cctx, r.c, in)
	if ctx.Err() != nil {
		return
	}
	r.invariants = results
	r.complete = true
}

// teardown stops the clients and the fleet, reads what the record needs from
// the server, takes the observer's last sample and closes the target. Each
// step has its own bounded context, so an interrupted run still cleans up.
func (r *run) teardown(ctx context.Context) {
	if r.tornDown {
		return
	}
	r.tornDown = true
	interrupted := ctx.Err() != nil
	if r.uiCancel != nil {
		r.uiCancel()
		// Clients still running could record into a histogram while the
		// record marshals it; record() then leaves theirs out.
		r.uiRunning = !r.wait(ctx, r.uiDone, uiStopTimeout, "UI clients")
	}
	if r.fleet != nil {
		d := fleetStopTimeout
		if interrupted {
			d = interruptedFleetStop
		}
		sctx, cancel := r.cleanupCtx(ctx, d)
		if err := r.fleet.Stop(sctx); err != nil {
			r.logger.WarnContext(ctx, "stopping the simulated fleet", slog.Any("error", err))
		}
		cancel()
	}
	if r.c != nil {
		qctx, cancel := r.cleanupCtx(ctx, serverQueryTimeout)
		r.serverJobs, r.serverErr = listFarmJobs(qctx, r.c, r.tgt.FarmID())
		cancel()
	}
	if r.obsCancel != nil {
		r.obsCancel()
		r.wait(ctx, r.obsDone, observerStopTimeout, "observer")
	}
	if r.tgt != nil {
		cctx, cancel := r.cleanupCtx(ctx, closeTimeout)
		if err := r.tgt.Close(cctx); err != nil {
			r.fail(fmt.Errorf("runner: close the target: %w", err))
		}
		cancel()
	}
}

// emergencyStop is the teardown after a panic: it waits for nothing and
// keeps only the steps that stop processes.
func (r *run) emergencyStop(ctx context.Context) {
	for _, cancel := range []context.CancelFunc{r.uiCancel, r.obsCancel} {
		if cancel != nil {
			cancel()
		}
	}
	if r.fleet != nil {
		sctx, cancel := r.cleanupCtx(ctx, emergencyStop)
		r.fleet.Stop(sctx) //nolint:errcheck // already panicking; stopping is best effort
		cancel()
	}
	if r.tgt != nil {
		cctx, cancel := r.cleanupCtx(ctx, closeTimeout)
		r.tgt.Close(cctx) // already panicking: best effort; Close kills what it cannot stop
		cancel()
	}
}

// cleanupCtx returns a context for one teardown step: not canceled by the
// run's context, ending after d or when Options.Abort closes.
func (r *run) cleanupCtx(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), d)
	if r.o.Abort == nil {
		return c, cancel
	}
	go func() {
		select {
		case <-r.o.Abort:
			cancel()
		case <-c.Done():
		}
	}()
	return c, cancel
}

// wait waits for done, at most d, and reports whether it came.
func (r *run) wait(ctx context.Context, done <-chan struct{}, d time.Duration, what string) bool {
	wctx, cancel := r.cleanupCtx(ctx, d)
	defer cancel()
	select {
	case <-done:
		return true
	case <-wctx.Done():
		r.logger.WarnContext(ctx, "gave up waiting for the "+what+" to stop", slog.Duration("after", d))
		return false
	}
}

// sleepUntil waits until t and reports whether ctx was still live then.
func sleepUntil(ctx context.Context, t time.Time) bool {
	timer := time.NewTimer(time.Until(t))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}
