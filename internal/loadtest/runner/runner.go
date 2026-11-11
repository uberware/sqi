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
// A chaos run (a scenario with faults, a rework budget, known violations or
// real tasks, on a target that can inject faults) also fires the scenario's
// faults beside the submissions, waits for every one to heal before the
// drain, waits for the jobs the server created for submissions it gave no
// 2xx answer, and checks submission-atomic with the six invariants.
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
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/loadtest/internal/sleep"

	"github.com/uberware/sqi/internal/loadtest/chaos"
	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/driver"
	"github.com/uberware/sqi/internal/loadtest/marker"
	"github.com/uberware/sqi/internal/loadtest/measure"
	"github.com/uberware/sqi/internal/loadtest/observe"
	"github.com/uberware/sqi/internal/loadtest/report"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
	"github.com/uberware/sqi/internal/loadtest/target"
	"github.com/uberware/sqi/internal/loadtest/verify"
	"github.com/uberware/sqi/internal/store"
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
	// emergencyStop bounds the fleet's stop in the teardown after a panic;
	// closing the target there is bounded by closeTimeout.
	emergencyStop = 10 * time.Second
	// storeCheckTimeout bounds stopping a chaos run's target for the store
	// checks (the same graces as closeTimeout) and the checks themselves.
	storeCheckTimeout = 15 * time.Minute

	// checkTimeout bounds the invariant checks.
	checkTimeout = 30 * time.Minute
	// drainSlack is added to the scenario's drain timeout for the context of
	// WaitDrained, which applies the timeout itself.
	drainSlack = time.Minute
	// faultHealTimeout bounds how long after the end of submissions the
	// faults may take to heal: a server restart waits for /readyz.
	faultHealTimeout = 5 * time.Minute
)

// Options configures a run.
type Options struct {
	// Scenario is the resolved scenario, overrides applied and defaults
	// filled. Normalize validates it for the target.
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
	// Shard runs one slice of the scenario (see report.ShardSpec.Apply).
	Shard report.ShardSpec
	// AllowSaturated keeps an invalid run (a saturated generator) from
	// counting against it: the record stays invalid and gains a warning
	// finding. The CLI uses it to choose its exit status.
	AllowSaturated bool
	// Managed starts the run's own sqi-server; Attach drives one somebody
	// else runs. Neither set means a managed target with defaults. Run fills
	// RunDir, RunID, Scenario and Logger itself, and builds the binaries
	// Managed does not name into OutDir/.bin.
	Managed *target.ManagedConfig
	Attach  *target.AttachConfig
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

	// validated is the scenario and target kind Normalize last validated.
	validated validation

	// Test seams; nil means the real thing: starting the target, making the
	// fault controller, and waiting for the jobs to drain.
	startTarget   func(ctx context.Context, r *run) (target.Target, error)
	newController func(faults []scenario.Fault, groups []scenario.WorkerGroup, t target.Faults, fleet chaos.Fleet, logger *slog.Logger) (faultController, error)
	waitDrained   func(ctx context.Context, v verify.ServerView, farmID string, jobs []driver.SubmittedJob, timeout time.Duration) error
}

// faultController fires the scenario's faults. *chaos.Controller implements
// it.
type faultController interface {
	Run(ctx context.Context, start time.Time) error
	Records() []report.FaultRecord
	Windows() []verify.FaultWindow
	HealAll()
}

func newChaosController(faults []scenario.Fault, groups []scenario.WorkerGroup, t target.Faults, fleet chaos.Fleet, logger *slog.Logger) (faultController, error) {
	c, err := chaos.NewController(faults, groups, t, fleet, logger)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// validation is a scenario validated for a kind of target.
type validation struct {
	sc   *scenario.Scenario
	kind scenario.TargetKind
}

// Normalize fills in what the options leave to Run (a managed target when
// neither Managed nor Attach is set, the mode, run ID, output directory,
// seed, and the sample and profile periods), rejects options that do not go
// together, and validates the scenario for the target. Run and Sweep call it;
// a caller that wants the run ID, seed or directory before the run starts
// calls it first. It is idempotent, and does not validate a scenario again
// that it validated for the same target, so the scenario must not be changed
// in place once it has.
func (o *Options) Normalize() error {
	switch {
	case o.Scenario == nil:
		return errors.New("runner: no scenario")
	case o.Managed != nil && o.Attach != nil:
		return errors.New("runner: a run is either managed or attached, not both")
	case o.Managed == nil && o.Attach == nil:
		o.Managed = &target.ManagedConfig{}
	}
	if err := o.fill(); err != nil {
		return err
	}
	v := validation{sc: o.Scenario, kind: o.TargetKind()}
	if o.validated == v {
		return nil
	}
	if err := o.Scenario.Validate(v.kind); err != nil {
		return fmt.Errorf("runner: scenario %s: %w", o.Scenario.Name, err)
	}
	o.validated = v
	return nil
}

// TargetKind is the kind of target the options run on.
func (o *Options) TargetKind() scenario.TargetKind {
	if o.Attach != nil {
		return scenario.TargetAttach
	}
	return scenario.TargetManaged
}

// fill fills and checks the options besides the target and the scenario.
func (o *Options) fill() error {
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
	if err := o.checkShard(); err != nil {
		return err
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
	// A negative ProfileEvery stays negative, so a second fill keeps it.
	if o.ProfileEvery == 0 && o.Mode == report.ModeSoak {
		o.ProfileEvery = DefaultSoakProfileEvery
	}
	return nil
}

// checkShard rejects a shard out of range, and a shard of a managed run: the
// shards of a run drive one shared server, and merge adds up their counters.
func (o *Options) checkShard() error {
	switch {
	case !o.Shard.Active():
		return nil
	case o.Shard.Index < 0 || o.Shard.Index >= o.Shard.Of:
		return fmt.Errorf("runner: shard %d/%d is out of range", o.Shard.Index, o.Shard.Of)
	case o.Attach == nil:
		return errors.New("runner: the shards of a run drive one shared server, so a shard must attach to it")
	}
	return nil
}

// Directory is the run directory the options name.
func (o *Options) Directory() string {
	if o.Dir != "" {
		return o.Dir
	}
	return filepath.Join(o.OutDir, o.shardRunID())
}

// shardRunID is the run ID with the shard's -s<i> suffix, for a shard of
// several: it names the shard's directory, farm and worker hostnames, so that
// the shards never collide.
func (o *Options) shardRunID() string {
	if o.Shard.Active() {
		return o.RunID + "-" + o.Shard.Prefix()
	}
	return o.RunID
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

	// faults is the target's fault injection in a chaos run (see
	// chaosScenario), nil otherwise. ctl fires the scenario's faults, nil
	// when it has none; cancelFaults ends its Run.
	faults       target.Faults
	ctl          faultController
	cancelFaults context.CancelFunc
	// serverUp is when the target had started, which bounds when its server
	// last connected to the broker; orphans are the jobs the server created
	// for submissions it gave no 2xx answer, in a chaos run.
	serverUp time.Time
	orphans  []driver.SubmittedJob
	// rework is what the stopped server's store says the faults cost, nil
	// until measured.
	rework *report.Rework

	uiCancel, obsCancel context.CancelFunc
	uiDone, obsDone     chan struct{}

	began, started, ended time.Time
	// windowEnd is when submissions stopped, and window the counters then.
	windowEnd  time.Time
	window     map[string]int64
	backlog    *report.Backlog
	complete   bool
	tornDown   bool
	uiRunning  bool // the UI clients did not stop in time
	invariants []verify.Result
	serverJobs map[string]client.Job
	serverErr  error

	mu sync.Mutex // guards the fields below
	// tailAt is the start of the window's last tenth, and tailTerminal the
	// farm's terminal tasks then; tailOK says it was read.
	tailAt       time.Time
	tailTerminal int64
	tailOK       bool
	// tasks are the tasks the invariant checks listed, by ID.
	tasks       map[string]taskEnd
	steady      map[string]int64
	unavailable []string
	errs        []error
}

func newRun(o Options) (*run, error) {
	if err := o.Normalize(); err != nil {
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
	start := r.startTarget
	if r.o.startTarget != nil {
		start = func(ctx context.Context) (target.Target, error) { return r.o.startTarget(ctx, r) }
	}
	tgt, err := start(ctx)
	if err != nil {
		return err
	}
	r.tgt, r.serverUp = tgt, time.Now()
	if r.o.OnTarget != nil {
		r.o.OnTarget(tgt)
	}
	if err := r.useFaults(); err != nil {
		return err
	}
	if r.c, err = target.NewClient(tgt); err != nil {
		return err
	}
	fc := simworker.FleetConfig{
		RunID:    r.o.shardRunID(),
		FarmID:   tgt.FarmID(),
		NATSURL:  tgt.NATSURL(),
		IDPrefix: r.o.shardRunID(),
		Groups:   r.sc.Workers,
		QueueIDs: tgt.QueueIDs(),
		Creds:    tgt.Credentials(),
		TLS:      tgt.TLS(),
		Seed:     r.seed,
	}
	if lanes, ok := tgt.(interface{ NATSURLFor(group string) string }); ok && len(r.sc.Faults) > 0 {
		// Each group dials its own broker lane, which a network fault acts on.
		fc.NATSURLFor = lanes.NATSURLFor
	}
	r.fleet = simworker.NewFleet(fc, simworker.ScenarioBehavior(taskSpecs(r.sc)), r.ledger, withLevelFloor(r.logger, fleetLogLevel))
	if err := r.makeController(); err != nil {
		return err
	}
	simulated, _ := r.sc.WorkerCounts()
	r.logger.InfoContext(ctx, "ramping the simulated fleet", slog.Int("workers", simulated))
	if err := r.fleet.Start(ctx); err != nil {
		return fmt.Errorf("runner: start the simulated fleet: %w", err)
	}
	r.startObserver(ctx)
	r.startUI(ctx)
	return nil
}

// useFaults puts the run in chaos mode when its scenario is a chaos one and
// its target can inject faults, and refuses a scenario with faults whose
// target cannot.
func (r *run) useFaults() error {
	if f, ok := r.tgt.(target.Faults); ok && chaosScenario(r.sc) {
		r.faults = f
	}
	if len(r.sc.Faults) > 0 && r.faults == nil {
		return fmt.Errorf("runner: scenario %s has faults, but its target cannot inject them (only a managed target can)", r.sc.Name)
	}
	return nil
}

// makeController makes the controller of the scenario's faults. The target
// has started its broker lanes by now, which the controller looks up once.
func (r *run) makeController() error {
	if len(r.sc.Faults) == 0 {
		return nil
	}
	newCtl := newChaosController
	if r.o.newController != nil {
		newCtl = r.o.newController
	}
	ctl, err := newCtl(r.sc.Faults, r.sc.Workers, r.faults, r.fleet, r.logger)
	if err != nil {
		return fmt.Errorf("runner: %w", err)
	}
	r.ctl = ctl
	return nil
}

// startTarget starts a managed target, building the binaries it was not
// given, or attaches to a running server.
func (r *run) startTarget(ctx context.Context) (target.Target, error) {
	if r.o.Attach != nil {
		if set := serverSettings(r.sc); len(set) > 0 {
			r.logger.WarnContext(ctx, "the scenario's server settings do not apply to an attached server, which keeps its own configuration",
				slog.Any("ignored", set))
		}
		cfg := *r.o.Attach
		cfg.Scenario, cfg.RunID, cfg.Logger = r.sc, r.o.shardRunID(), r.logger
		// The shards of one run share its usage pools, named for the run.
		cfg.PoolRunID = r.o.RunID
		cfg.SharedPools = cfg.SharedPools || r.o.Shard.Active()
		return target.Attach(ctx, cfg)
	}
	cfg := *r.o.Managed
	cfg.RunDir = filepath.Join(r.dir, "target")
	cfg.RunID, cfg.Scenario, cfg.Logger = r.o.shardRunID(), r.sc, r.logger
	if cfg.Seed == 0 {
		cfg.Seed = uint64(r.seed) //nolint:gosec // G115: a seed is a bit pattern
	}
	_, procs := r.sc.WorkerCounts()
	if err := cfg.ResolveBinaries(ctx, buildDir(r.o.OutDir), procs > 0); err != nil {
		return nil, err
	}
	r.logger.InfoContext(ctx, "starting sqi-server", slog.String("store", r.sc.Server.Store))
	return target.StartManaged(ctx, cfg)
}

// buildDir is where a run, or a sweep, builds the binaries it was not given.
func buildDir(outDir string) string { return filepath.Join(outDir, ".bin") }

// startObserver samples the target until teardown stops it. Its context is
// not the run's, so an interrupt does not take the last sample before the
// fleet has stopped.
func (r *run) startObserver(ctx context.Context) {
	r.obs = observe.New(r.tgt, r.recorder, observe.Config{
		Interval:     r.o.Sample,
		ProfileEvery: max(r.o.ProfileEvery, 0),
		ProfileDir:   filepath.Join(r.dir, "profiles"),
		PostgresDSN:  r.tgt.PostgresDSN(),
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
	ui := driver.NewUIClients(r.c, r.tgt, r.sc.Clients, r.jobs, r.metrics, driver.WithUIRand(r.rand(3)))
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
	if r.o.Shard.Active() {
		opts = append(opts, driver.WithNamePrefix(r.o.Shard.Prefix()))
	}
	sub := driver.NewSubmitter(r.c, r.tgt, sc, r.jobs, r.metrics, r.rand(1), opts...)
	r.actions = driver.NewActions(r.c, r.tgt, sc.Actions, r.jobs, r.fleet, driver.WithActionsRand(r.rand(2)))
	r.logger.InfoContext(ctx, "submitting", slog.Duration("duration", sc.Duration.Duration), slog.Duration("warmup", sc.Warmup.Duration))

	// The faults run on a context of their own, which the end of submissions
	// does not cancel: a fault still healing then (a server restart, say)
	// finishes, and the drain below starts only once every fault has healed.
	// An interrupt or a driver failure cancels it.
	faultCtx, cancelFaults := context.WithDeadline(ctx, end.Add(faultHealTimeout))
	defer cancelFaults()
	r.cancelFaults = cancelFaults
	stop := func() {
		cancelSub()
		cancelFaults()
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		if err := sub.Run(subCtx); err != nil {
			r.fail(fmt.Errorf("runner: submitter: %w", err))
			stop()
		}
	})
	wg.Go(func() {
		if err := r.actions.Run(subCtx, r.started); err != nil {
			r.fail(fmt.Errorf("runner: actions: %w", err))
			stop()
		}
	})
	if r.ctl != nil {
		wg.Go(func() { r.runFaults(ctx, faultCtx, cancelSub) })
	}
	wg.Go(func() { r.fleet.RunChurn(subCtx) })
	wg.Go(func() { r.snapshotSteady(subCtx, steadyFrom) })
	wg.Go(func() { r.snapshotTail(subCtx, end.Add(-end.Sub(steadyFrom)/10)) })
	wg.Go(func() { r.profileCPU(subCtx, steadyFrom, end) })
	<-subCtx.Done()
	// The fault controller has returned once wg.Wait does, so every fault
	// has healed before the window snapshot and the drain.
	wg.Wait()
	// Submissions stop at end; one in flight then is still the window's.
	r.windowEnd = time.Now()
	if r.windowEnd.After(end) {
		r.windowEnd = end
	}
	r.ended = time.Now()
	if ctx.Err() != nil || r.failed() {
		return
	}
	r.snapshotWindow(ctx)
	if resumed := r.actions.ResumePaused(ctx); len(resumed) > 0 {
		r.logger.InfoContext(ctx, "resumed the queues the run paused, so their jobs can finish", slog.Any("queues", resumed))
	}

	r.findOrphans(ctx)

	r.logger.InfoContext(ctx, "submissions stopped; waiting for the jobs to finish",
		slog.Int("jobs", r.jobs.AcceptedCount()), slog.Int("orphans", len(r.orphans)), slog.Duration("drain_timeout", sc.DrainTimeout.Duration))
	wait := verify.WaitDrained
	if r.o.waitDrained != nil {
		wait = r.o.waitDrained
	}
	dctx, cancel := context.WithTimeout(ctx, sc.DrainTimeout.Duration+drainSlack)
	err := wait(dctx, r.c.RetryingReads(), r.tgt.FarmID(), append(r.jobs.Accepted(), r.orphans...), sc.DrainTimeout.Duration)
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

// runFaults runs the fault controller until every fault has healed. A fault
// failing fails the run and ends submissions; an error caused by the run's
// interrupt, or by the end of faultCtx after another failure, does not.
func (r *run) runFaults(ctx, faultCtx context.Context, cancelSub context.CancelFunc) {
	err := r.ctl.Run(faultCtx, r.started)
	switch {
	case err == nil:
	case ctx.Err() != nil || errors.Is(err, context.Canceled):
		r.logger.WarnContext(ctx, "faults interrupted before they healed", slog.Any("error", err))
	default:
		r.fail(fmt.Errorf("runner: faults: %w", err))
		cancelSub()
	}
}

// findOrphans lists the farm's jobs, in a chaos run, and keeps each one the
// run got no 2xx answer for as an orphan of its submission (see orphanJobs).
// A listing that fails keeps the orphans found before, if any.
func (r *run) findOrphans(ctx context.Context) {
	if r.faults == nil {
		return
	}
	if r.orphans == nil {
		r.orphans = []driver.SubmittedJob{}
	}
	jobs, err := listFarmJobs(ctx, r.c.RetryingReads(), r.tgt.FarmID())
	if err != nil {
		if ctx.Err() == nil {
			r.logger.WarnContext(ctx, "listing the farm's jobs for orphaned submissions failed", slog.Any("error", err))
		}
		return
	}
	r.orphans = orphanJobs(jobs, r.jobs.Accepted(), r.jobs.Rejected())
	if len(r.orphans) > 0 {
		r.logger.InfoContext(ctx, "the server created jobs for submissions it gave no 2xx answer", slog.Int("orphans", len(r.orphans)))
	}
}

// snapshotSteady takes the counters at the end of warmup, the base of every
// steady-window rate.
func (r *run) snapshotSteady(ctx context.Context, at time.Time) {
	if !sleep.Until(ctx, at) {
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

// snapshotWindow takes the counters at the end of submissions, the end of the
// measured window: every rate is taken over the window, not over the drain
// that follows it.
func (r *run) snapshotWindow(ctx context.Context) {
	counters := r.liveCounters()
	jobs, err := listFarmJobs(ctx, r.c, r.tgt.FarmID())
	if err != nil {
		if ctx.Err() == nil {
			r.unavail("window_counters", "listing the farm's jobs when submissions stopped failed, so tasks.terminal is rated at its end-of-run value: "+err.Error())
		}
		r.window = counters
		return
	}
	counters[report.CounterTasksTerminal] = terminalTasks(jobs)
	r.window = counters
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tailOK {
		r.backlog = &report.Backlog{
			Open:          allTasks(jobs) - counters[report.CounterTasksTerminal],
			TailCompleted: counters[report.CounterTasksTerminal] - r.tailTerminal,
			TailFrom:      r.tailAt,
		}
	}
}

// snapshotTail counts the farm's terminal tasks at the start of the window's
// last tenth, the base of how much the server finished just before
// submissions stopped.
func (r *run) snapshotTail(ctx context.Context, at time.Time) {
	if !sleep.Until(ctx, at) {
		return
	}
	jobs, err := listFarmJobs(ctx, r.c, r.tgt.FarmID())
	if err != nil {
		if ctx.Err() == nil {
			r.unavail("backlog", "listing the farm's jobs near the end of the window failed: "+err.Error())
		}
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tailAt, r.tailTerminal, r.tailOK = at, terminalTasks(jobs), true
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
	if !sleep.Until(ctx, steadyFrom.Add(window/2-d/2)) {
		return
	}
	if err := r.obs.CPUProfile(ctx, d); err != nil && ctx.Err() == nil {
		r.logger.WarnContext(ctx, "CPU profile failed", slog.Any("error", err))
	}
}

// check evaluates the invariants with the fleet still up. An interrupt
// during the checks discards their results.
func (r *run) check(ctx context.Context) {
	// A submission the client gave up on may have created its job since the
	// drain began.
	r.findOrphans(ctx)
	accepted := r.jobs.Accepted()
	records := r.actions.Records()
	share := sampleShare(r.sc, accepted)
	_, procs := r.sc.WorkerCounts()
	in := verify.Input{
		Jobs:                accepted,
		Ledger:              r.ledger,
		Scenario:            r.sc,
		PoolNames:           poolNames(r.tgt, r.sc),
		FarmID:              r.tgt.FarmID(),
		LiveWorkers:         r.fleet.Live() + procs,
		LogSample:           share,
		LogOverflowExpected: logOverflowExpected(r.sc),
		AttemptSample:       share,
		Seed:                uint64(r.seed), //nolint:gosec // G115: a seed is a bit pattern
		CanceledAt:          verify.CancelTimes(records),
		ConvergeWithin:      convergeWithin(r.sc),
		Simulated:           r.fleet.Simulates,
		TaskSeen:            r.taskSeen,
	}
	r.chaosInput(&in)
	if r.o.BeforeVerify != nil {
		r.o.BeforeVerify(&in)
	}
	r.logger.InfoContext(ctx, "checking the invariants", slog.Int("jobs", len(accepted)), slog.Float64("sample", share))
	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	results := verify.Check(cctx, r.c.RetryingReads(), in)
	if ctx.Err() != nil {
		return
	}
	results = append(results, r.markerChecks()...)
	r.invariants = results
	r.complete = true
}

// markerChecks runs a chaos run's no-orphans and single-writer checks on what
// the real tasks' processes wrote, with the real workers still up: none when
// the run had no real task. Markers that cannot be read fail the check that
// needs them.
func (r *run) markerChecks() []verify.Result {
	if r.faults == nil || r.faults.MarkerDir() == "" {
		return nil
	}
	dir := r.faults.MarkerDir()
	execs, skipped, err := marker.ReadAll(dir)
	if err != nil {
		msg := "read the real tasks' markers: " + err.Error()
		return []verify.Result{
			{Name: chaos.NameNoOrphans, Evidence: []string{msg}},
			{Name: chaos.NameSingleWriter, Evidence: []string{msg}},
		}
	}
	outputs, outErr := marker.Outputs(dir)
	results := chaos.MarkerChecks(execs, skipped, outputs, r.faults.Incarnations())
	if outErr != nil {
		results[1] = verify.Result{Name: chaos.NameSingleWriter, Evidence: []string{"read the real tasks' outputs: " + outErr.Error()}}
	}
	return results
}

// chaosInput fills what a chaos run adds to the verifier's input: the
// orphans (never nil, so submission-atomic runs), the fault windows, the
// workers' groups, the real workers that are running now rather than the
// scenario's count, and a convergence time that allows for the server's
// liveness grace after its last start.
func (r *run) chaosInput(in *verify.Input) {
	if r.faults == nil {
		return
	}
	in.Orphans = append([]driver.SubmittedJob{}, r.orphans...)
	in.GroupOf = r.fleet.GroupOf
	in.LiveWorkers = r.fleet.Live() + r.faults.RealLive()
	var records []report.FaultRecord
	if r.ctl != nil {
		in.FaultWindows = r.ctl.Windows()
		records = r.ctl.Records()
	}
	in.ConvergeWithin = chaosConvergeWithin(r.sc, r.serverUp, records, time.Now())
}

// taskSeen keeps what terminal_ingest needs of a task the invariant checks
// listed.
func (r *run) taskSeen(_ string, t client.Task) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tasks == nil {
		r.tasks = map[string]taskEnd{}
	}
	r.tasks[t.ID] = taskEnd{status: t.Status, updatedAt: t.UpdatedAt}
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
	r.healFaults()
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
		if !r.wait(ctx, r.obsDone, observerStopTimeout, "observer") {
			r.unavail("observer", "its last sample and end-of-run profiles did not finish (timed out or aborted), so the record may lack them")
		}
	}
	r.storeChecks(ctx)
	r.applyExpectations()
	if r.tgt != nil {
		cctx, cancel := r.cleanupCtx(ctx, closeTimeout)
		if err := r.tgt.Close(cctx); err != nil {
			r.fail(fmt.Errorf("runner: close the target: %w", err))
		}
		cancel()
	}
}

// applyExpectations applies the scenario's rework bound and known violations
// to a chaos run's invariants. A run that ended before its invariants were
// checked has none to apply them to.
func (r *run) applyExpectations() {
	if r.faults == nil || !r.complete {
		return
	}
	r.invariants = chaos.Apply(r.invariants, r.rework, r.sc.Expect, runtime.GOOS)
}

// storeChecks stops a chaos run's real workers and server, once its
// invariants are checked, and appends the checks of the stopped server's
// store to them. A store that cannot be stopped, opened or read fails the
// checks; it is not a run error.
func (r *run) storeChecks(ctx context.Context) {
	if r.faults == nil || !r.complete {
		return
	}
	sctx, cancel := r.cleanupCtx(ctx, storeCheckTimeout)
	defer cancel()
	r.logger.InfoContext(ctx, "stopping the server and the real workers to check the store")
	if err := r.faults.StopForChecks(sctx); err != nil {
		// A server still running would write under the checks.
		msg := "the server and the real workers did not stop for the store checks: " + err.Error()
		r.unavail("rework", msg)
		r.invariants = append(r.invariants,
			verify.Result{Name: chaos.NameStoreIntegrity, Evidence: []string{msg}},
			verify.Result{Name: chaos.NameExecutionsRecorded, Evidence: []string{msg}})
		return
	}
	markers, skipped, markerErr := mainMarkers(r.faults.MarkerDir())
	results := chaos.StoreChecks(sctx, chaos.StoreInput{
		Options:        r.faults.StoreOptions(),
		FarmID:         r.tgt.FarmID(),
		Ledger:         r.ledger,
		Markers:        markers,
		SkippedMarkers: skipped,
		WithStore: func(ctx context.Context, st store.Store) {
			r.measureRework(ctx, st, markers, markerErr)
		},
	})
	if r.rework == nil {
		r.unavail("rework", "the stopped server's store could not be read")
	}
	for i := range results {
		if markerErr != nil && results[i].Name == chaos.NameExecutionsRecorded {
			results[i].OK = false
			results[i].Evidence = append(results[i].Evidence, markerErr.Error())
		}
	}
	r.invariants = append(r.invariants, results...)
}

// measureRework records what the faults cost, from the stopped server's store.
func (r *run) measureRework(ctx context.Context, st store.Store, markers []marker.Exec, markerErr error) {
	var faults []report.FaultRecord
	if r.ctl != nil {
		faults = r.ctl.Records()
	}
	rw, err := chaos.ComputeRework(ctx, st, r.ledger, markers, r.tgt.FarmID(), faults)
	if err != nil {
		r.unavail("rework", err.Error())
		return
	}
	if markerErr != nil {
		r.unavail("rework", "the real tasks' executions are not counted: "+markerErr.Error())
	}
	r.rework = &rw
}

// mainMarkers reads the real tasks' main executions from dir, none when dir
// is "", and how many exec files it could not parse.
func mainMarkers(dir string) (execs []marker.Exec, skipped int, err error) {
	if dir == "" {
		return nil, 0, nil
	}
	execs, skipped, err = marker.ReadAll(dir)
	if err != nil {
		return nil, 0, fmt.Errorf("read the real tasks' markers: %w", err)
	}
	return slices.DeleteFunc(execs, func(e marker.Exec) bool { return e.Role != marker.RoleMain }), skipped, nil
}

// healFaults ends the fault controller's Run, if it is still running, then
// heals every lane and resumes every stalled process, before teardown stops
// anything. Run must end first: HealAll does not stop a kill or term restart
// in progress.
func (r *run) healFaults() {
	if r.cancelFaults != nil {
		r.cancelFaults()
	}
	if r.ctl != nil {
		r.ctl.HealAll()
	}
}

// emergencyStop is the teardown after a panic: it cancels the UI clients and
// the observer without waiting for them, and keeps only the steps that stop
// processes.
func (r *run) emergencyStop(ctx context.Context) {
	r.healFaults()
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
