// SPDX-License-Identifier: AGPL-3.0-or-later

// Package observe samples a load run into time series: the server's /metrics,
// the disk its state occupies, JetStream stream state, pprof profiles, and the
// load generator's own health, which decides whether the run is valid.
//
// Series names are the contract the report reads. A series with a label
// carries it in braces after the name, as name{label}: a queue, a usage pool,
// a JetStream stream (or stream/consumer), or an HTTP route pattern.
//
//	server.cpu_cores   server.rss_bytes   server.goroutines   server.open_fds
//	server.gc_pause_p99_s   server.heap_bytes
//	sched.queue_depth{queue}   sched.idle_workers   workers.online
//	usage.claims{pool}   http.p95_s{route}
//	disk.sqlite_db_bytes   disk.sqlite_wal_bytes   disk.nats_bytes   pg.db_bytes
//	js.bytes{stream}   js.msgs{stream}   js.pending{stream/consumer}
//	gen.cpu_cores   gen.goroutines   gen.sched_lag_p99_s
//
// A measurement that cannot be taken is not an error: it is listed, with the
// reason, by Observer.Unavailable.
package observe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/loadtest/measure"
	"github.com/uberware/sqi/internal/loadtest/target"
)

const (
	// defaultInterval is the sample interval when Config.Interval is unset.
	defaultInterval = 5 * time.Second
	// callTimeout bounds each network call of one sample.
	callTimeout = 10 * time.Second
	// finalSampleTimeout bounds the sample taken after the run's context ends.
	finalSampleTimeout = 30 * time.Second
)

// Measurement names, as they appear in Unavailable.
const (
	mMetrics   = "metrics"
	mDisk      = "disk"
	mJetStream = "jetstream"
	mPprof     = "pprof"
	mPostgres  = "pg.db_bytes"
)

// Config configures an Observer.
type Config struct {
	// Interval is the sampling period; 5 s when zero.
	Interval time.Duration
	// ProfileEvery is the period of heap and goroutine profiles. Zero takes
	// them only at the start and the end of the run.
	ProfileEvery time.Duration
	// ProfileDir is where profiles are written. Empty disables profiling.
	ProfileDir string
	// PostgresDSN, when set, adds pg.db_bytes: the size of the server's
	// PostgreSQL database.
	PostgresDSN string
}

// Observer samples a target into a recorder.
type Observer struct {
	tgt    target.Target
	rec    *measure.Recorder
	cfg    Config
	logger *slog.Logger
	health *SelfHealth

	mu          sync.Mutex
	unavailable map[string]string // measurement -> reason
	// pprofOff is set once the server has said it does not serve pprof.
	pprofOff bool

	prof profiler // set by Run, or lazily by CPUProfile; guarded by mu
}

// New returns an Observer of t that records into rec.
func New(t target.Target, rec *measure.Recorder, cfg Config, logger *slog.Logger) *Observer {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Interval <= 0 {
		cfg.Interval = defaultInterval
	}
	return &Observer{tgt: t, rec: rec, cfg: cfg, logger: logger, health: NewSelfHealth(), unavailable: map[string]string{}}
}

// SelfHealth returns the generator's health record, which Run fills.
func (o *Observer) SelfHealth() *SelfHealth { return o.health }

// Unavailable lists the measurements that could not be taken, each as
// "<measurement>: <reason>", sorted and without duplicates.
func (o *Observer) Unavailable() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]string, 0, len(o.unavailable))
	for m, reason := range o.unavailable {
		out = append(out, m+": "+reason)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func (o *Observer) mark(ctx context.Context, measurement string, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, seen := o.unavailable[measurement]; !seen {
		o.logger.WarnContext(ctx, "observation unavailable", slog.String("measurement", measurement), slog.Any("reason", err))
	}
	o.unavailable[measurement] = err.Error()
}

func (o *Observer) clear(measurement string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.unavailable, measurement)
}

// sampler is the state one Run keeps between samples.
type sampler struct {
	o       *Observer
	scrape  scraper
	state   *scrapeState
	js      *jsReader
	pg      *pgSizer
	gen     *genSampler
	nextDue time.Time // when the next periodic profile is due
}

// Run samples until ctx ends: once immediately, then every Config.Interval,
// then once more after ctx ends. It returns nil when ctx ends, whether or not
// every measurement could be taken (see Unavailable); it returns an error only
// when the observer cannot be set up at all.
func (o *Observer) Run(ctx context.Context) error {
	hc, err := target.NewClient(o.tgt)
	if err != nil {
		return fmt.Errorf("observe: %w", err)
	}
	s := &sampler{
		o:      o,
		scrape: scraper{c: hc, timeout: callTimeout},
		state:  newScrapeState(invert(o.tgt.QueueIDs())),
		js:     &jsReader{url: o.tgt.NATSURL(), files: o.tgt.TLS()},
		gen:    newGenSampler(ctx, o.rec, o.health, lagPeriod),
	}
	if o.cfg.PostgresDSN != "" {
		s.pg = &pgSizer{dsn: o.cfg.PostgresDSN}
	}
	o.setProfiler(profiler{c: hc, dir: o.cfg.ProfileDir, timeout: callTimeout})
	defer s.js.close()
	if s.pg != nil {
		defer s.pg.close()
	}
	defer s.gen.stop()

	if o.tgt.DataDir() == "" {
		o.mark(ctx, mDisk, errors.New("the target does not expose its data directory"))
	}

	s.sample(ctx, "start")
	ticker := time.NewTicker(o.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalSampleTimeout)
			s.sample(final, "end")
			cancel()
			return nil
		case <-ticker.C:
			s.sample(ctx, "periodic")
		}
	}
}

// invert maps queue names by id.
func invert(ids map[string]string) map[string]string {
	out := make(map[string]string, len(ids))
	for name, id := range ids {
		out[id] = name
	}
	return out
}

// sample takes every measurement once. phase is "start", "periodic" or "end".
func (s *sampler) sample(ctx context.Context, phase string) {
	// The generator is sampled first, and stamps itself: its CPU rate is a
	// difference of two reads, and the network calls below can take seconds.
	s.gen.sample()
	t := time.Now()
	s.sampleMetrics(ctx, t)
	s.sampleDisk(ctx, t)
	s.samplePostgres(ctx, t)
	s.sampleJetStream(ctx, t)
	s.sampleProfiles(ctx, t, phase)
}

// failed records err against measurement unless the run's own cancellation
// caused it.
func (s *sampler) failed(ctx context.Context, measurement string, err error) {
	if ctx.Err() != nil {
		return
	}
	s.o.mark(ctx, measurement, err)
}

func (s *sampler) sampleMetrics(ctx context.Context, t time.Time) {
	body, err := s.scrape.fetch(ctx)
	if err == nil {
		err = s.state.apply(s.o.rec, t, body)
	}
	if err != nil {
		s.failed(ctx, mMetrics, fmt.Errorf("scrape failed: %w", err))
		return
	}
	s.o.clear(mMetrics)
}

func (s *sampler) sampleDisk(ctx context.Context, t time.Time) {
	dir := s.o.tgt.DataDir()
	if dir == "" {
		return
	}
	if err := sampleDisk(s.o.rec, t, dir); err != nil {
		s.o.mark(ctx, mDisk, err)
		return
	}
	s.o.clear(mDisk)
}

func (s *sampler) samplePostgres(ctx context.Context, t time.Time) {
	if s.pg == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	n, err := s.pg.size(cctx)
	if err != nil {
		s.failed(ctx, mPostgres, err)
		return
	}
	s.o.rec.Add("pg.db_bytes", "bytes", t, n)
	s.o.clear(mPostgres)
}

func (s *sampler) sampleJetStream(ctx context.Context, t time.Time) {
	if err := s.js.connect(); err != nil {
		s.failed(ctx, mJetStream, err)
		return
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	failures := s.js.sample(cctx, s.o.rec, t)
	if len(failures) == 0 {
		s.o.clear(mJetStream)
		return
	}
	var errs []error
	for _, name := range jsStreams {
		if err := failures[name]; err != nil {
			errs = append(errs, err)
		}
	}
	s.failed(ctx, mJetStream, errors.Join(errs...))
}

// sampleProfiles takes heap and goroutine profiles at the start and end of the
// run and, with ProfileEvery set, periodically.
func (s *sampler) sampleProfiles(ctx context.Context, t time.Time, phase string) {
	o := s.o
	if o.cfg.ProfileDir == "" || o.pprofUnavailable() {
		return
	}
	switch phase {
	case "start":
		if o.cfg.ProfileEvery > 0 {
			s.nextDue = t.Add(o.cfg.ProfileEvery)
		}
	case "periodic":
		if o.cfg.ProfileEvery <= 0 || t.Before(s.nextDue) {
			return
		}
		s.nextDue = t.Add(o.cfg.ProfileEvery)
	}
	if err := o.profiler().snapshot(ctx, t, phase); err != nil {
		o.profileFailed(ctx, err)
	}
}

func (o *Observer) setProfiler(p profiler) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.prof = p
}

func (o *Observer) profiler() profiler {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.prof
}

func (o *Observer) pprofUnavailable() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.pprofOff
}

// profileFailed records err. A server that does not serve pprof is recorded
// once and left alone; any other failure is logged and retried at the next
// profile.
func (o *Observer) profileFailed(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	if errors.Is(err, errPprofUnavailable) {
		o.mu.Lock()
		o.pprofOff = true
		o.mu.Unlock()
		o.mark(ctx, mPprof, err)
		return
	}
	o.logger.WarnContext(ctx, "profile failed", slog.Any("error", err))
}

// CPUProfile fetches a CPU profile of d from the server into Config.ProfileDir.
// It blocks for d. A server without pprof is recorded as unavailable and
// returned as an error.
func (o *Observer) CPUProfile(ctx context.Context, d time.Duration) error {
	if o.cfg.ProfileDir == "" {
		return errors.New("observe: no profile directory configured")
	}
	if o.pprofUnavailable() {
		return errPprofUnavailable
	}
	p := o.profiler()
	if p.c == nil {
		hc, err := target.NewClient(o.tgt)
		if err != nil {
			return fmt.Errorf("observe: %w", err)
		}
		p = profiler{c: hc, dir: o.cfg.ProfileDir, timeout: callTimeout}
		o.setProfiler(p)
	}
	err := p.cpu(ctx, time.Now(), d)
	if err != nil {
		o.profileFailed(ctx, err)
		return fmt.Errorf("observe: CPU profile: %w", err)
	}
	return nil
}
