// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/diag"
	"github.com/uberware/sqi/internal/discovery"
	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/backend"
	"github.com/uberware/sqi/internal/store/fake"
)

// recorder keeps the order in which the first occurrence of each named event
// happened, across the store, the broker and the discovery responder.
type recorder struct {
	mu     sync.Mutex
	events []string
	counts map[string]int
}

func newRecorder() *recorder { return &recorder{counts: map[string]int{}} }

func (r *recorder) add(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counts[name] == 0 {
		r.events = append(r.events, name)
	}
	r.counts[name]++
}

func (r *recorder) count(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[name]
}

// order returns the first-occurrence order of the named events, dropping the
// others.
func (r *recorder) order(names ...string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.events {
		if slices.Contains(names, e) {
			out = append(out, e)
		}
	}
	return out
}

// syncBuffer is a log sink safe to read while the server writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// haStore is the fake store recording the calls that mark each step of a
// server's life, with the leadership epoch the context carried.
type haStore struct {
	*fake.Store

	rec *recorder

	mu     sync.Mutex
	epochs map[string]int64 // event → the epoch on its context, -1 for none
}

func newHAStore(rec *recorder) *haStore {
	return &haStore{Store: fake.New(), rec: rec, epochs: map[string]int64{}}
}

func (*haStore) Ping(context.Context) error { return nil }

func (s *haStore) note(ctx context.Context, event string) {
	epoch, ok := store.LeaderEpoch(ctx)
	if !ok {
		epoch = -1
	}
	s.mu.Lock()
	if _, seen := s.epochs[event]; !seen {
		s.epochs[event] = epoch
	}
	s.mu.Unlock()
	s.rec.add(event)
}

// epochAt is the epoch the first call of event carried, -1 for none.
func (s *haStore) epochAt(event string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.epochs[event]; ok {
		return e
	}
	return -2
}

func (s *haStore) AcquireLeadership(ctx context.Context, holder string, ttl time.Duration) (int64, bool, error) {
	s.note(ctx, "AcquireAttempt")
	epoch, ok, err := s.Store.AcquireLeadership(ctx, holder, ttl)
	if ok {
		s.note(ctx, "Acquire")
	}
	return epoch, ok, err
}

func (s *haStore) ReleaseLeadership(ctx context.Context, holder string, epoch int64) error {
	s.note(ctx, "Release")
	return s.Store.ReleaseLeadership(ctx, holder, epoch)
}

func (s *haStore) UpsertServerMember(ctx context.Context, m store.ServerMember) error {
	s.note(ctx, "UpsertServerMember")
	return s.Store.UpsertServerMember(ctx, m)
}

func (s *haStore) GetBrokerIdentity(ctx context.Context) (string, error) {
	s.note(ctx, "reconcileBrokerLayout")
	return s.Store.GetBrokerIdentity(ctx)
}

func (s *haStore) ListFarms(ctx context.Context) ([]store.Farm, error) {
	s.note(ctx, "seedDefaults")
	return s.Store.ListFarms(ctx)
}

func (s *haStore) CountUsers(ctx context.Context) (int, error) {
	s.note(ctx, "bootstrapAdmin")
	return s.Store.CountUsers(ctx)
}

func (s *haStore) ListStuckSteps(ctx context.Context) ([]store.Step, error) {
	s.note(ctx, "scheduler.Run")
	return s.Store.ListStuckSteps(ctx)
}

func (s *haStore) DeleteExpiredSessions(ctx context.Context, now time.Time) (int, error) {
	s.note(ctx, "session sweeper")
	return s.Store.DeleteExpiredSessions(ctx, now)
}

func (s *haStore) DeleteServerMembersUnseenFor(ctx context.Context, d time.Duration) (int, error) {
	s.note(ctx, "prune members")
	return s.Store.DeleteServerMembersUnseenFor(ctx, d)
}

// fakeHABroker is an HA broker over a real embedded broker: the embedded
// broker carries the bus traffic, and the HA calls are recorded and steered
// by the test.
type fakeHABroker struct {
	bus.Broker

	emb *bus.Embedded
	rec *recorder

	mu       sync.Mutex
	up       bool
	upSince  time.Time
	readyErr error
	checked  chan struct{}
	takeErr  error
	takeWait time.Duration // how long TakeLeadership takes
	opts     bus.GuardOptions
	callout  []bool
	identity string
}

func newFakeHABroker(emb *bus.Embedded, rec *recorder, up bool) *fakeHABroker {
	b := &fakeHABroker{Broker: emb, emb: emb, rec: rec, checked: make(chan struct{}), readyErr: bus.ErrStandbyUnchecked}
	if up {
		b.up, b.upSince, b.readyErr = true, time.Now().UTC(), nil
		close(b.checked)
	}
	return b
}

func (b *fakeHABroker) StartStandby(ctx context.Context, opts bus.GuardOptions) error {
	b.rec.add("StartStandby")
	b.mu.Lock()
	b.opts = opts
	b.mu.Unlock()
	return b.Start(ctx)
}

func (b *fakeHABroker) StandbyReady() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.readyErr
}

func (b *fakeHABroker) StandbyChecked() <-chan struct{} { return b.checked }

func (b *fakeHABroker) TakeLeadership(ctx context.Context, opts bus.GuardOptions) error {
	b.rec.add("TakeLeadership")
	b.mu.Lock()
	wait := b.takeWait
	b.opts = opts
	b.mu.Unlock()
	select {
	case <-time.After(wait):
	case <-ctx.Done():
		return ctx.Err()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.takeErr
}

func (b *fakeHABroker) Connected() (bool, time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.up {
		return false, time.Time{}
	}
	return true, b.upSince
}

func (b *fakeHABroker) SetCalloutActive(active bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.callout = append(b.callout, active)
}

func (b *fakeHABroker) Check(ctx context.Context) error {
	if up, _ := b.Connected(); !up {
		return errors.New("bus: not connected")
	}
	return b.Broker.Check(ctx)
}

func (b *fakeHABroker) Identity() string {
	b.mu.Lock()
	id := b.identity
	b.mu.Unlock()
	if id != "" {
		return id
	}
	return b.Broker.Identity()
}

// bringUp reports the broker connected and checked.
func (b *fakeHABroker) bringUp() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.up, b.upSince, b.readyErr = true, time.Now().UTC(), nil
	close(b.checked)
}

// loseGuard plays another server taking the account's guard.
func (b *fakeHABroker) loseGuard(err error) {
	b.mu.Lock()
	onLost := b.opts.OnLost
	b.mu.Unlock()
	onLost(err)
}

func (b *fakeHABroker) calloutStates() []bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.callout)
}

// fakeResponder stands in for the mDNS responder.
type fakeResponder struct {
	rec      *recorder
	srv      *Server
	onStart  func() // run inside Start, on the server's goroutine
	mu       sync.Mutex
	leaderAt bool // whether the server reported itself leader at Start
	stopped  bool
}

func (r *fakeResponder) Start(context.Context) error {
	leader := r.srv.ha != nil && r.srv.ha.role.isLeader()
	r.mu.Lock()
	r.leaderAt = leader
	r.mu.Unlock()
	r.rec.add("discovery.Start")
	if r.onStart != nil {
		r.onStart()
	}
	return nil
}

func (r *fakeResponder) Shutdown() {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	r.rec.add("discovery.Shutdown")
}

// haHarness is one HA server under test, run on a fake store and an embedded
// broker behind a fake HA broker.
type haHarness struct {
	t      *testing.T
	srv    *Server
	st     *haStore
	br     *fakeHABroker
	disc   *fakeResponder
	rec    *recorder
	logs   *syncBuffer
	opts   []backend.Options
	optsMu sync.Mutex
	done   chan error
	cancel context.CancelFunc
	result error
	ended  bool
}

type haSetup struct {
	brokerDown bool
	diagBuf    *diag.Buffer
	ha         bool
	mutateCfg  func(*Config)
	seed       func(*haStore)
	mutateBr   func(*fakeHABroker)
	// onDiscoveryStart runs inside the mDNS responder's Start.
	onDiscoveryStart func(*Server)
}

// haConfig is a server configuration for the harness: HA on unless disabled,
// a fake-store-backed "postgres", an embedded broker on a free port.
func haConfig(t *testing.T, haOn bool) Config {
	t.Helper()
	tmp := t.TempDir()
	cfg := Config{
		HTTPAddr: freeLoopbackAddr(t),
		NATS: config.NATSConfig{Embedded: config.NATSEmbeddedConfig{
			Addr: freeLoopbackAddr(t), DataDir: filepath.Join(tmp, "nats"), MaxStoreMB: 64,
		}},
		Scheduler:        scheduler.DefaultConfig(),
		DisableRateLimit: true,
		SeedDefaults:     true,
	}
	if haOn {
		cfg.StoreDriver = backend.DriverPostgres
		cfg.DiscoveryEnabled = true
		cfg.HA = config.HAConfig{Enabled: true, LeaseTTL: 3 * time.Second, InstanceName: "self"}
	} else {
		cfg.SQLitePath = filepath.Join(tmp, "test.db")
		cfg.CheckpointInterval = time.Minute
	}
	return cfg
}

func startHarness(t *testing.T, setup haSetup) *haHarness {
	t.Helper()
	rec := newRecorder()
	h := &haHarness{t: t, st: newHAStore(rec), rec: rec, logs: &syncBuffer{}, done: make(chan error, 1)}
	if setup.seed != nil {
		setup.seed(h.st)
	}
	cfg := haConfig(t, setup.ha)
	if setup.mutateCfg != nil {
		setup.mutateCfg(&cfg)
	}
	logger := slog.New(slog.NewTextHandler(io.MultiWriter(h.logs), &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.srv = New(cfg, logger, setup.diagBuf)
	h.srv.openBackend = func(_ context.Context, o backend.Options) (backend.Backend, error) {
		h.optsMu.Lock()
		h.opts = append(h.opts, o)
		h.optsMu.Unlock()
		return h.st, nil
	}
	h.disc = &fakeResponder{rec: rec, srv: h.srv}
	if setup.onDiscoveryStart != nil {
		h.disc.onStart = func() { setup.onDiscoveryStart(h.srv) }
	}
	h.srv.newResponder = func(discovery.Config, *slog.Logger) (responder, error) { return h.disc, nil }
	if setup.ha {
		brokerReady := make(chan *fakeHABroker, 1)
		h.srv.wrapBroker = func(b bus.Broker) bus.Broker {
			emb, ok := b.(*bus.Embedded)
			if !ok {
				t.Errorf("broker is %T, want *bus.Embedded", b)
				return b
			}
			fb := newFakeHABroker(emb, rec, !setup.brokerDown)
			if setup.mutateBr != nil {
				setup.mutateBr(fb)
			}
			brokerReady <- fb
			return fb
		}
		defer func() {
			select {
			case h.br = <-brokerReady:
			case <-time.After(30 * time.Second):
				t.Fatal("the HA broker was never built")
			}
		}()
	}

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.done <- h.srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		h.await(45 * time.Second)
	})
	return h
}

// wait returns Run's result, failing the test if it does not end in time.
func (h *haHarness) wait(d time.Duration) error {
	h.t.Helper()
	h.await(d)
	return h.result
}

// await waits for Run to return and keeps its result.
func (h *haHarness) await(d time.Duration) {
	h.t.Helper()
	if h.ended {
		return
	}
	select {
	case h.result = <-h.done:
		h.ended = true
	case <-time.After(d):
		h.t.Fatalf("Run did not return within %v", d)
	}
}

// waitFor polls cond until it holds or d passes.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitLeader waits until the server has promoted itself.
func (h *haHarness) waitLeader() {
	h.t.Helper()
	waitFor(h.t, 30*time.Second, "promotion", func() bool {
		return h.rec.count("StartStandby") > 0 && h.srv.ha.role.isLeader()
	})
}

// get fetches path from the server's HTTP listener.
func (h *haHarness) get(path string) (int, http.Header, string) {
	h.t.Helper()
	req, err := http.NewRequestWithContext(h.t.Context(), http.MethodGet, "http://"+h.srv.cfg.HTTPAddr+path, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err.Error()
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, strings.TrimSpace(string(body))
}

// waitHTTP waits until path answers want.
func (h *haHarness) waitHTTP(path string, want int) string {
	h.t.Helper()
	var body string
	waitFor(h.t, 30*time.Second, path+" answering", func() bool {
		var code int
		code, _, body = h.get(path)
		return code == want
	})
	return body
}

// holdLease makes another member hold the lease, so the server under test
// stays a standby.
func holdLease(t *testing.T, st *haStore, name, version string) {
	t.Helper()
	ctx := context.Background()
	if err := st.Store.UpsertServerMember(ctx, store.ServerMember{
		InstanceID: "other-instance", Name: name, Host: "elsewhere", Version: version,
		GrantsFingerprint: bus.GrantsFingerprint(), StartedAt: time.Now().UTC(), BrokerOK: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.Store.AcquireLeadership(ctx, "other-instance", time.Hour); err != nil || !ok {
		t.Fatalf("hold the lease: ok %v, %v", ok, err)
	}
}
