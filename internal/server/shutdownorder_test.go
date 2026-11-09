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
// returned by then.
type closeObservingStore struct {
	store.Store

	schedDone <-chan struct{}
	closed    atomic.Bool
	afterRun  atomic.Bool
}

func (c *closeObservingStore) Close() error {
	select {
	case <-c.schedDone:
		c.afterRun.Store(true)
	default:
	}
	c.closed.Store(true)
	return c.Store.Close()
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
// outlasts its bound is logged once and the shutdown goes on.
func TestShutdown_WaitsForTheSchedulerBeforeClosingTheStore(t *testing.T) {
	tests := []struct {
		name       string
		delay      time.Duration // how long Run takes to return once canceled; <0 never
		bound      time.Duration
		wantAfter  bool
		wantWarns  int
		maxElapsed time.Duration
	}{
		{"a scheduler that stops in time", 300 * time.Millisecond, 10 * time.Second, true, 0, 5 * time.Second},
		{"a scheduler that outlasts its bound", -1, 200 * time.Millisecond, false, 1, 5 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cancel, done := stoppingScheduler(tc.delay)
			defer cancel()
			st := &closeObservingStore{Store: fake.New(), schedDone: done}
			warns := &warnCounter{}
			s := &Server{
				logger: slog.New(warns), store: st,
				cancelSched: cancel, schedDone: done, schedStopTimeout: tc.bound,
			}

			start := time.Now()
			if err := s.shutdown(); err != nil {
				t.Fatalf("shutdown = %v", err)
			}
			if took := time.Since(start); took > tc.maxElapsed {
				t.Errorf("shutdown took %v", took)
			}
			if !st.closed.Load() {
				t.Fatal("the store was not closed")
			}
			if got := st.afterRun.Load(); got != tc.wantAfter {
				t.Errorf("store closed after the scheduler returned = %v, want %v", got, tc.wantAfter)
			}
			if got := warns.count(); got != tc.wantWarns {
				t.Errorf("warnings = %d (%v), want %d", got, warns.warns, tc.wantWarns)
			}
		})
	}
}

// TestShutdown_SchedulerWaitIsOutsideTheBudget: the time spent waiting for
// the scheduler does not come out of the shutdown budget the bus drain and
// the store close run under, so a long capacity drain does not report a
// timed-out shutdown.
func TestShutdown_SchedulerWaitIsOutsideTheBudget(t *testing.T) {
	cancel, done := stoppingScheduler(300 * time.Millisecond)
	defer cancel()
	s := &Server{
		logger: testLogger(), store: fake.New(),
		cancelSched: cancel, schedDone: done, schedStopTimeout: time.Minute,
		shutdownTimeout: 200 * time.Millisecond,
	}
	if err := s.shutdown(); err != nil {
		t.Fatalf("shutdown = %v, want nil: the scheduler wait must not spend the budget", err)
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

// TestRun_ClosesTheStoreOnlyAfterTheSchedulerStops drives a real single
// server (embedded broker, SQLite) through start and a graceful stop: start
// records when the scheduler's Run returns, and it has returned before the
// store closes. An idle scheduler returns quickly, so the wait itself is
// pinned by TestShutdown_WaitsForTheSchedulerBeforeClosingTheStore.
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

// TestSchedulerStopTimeout_CoversTheSchedulersDrain: the server waits at
// least as long as the scheduler's own drain, which a configured script
// provider's timeout lengthens.
func TestSchedulerStopTimeout_CoversTheSchedulersDrain(t *testing.T) {
	if got := (&Server{}).schedulerStopTimeout(); got < schedulerStopMargin {
		t.Errorf("schedulerStopTimeout() = %v with no scheduler, want at least the margin %v", got, schedulerStopMargin)
	}
	cfg := DefaultConfig()
	cfg.Capacity.Providers = map[string]config.CapacityProviderConfig{
		"slow": {Command: []string{"/bin/true"}, Timeout: 10 * time.Minute},
	}
	s := &Server{cfg: cfg, logger: testLogger()}
	s.sched = scheduler.New(s.schedulerConfigFor(nil), fake.New(), nil, metrics.New(), testLogger(), nil, nil)
	if got, want := s.schedulerStopTimeout(), s.sched.StopTimeout()+schedulerStopMargin; got != want {
		t.Errorf("schedulerStopTimeout() = %v, want the scheduler's %v plus the margin", got, want)
	}
	if got := s.sched.StopTimeout(); got < 10*time.Minute {
		t.Errorf("scheduler StopTimeout() = %v, want at least the script provider's 10m timeout", got)
	}
}
