// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/backend"
	"github.com/uberware/sqi/internal/store/fake"
)

// closeObservingStore records, as it is closed, whether the scheduler had
// returned by then, and whether its capacity calls had been abandoned.
type closeObservingStore struct {
	store.Store

	schedDone      <-chan struct{}
	abandoned      atomic.Bool // set by the server's abandonCalls seam
	closed         atomic.Bool
	afterRun       atomic.Bool
	afterAbandoned atomic.Bool
}

func (c *closeObservingStore) Close() error {
	select {
	case <-c.schedDone:
		c.afterRun.Store(true)
	default:
	}
	c.afterAbandoned.Store(c.abandoned.Load())
	c.closed.Store(true)
	return c.Store.Close()
}

// observedServer is a server with no component but a store that observes its
// close and a scheduler stand-in returning delay after it is stopped (never,
// when delay is negative).
func observedServer(t *testing.T, delay time.Duration, warns *warnCounter) (*Server, *closeObservingStore) {
	t.Helper()
	cancel, done := stoppingScheduler(delay)
	t.Cleanup(cancel)
	st := &closeObservingStore{Store: fake.New(), schedDone: done}
	s := &Server{
		logger: slog.New(warns), store: st, metrics: metrics.New(),
		cancelSched: cancel, schedDone: done,
		abandonCalls: func() { st.abandoned.Store(true) },
	}
	return s, st
}

// warnCounter counts the Warn records logged through it.
type warnCounter struct {
	mu    sync.Mutex
	warns []string
}

func (*warnCounter) Enabled(context.Context, slog.Level) bool { return true }

func (w *warnCounter) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn {
		w.mu.Lock()
		w.warns = append(w.warns, r.Message)
		w.mu.Unlock()
	}
	return nil
}

func (w *warnCounter) WithAttrs([]slog.Attr) slog.Handler { return w }
func (w *warnCounter) WithGroup(string) slog.Handler      { return w }

func (w *warnCounter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.warns)
}

// stoppingScheduler stands in for a running scheduler.Run: cancel ends it,
// and it returns after delay (never, when delay is negative).
func stoppingScheduler(delay time.Duration) (context.CancelFunc, chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	if delay >= 0 {
		go func() {
			<-ctx.Done()
			time.Sleep(delay)
			close(done)
		}()
	}
	return cancel, done
}

// TestShutdown_WaitsForTheSchedulerBeforeClosingTheStore: the scheduler's
// shutdown drains capacity provider calls, which write to the store, so the
// store must not close until scheduler.Run has returned. A scheduler that
// outlasts the ceiling is logged once, its capacity calls are abandoned
// before the store closes, and the shutdown goes on.
func TestShutdown_WaitsForTheSchedulerBeforeClosingTheStore(t *testing.T) {
	tests := []struct {
		name          string
		delay         time.Duration // how long Run takes to return once canceled; <0 never
		ceiling       time.Duration
		wantAfter     bool
		wantAbandoned bool
		wantWarns     int
	}{
		{"a scheduler that stops in time", 300 * time.Millisecond, 10 * time.Second, true, false, 0},
		{"a scheduler that outlasts the ceiling", -1, 200 * time.Millisecond, false, true, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			warns := &warnCounter{}
			s, st := observedServer(t, tc.delay, warns)
			s.schedStopCeiling = tc.ceiling

			start := time.Now()
			if err := s.shutdown(); err != nil {
				t.Fatalf("shutdown = %v", err)
			}
			if took := time.Since(start); took > 5*time.Second {
				t.Errorf("shutdown took %v", took)
			}
			if !st.closed.Load() {
				t.Fatal("the store was not closed")
			}
			if got := st.afterRun.Load(); got != tc.wantAfter {
				t.Errorf("store closed after the scheduler returned = %v, want %v", got, tc.wantAfter)
			}
			if got := st.afterAbandoned.Load(); got != tc.wantAbandoned {
				t.Errorf("capacity calls abandoned before the store closed = %v, want %v", got, tc.wantAbandoned)
			}
			if got := warns.count(); got != tc.wantWarns {
				t.Errorf("warnings = %d (%v), want %d", got, warns.warns, tc.wantWarns)
			}
		})
	}
}

// TestShutdown_SchedulerWaitSharesTheBudget: the wait for the scheduler comes
// out of the shutdown budget, so a scheduler that does not stop cannot hold
// the stop past it, and the store still closes in the reserve after it.
func TestShutdown_SchedulerWaitSharesTheBudget(t *testing.T) {
	warns := &warnCounter{}
	s, st := observedServer(t, -1, warns)
	s.shutdownTimeout = 300 * time.Millisecond

	start := time.Now()
	if err := s.shutdown(); err != nil {
		t.Fatalf("shutdown = %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("shutdown took %v, want about the 300ms budget", took)
	}
	if !st.closed.Load() || !st.afterAbandoned.Load() {
		t.Errorf("store closed = %v, after the calls were abandoned = %v; want both", st.closed.Load(), st.afterAbandoned.Load())
	}
	if got := warns.count(); got != 1 {
		t.Errorf("warnings = %d (%v), want 1", got, warns.warns)
	}
}

// TestShutdown_HAHandOverCountsAgainstTheCeiling: on an HA leader the
// hand-over waits for the scheduler first (demoteTimeout), and the shutdown's
// own wait continues within the same ceiling, counted from the hand-over: the
// whole stop waits about one ceiling, not the hand-over plus a ceiling, and
// logs one warning.
func TestShutdown_HAHandOverCountsAgainstTheCeiling(t *testing.T) {
	warns := &warnCounter{}
	s, st := observedServer(t, -1, warns)
	s.ha = newHAState(store.ServerMember{Name: "self"})
	s.ha.role.promote("self", 1)
	ceiling := demoteTimeout + 500*time.Millisecond
	s.schedStopCeiling = ceiling

	start := time.Now()
	if err := s.shutdown(); err != nil {
		t.Fatalf("shutdown = %v", err)
	}
	// The wait runs on real timers, so the upper bound has slack for a loaded
	// machine: as much as it can take and still fail a stop that waits the
	// hand-over plus a whole ceiling, which takes demoteTimeout longer.
	if took := time.Since(start); took < demoteTimeout || took > ceiling+demoteTimeout-250*time.Millisecond {
		t.Errorf("shutdown took %v, want about the %v ceiling", took, ceiling)
	}
	if !st.closed.Load() || !st.afterAbandoned.Load() {
		t.Errorf("store closed = %v, after the calls were abandoned = %v; want both", st.closed.Load(), st.afterAbandoned.Load())
	}
	if got := warns.count(); got != 1 {
		t.Errorf("warnings = %d (%v), want exactly 1 across the hand-over and the shutdown", got, warns.warns)
	}
}

// orderLog records the messages logged through it, in order.
type orderLog struct {
	mu   sync.Mutex
	msgs []string
}

func (*orderLog) Enabled(context.Context, slog.Level) bool { return true }

func (o *orderLog) Handle(_ context.Context, r slog.Record) error {
	o.add(r.Message)
	return nil
}

func (o *orderLog) WithAttrs([]slog.Attr) slog.Handler { return o }
func (o *orderLog) WithGroup(string) slog.Handler      { return o }

func (o *orderLog) add(msg string) {
	o.mu.Lock()
	o.msgs = append(o.msgs, msg)
	o.mu.Unlock()
}

func (o *orderLog) index(msg string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Index(o.msgs, msg)
}

// orderedCloseBackend notes its Close in the log, saying whether the
// server's scheduler had returned by then. Close runs on the goroutine that
// started the scheduler, so reading srv.schedDone there is not a race.
type orderedCloseBackend struct {
	backend.Backend

	log *orderLog
	srv *Server
}

func (b orderedCloseBackend) Close() error {
	msg := "test: store closed while the scheduler runs"
	if done := b.srv.schedDone; done != nil {
		select {
		case <-done:
			msg = "test: store closed"
		default:
		}
	}
	b.log.add(msg)
	return b.Backend.Close()
}

// TestRun_ClosesTheStoreOnlyAfterTheSchedulerStops is a wiring smoke test: it
// drives a real single server (embedded broker, SQLite) through start and a
// graceful stop, and checks that start records when the scheduler's Run
// returns and that it has returned before the store closes. It does not pin
// the wait itself: an idle scheduler returns faster than the bus drain, so it
// would pass if shutdown did not wait. The wait is pinned by
// TestShutdown_WaitsForTheSchedulerBeforeClosingTheStore.
func TestRun_ClosesTheStoreOnlyAfterTheSchedulerStops(t *testing.T) {
	tmp := t.TempDir()
	cfg := Config{
		HTTPAddr: freeLoopbackAddr(t),
		NATS: config.NATSConfig{Embedded: config.NATSEmbeddedConfig{
			Addr: freeLoopbackAddr(t), DataDir: filepath.Join(tmp, "nats"), MaxStoreMB: 64,
		}},
		SQLitePath:         filepath.Join(tmp, "test.db"),
		CheckpointInterval: time.Minute,
		Scheduler:          scheduler.DefaultConfig(),
	}
	log := &orderLog{}
	srv := New(cfg, slog.New(log), nil)
	srv.openBackend = func(ctx context.Context, o backend.Options) (backend.Backend, error) {
		st, err := backend.Open(ctx, o)
		if err != nil {
			return nil, err
		}
		return orderedCloseBackend{Backend: st, log: log, srv: srv}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	deadline := time.Now().Add(30 * time.Second)
	for log.index("sqi-server ready — waiting for signal") < 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the server never became ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Run did not return")
	}
	if log.index("test: store closed") < 0 {
		t.Errorf("the store did not close after the scheduler returned; log: %q", log.msgs)
	}
}

// TestSchedulerStopCeiling_BoundsTheWait: however long a configured script
// provider may run, the server waits at most 30 s for the scheduler, and the
// scheduler's own drains end inside that wait.
func TestSchedulerStopCeiling_BoundsTheWait(t *testing.T) {
	if scheduler.StopCeiling > 30*time.Second || scheduler.StopCeiling > ShutdownTimeout {
		t.Errorf("scheduler.StopCeiling = %v, want at most 30s and inside the %v budget", scheduler.StopCeiling, ShutdownTimeout)
	}
	cfg := DefaultConfig()
	cfg.Capacity.Providers = map[string]config.CapacityProviderConfig{
		"slow": {Command: []string{"/bin/true"}, Timeout: 10 * time.Minute},
	}
	s := &Server{cfg: cfg, logger: testLogger()}
	s.sched = scheduler.New(s.schedulerConfigFor(nil), fake.New(), nil, metrics.New(), testLogger(), nil, nil)
	if got := s.sched.StopTimeout(); got >= s.stopCeiling() {
		t.Errorf("scheduler StopTimeout() = %v with a 10m provider, want below the server's %v ceiling", got, s.stopCeiling())
	}
}

// TestAwaitScheduler_AlreadyStoppedIsNeverReportedUnstopped: a scheduler that
// had already returned is not logged or abandoned as one still stopping, even
// when the earlier shutdown steps used the whole budget and the wait has no
// time left at all.
func TestAwaitScheduler_AlreadyStoppedIsNeverReportedUnstopped(t *testing.T) {
	for range 50 {
		warns := &warnCounter{}
		s, st := observedServer(t, 0, warns)
		s.cancelSched()
		<-s.schedDone

		s.awaitScheduler(time.Now().Add(-time.Second))

		if got := warns.count(); got != 0 {
			t.Fatalf("warnings = %d (%v), want none for a stopped scheduler", got, warns.warns)
		}
		if st.abandoned.Load() {
			t.Fatal("the capacity calls of a stopped scheduler were abandoned")
		}
	}
}

// limitRecorder records the limit attribute of each Warn record.
type limitRecorder struct {
	mu     sync.Mutex
	limits []time.Duration
}

func (*limitRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (l *limitRecorder) Handle(_ context.Context, r slog.Record) error {
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "limit" {
			l.mu.Lock()
			l.limits = append(l.limits, a.Value.Duration())
			l.mu.Unlock()
		}
		return true
	})
	return nil
}

func (l *limitRecorder) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *limitRecorder) WithGroup(string) slog.Handler      { return l }

// TestAwaitScheduler_WarnLimitIsNeverNegative: a budget that ran out before the
// scheduler was told to stop logs a limit of zero, not a negative one.
func TestAwaitScheduler_WarnLimitIsNeverNegative(t *testing.T) {
	s, _ := observedServer(t, -1, &warnCounter{})
	rec := &limitRecorder{}
	s.logger = slog.New(rec)

	s.awaitScheduler(time.Now().Add(-time.Second))

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.limits) != 1 || rec.limits[0] < 0 {
		t.Errorf("logged limits = %v, want one, not negative", rec.limits)
	}
}
