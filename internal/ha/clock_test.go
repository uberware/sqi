// SPDX-License-Identifier: AGPL-3.0-or-later

package ha_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/ha"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// testTTL is the lease lifetime every test runs at: the default, so the
// renew period is 5s and the self-fence deadline 12s.
const testTTL = 15 * time.Second

// settleTimeout bounds every real-time wait, so a broken elector fails the
// test instead of hanging it.
const settleTimeout = 5 * time.Second

// fakeClock is a manually advanced clock. Its timers fire only from Advance,
// so a test decides exactly when time passes.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	timers  map[*fakeTimer]struct{}
	busy    map[any]bool  // Holds with a renewal in flight
	changed chan struct{} // closed and replaced whenever timers or busy changes
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start, timers: map[*fakeTimer]struct{}{}, busy: map[any]bool{}, changed: make(chan struct{})}
}

// setBusy records whether the Hold identified by key has a renewal in flight.
// Until its answer is handled, a Hold waiting on its deadline timer has not
// settled, however many timers are pending.
func (c *fakeClock) setBusy(key any, inFlight bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if inFlight {
		c.busy[key] = true
	} else {
		delete(c.busy, key)
	}
	c.notify()
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) ha.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, at: c.now.Add(d), ch: make(chan time.Time, 1)}
	if d <= 0 {
		t.ch <- c.now
		return t
	}
	c.timers[t] = struct{}{}
	c.notify()
	return t
}

// notify wakes waitTimers. The caller holds c.mu.
func (c *fakeClock) notify() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// Advance moves the clock forward by d and fires every timer now due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for t := range c.timers {
		if !t.at.After(c.now) {
			delete(c.timers, t)
			t.ch <- c.now
		}
	}
	c.notify()
}

// waitTimers waits until exactly n timers are pending and no Hold has a
// renewal in flight: the goroutines under test have gone back to waiting on
// the clock.
func (c *fakeClock) waitTimers(t *testing.T, n int) {
	t.Helper()
	c.waitTimersOr(t, n, nil)
}

// waitPending waits until exactly n timers are pending, whatever is in
// flight: for a renewal that never answers.
func (c *fakeClock) waitPending(t *testing.T, n int) {
	t.Helper()
	c.wait(t, n, nil, true)
}

// waitTimersOr is waitTimers that also returns, reporting true, once stop
// does: a goroutine under test that has finished holds no timer at all.
func (c *fakeClock) waitTimersOr(t *testing.T, n int, stop func() bool) bool {
	t.Helper()
	return c.wait(t, n, stop, false)
}

func (c *fakeClock) wait(t *testing.T, n int, stop func() bool, ignoreBusy bool) bool {
	t.Helper()
	deadline := time.After(settleTimeout)
	for {
		c.mu.Lock()
		got, busy, ch := len(c.timers), len(c.busy), c.changed
		c.mu.Unlock()
		if got == n && (ignoreBusy || busy == 0) {
			return false
		}
		if stop != nil && stop() {
			return true
		}
		var poll <-chan time.Time
		if stop != nil {
			poll = time.After(time.Millisecond)
		}
		select {
		case <-ch:
		case <-poll:
		case <-deadline:
			t.Fatalf("waiting for %d pending timers and none busy: %d pending, %d busy after %v", n, got, busy, settleTimeout)
		}
	}
}

type fakeTimer struct {
	c  *fakeClock
	at time.Time
	ch chan time.Time
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	if _, ok := t.c.timers[t]; !ok {
		return false
	}
	delete(t.c.timers, t)
	t.c.notify()
	return true
}

// skewedClock reads a fakeClock off by a fixed offset: a server whose wall
// clock disagrees with everyone else's, while time passes at the same rate.
type skewedClock struct {
	*fakeClock

	off time.Duration
}

func (c skewedClock) Now() time.Time { return c.fakeClock.Now().Add(c.off) }

// broker is a controllable bus connection state.
type broker struct {
	mu    sync.Mutex
	up    bool
	since time.Time
}

func (b *broker) set(up bool, since time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.up, b.since = up, since
}

func (b *broker) state() (bool, time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.up, b.since
}

// hookStore wraps the fake's leadership methods so a test can fail, slow or
// block them, and counts the calls.
type hookStore struct {
	store.LeadershipStore

	acquire  func(ctx context.Context, holder string, ttl time.Duration) (int64, bool, error)
	renew    func(ctx context.Context, holder string, epoch int64, ttl time.Duration) (bool, error)
	release  func(ctx context.Context, holder string, epoch int64) error
	upsert   func(ctx context.Context, m store.ServerMember) error
	acquires atomic.Int64
	renews   atomic.Int64
	inflight atomic.Int64 // renews not yet returned

	mu       sync.Mutex
	releases []int64 // epochs released
}

func (s *hookStore) AcquireLeadership(ctx context.Context, holder string, ttl time.Duration) (int64, bool, error) {
	s.acquires.Add(1)
	if s.acquire != nil {
		return s.acquire(ctx, holder, ttl)
	}
	return s.LeadershipStore.AcquireLeadership(ctx, holder, ttl)
}

func (s *hookStore) RenewLeadership(ctx context.Context, holder string, epoch int64, ttl time.Duration) (bool, error) {
	s.renews.Add(1)
	s.inflight.Add(1)
	defer s.inflight.Add(-1)
	if s.renew != nil {
		return s.renew(ctx, holder, epoch, ttl)
	}
	return s.LeadershipStore.RenewLeadership(ctx, holder, epoch, ttl)
}

func (s *hookStore) ReleaseLeadership(ctx context.Context, holder string, epoch int64) error {
	s.mu.Lock()
	s.releases = append(s.releases, epoch)
	s.mu.Unlock()
	if s.release != nil {
		return s.release(ctx, holder, epoch)
	}
	return s.LeadershipStore.ReleaseLeadership(ctx, holder, epoch)
}

func (s *hookStore) UpsertServerMember(ctx context.Context, m store.ServerMember) error {
	if s.upsert != nil {
		return s.upsert(ctx, m)
	}
	return s.LeadershipStore.UpsertServerMember(ctx, m)
}

func (s *hookStore) released() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.releases...)
}

// waitIdle waits until no renew is in flight, so a renew a finished Hold left
// behind cannot land after the test has read the lease.
func (s *hookStore) waitIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(settleTimeout)
	for s.inflight.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("a renew is still in flight")
		}
		time.Sleep(time.Millisecond)
	}
}

// harness is one fake database, its clock and the electors sharing it.
type harness struct {
	clk *fakeClock
	db  *fake.Store
	st  *hookStore
	t0  time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	start := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	clk := newFakeClock(start)
	db := fake.New()
	db.SetClock(func() int64 { return clk.Now().UnixMicro() })
	return &harness{clk: clk, db: db, st: &hookStore{LeadershipStore: db}, t0: start}
}

type electorOpt func(*ha.Config)

func withClock(c ha.Clock) electorOpt      { return func(cfg *ha.Config) { cfg.Clock = c } }
func withBroker(b *broker) electorOpt      { return func(cfg *ha.Config) { cfg.BrokerUp = b.state } }
func withLogger(l *slog.Logger) electorOpt { return func(cfg *ha.Config) { cfg.Logger = l } }

// elector returns an elector for instance id with its broker up, on the
// harness clock and store.
func (h *harness) elector(id string, opts ...electorOpt) *ha.Elector {
	b := &broker{up: true, since: h.t0}
	cfg := ha.Config{
		Self:     store.ServerMember{InstanceID: id, Name: "name-" + id, Host: "host-" + id, PID: 42, Version: "v0.0.0-test"},
		LeaseTTL: testTTL,
		BrokerUp: b.state,
		Clock:    h.clk,
		Logger:   slog.New(slog.DiscardHandler),
	}
	for _, o := range opts {
		o(&cfg)
	}
	e := ha.New(cfg, h.st)
	ha.SetWaitHook(e, func(inFlight bool) { h.clk.setBusy(e, inFlight) })
	return e
}

// lease reads the lease from the database.
func (h *harness) lease(t *testing.T) store.Lease {
	t.Helper()
	l, err := h.db.GetLeadership(context.Background())
	if err != nil {
		t.Fatalf("GetLeadership: %v", err)
	}
	return l
}

// step advances the clock by d in increments of inc, first waiting each time
// for n pending timers. each, when non-nil, runs after every increment; stop,
// when non-nil, ends the walk early once it reports true (a goroutine that
// has returned holds no timer, so n would never be reached again).
func (h *harness) step(t *testing.T, d, inc time.Duration, n int, each func(), stop func() bool) {
	t.Helper()
	for elapsed := time.Duration(0); elapsed < d; elapsed += inc {
		if h.clk.waitTimersOr(t, n, stop) {
			return
		}
		h.clk.Advance(inc)
		if each != nil {
			each()
		}
	}
	h.clk.waitTimersOr(t, n, stop)
}

// returned reports whether a result channel has a value waiting.
func returned[T any](ch <-chan T) func() bool {
	return func() bool { return len(ch) > 0 }
}

// campaign wins the lease for e at once, failing the test otherwise.
func campaign(t *testing.T, e *ha.Elector) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), settleTimeout)
	defer cancel()
	epoch, err := e.Campaign(ctx)
	if err != nil {
		t.Fatalf("Campaign: %v", err)
	}
	return epoch
}

// holdAsync runs Hold in the background and returns its result channel.
func holdAsync(ctx context.Context, e *ha.Elector, epoch int64) <-chan error {
	out := make(chan error, 1)
	go func() { out <- e.Hold(ctx, epoch) }()
	return out
}

// campaignAsync runs Campaign in the background.
type campaignResult struct {
	epoch int64
	err   error
}

func campaignAsync(ctx context.Context, e *ha.Elector) <-chan campaignResult {
	out := make(chan campaignResult, 1)
	go func() {
		epoch, err := e.Campaign(ctx)
		out <- campaignResult{epoch, err}
	}()
	return out
}

// lostErr waits for a Hold result and returns its loss, failing the test
// when Hold does not return a *LostError in time.
func lostErr(t *testing.T, done <-chan error, within time.Duration) *ha.LostError {
	t.Helper()
	select {
	case err := <-done:
		var lost *ha.LostError
		if !errors.As(err, &lost) {
			t.Fatalf("Hold returned %v (%T), want a *ha.LostError", err, err)
		}
		return lost
	case <-time.After(within):
		t.Fatalf("Hold did not return within %v", within)
		return nil
	}
}

// notReturned fails the test when done has a result ready.
func notReturned(t *testing.T, done <-chan error, when string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("Hold returned %v %s; it must still be holding", err, when)
	default:
	}
}
