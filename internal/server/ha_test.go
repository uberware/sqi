// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/diag"
	"github.com/uberware/sqi/internal/ha"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// readyzGoldenMain is the /readyz body a server without HA serves with a
// healthy store and broker. HA must not change it.
const readyzGoldenMain = `{"status":"ok","checks":{"nats":{"status":"ok"},"sqlite":{"status":"ok"}}}`

// requireDemoted checks that err is a demotion for reason.
func requireDemoted(t *testing.T, err error, reason ha.Reason) {
	t.Helper()
	if !errors.Is(err, ErrDemoted) {
		t.Fatalf("Run returned %v; want an error wrapping ErrDemoted", err)
	}
	var lost *ha.LostError
	if !errors.As(err, &lost) || lost.Reason != reason {
		t.Fatalf("Run returned %v; want a loss for %q", err, reason)
	}
}

// requireOneError checks that a loss was logged at ERROR exactly once, by
// the elector, with nothing else at ERROR beside it.
func requireOneError(t *testing.T, logs string) {
	t.Helper()
	if n := strings.Count(logs, "level=ERROR"); n != 1 || !strings.Contains(logs, `level=ERROR msg="ha: lost leadership"`) {
		t.Errorf("got %d ERROR lines, want only the elector's one:\n%s", n, logs)
	}
}

func demotions(t *testing.T, m *metrics.Metrics, reason ha.Reason) float64 {
	t.Helper()
	return metricValue(t, m, `sqi_server_ha_demotions_total{reason="`+string(reason)+`"}`)
}

// metricValue reads one series from m's exposition, 0 when it is absent.
func metricValue(t *testing.T, m *metrics.Metrics, series string) float64 {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	m.Handler().ServeHTTP(rr, req)
	for line := range strings.SplitSeq(rr.Body.String(), "\n") {
		if v, ok := strings.CutPrefix(line, series+" "); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatalf("series %s: %v", series, err)
			}
			return f
		}
	}
	return 0
}

func TestHADisabled_DefaultUnchanged(t *testing.T) {
	h := startHarness(t, haSetup{})
	body := h.waitHTTP("/readyz", http.StatusOK)
	if body != readyzGoldenMain {
		t.Fatalf("/readyz = %s\nwant      %s", body, readyzGoldenMain)
	}
	if code, _, _ := h.get("/leaderz"); code != http.StatusNotFound {
		t.Errorf("/leaderz = %d without HA, want 404", code)
	}
	if code, hdr, _ := h.get("/api/v1/version"); code != http.StatusOK || hdr.Get("Retry-After") != "" {
		t.Errorf("GET /api/v1/version = %d (Retry-After %q) without HA, want 200 and no gate", code, hdr.Get("Retry-After"))
	}

	for _, ev := range []string{"AcquireAttempt", "Release", "UpsertServerMember", "prune members", "StartStandby"} {
		if n := h.rec.count(ev); n != 0 {
			t.Errorf("%s happened %d times without HA, want never", ev, n)
		}
	}
	members, _, err := h.st.ListServerMembers(context.Background())
	if err != nil || len(members) != 0 {
		t.Errorf("server members = %v, %v; want none without HA", members, err)
	}
	h.optsMu.Lock()
	opts := slices.Clone(h.opts)
	h.optsMu.Unlock()
	if len(opts) != 1 || opts[0].HA || opts[0].LeaseTTL != 0 || !opts[0].ServerLock || !opts[0].AutoMigrate {
		t.Errorf("store options = %+v; want one open, exclusive lock, auto-migrate, no HA", opts)
	}
	if h.srv.ha != nil {
		t.Error("a server without HA built HA state")
	}
	if strings.Contains(h.logs.String(), "exit_code=75") {
		t.Error("a server without HA warned about the demotion exit code")
	}
	if _, _, body := h.get("/metrics"); strings.Contains(body, "sqi_server_ha_") {
		t.Error("a server without HA exposes sqi_server_ha_* series; its /metrics must be main's")
	}

	h.cancel()
	if err := h.wait(30 * time.Second); err != nil {
		t.Fatalf("Run = %v; want a clean stop", err)
	}
}

func TestHA_StandbyServesHealthOnly(t *testing.T) {
	h := startHarness(t, haSetup{ha: true, seed: func(st *haStore) { holdLease(t, st, "alpha", "") }})

	h.waitHTTP("/healthz", http.StatusOK)
	const wantReady = `{"status":"ok","checks":{"nats":{"status":"ok"},"postgres":{"status":"ok"}},"role":"standby","leader":"alpha","epoch":1}`
	waitFor(t, 30*time.Second, "a warm standby's /readyz", func() bool {
		code, _, body := h.get("/readyz")
		return code == http.StatusOK && body == wantReady
	})
	code, _, body := h.get("/leaderz")
	if code != http.StatusServiceUnavailable || body != `{"status":"standby","role":"standby","leader":"alpha","epoch":1}` {
		t.Fatalf("/leaderz = %d %s; want 503 naming the leader", code, body)
	}
	if code, _, _ := h.get("/metrics"); code != http.StatusOK {
		t.Errorf("/metrics = %d on a standby, want 200", code)
	}
	for _, path := range []string{"/api/v1/version", "/api/v1/jobs", "/"} {
		code, hdr, body := h.get(path)
		if code != http.StatusServiceUnavailable || hdr.Get("Retry-After") != "2" {
			t.Errorf("GET %s = %d (Retry-After %q) on a standby; want 503 with Retry-After: 2", path, code, hdr.Get("Retry-After"))
		}
		var got map[string]string
		if err := json.Unmarshal([]byte(body), &got); err != nil || got["error"] != "standby" || got["leader"] != "alpha" {
			t.Errorf("GET %s body = %s; want the standby error naming alpha", path, body)
		}
	}
	if n := h.rec.count("Acquire"); n != 0 {
		t.Fatalf("a standby whose leader holds the lease won it %d times", n)
	}
	if got := metricValue(t, h.srv.metrics, "sqi_server_ha_leader"); got != 0 {
		t.Errorf("sqi_server_ha_leader = %v on a standby, want 0", got)
	}
}

func TestHA_PromotionOrder(t *testing.T) {
	h := startHarness(t, haSetup{ha: true, mutateCfg: func(c *Config) { c.AuthEnabled = true }})
	h.waitLeader()
	h.waitHTTP("/leaderz", http.StatusOK)
	waitFor(t, 10*time.Second, "the scheduler", func() bool { return h.rec.count("scheduler.Run") > 0 })

	// scheduler.Run is started in its place but runs on its own goroutine, as
	// without HA; its first store read can land after the steps that follow
	// it, never before the ones that precede it.
	want := []string{
		"StartStandby", "Acquire", "TakeLeadership", "reconcileBrokerLayout", "seedDefaults",
		"bootstrapAdmin", "session sweeper", "discovery.Start",
	}
	if got := h.rec.order(want...); !slices.Equal(got, want) {
		t.Fatalf("promotion order = %v\nwant            %v", got, want)
	}
	if got := h.rec.order("bootstrapAdmin", "scheduler.Run"); !slices.Equal(got, []string{"bootstrapAdmin", "scheduler.Run"}) {
		t.Fatalf("order = %v; the scheduler must start after the bootstrap", got)
	}
	h.disc.mu.Lock()
	leaderAtDiscovery := h.disc.leaderAt
	h.disc.mu.Unlock()
	if leaderAtDiscovery {
		t.Error("the server reported itself leader before the mDNS responder started; the role flips last")
	}

	lease, err := h.st.GetLeadership(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []string{"reconcileBrokerLayout", "seedDefaults", "scheduler.Run", "session sweeper"} {
		if got := h.st.epochAt(ev); got != lease.Epoch {
			t.Errorf("%s ran under epoch %d, want the leader's %d", ev, got, lease.Epoch)
		}
	}
	if got := metricValue(t, h.srv.metrics, "sqi_server_ha_leader"); got != 1 {
		t.Errorf("sqi_server_ha_leader = %v, want 1", got)
	}
	if got := metricValue(t, h.srv.metrics, "sqi_server_ha_epoch"); got != float64(lease.Epoch) {
		t.Errorf("sqi_server_ha_epoch = %v, want %d", got, lease.Epoch)
	}
	if got := metricValue(t, h.srv.metrics, "sqi_server_ha_promotions_total"); got != 1 {
		t.Errorf("sqi_server_ha_promotions_total = %v, want 1", got)
	}
	// Auth is on here, so the open API answers 401 rather than the gate's 503.
	if code, hdr, _ := h.get("/api/v1/version"); code != http.StatusUnauthorized || hdr.Get("Retry-After") != "" {
		t.Errorf("GET /api/v1/version = %d on the leader, want the API's own 401", code)
	}
	body := h.waitHTTP("/readyz", http.StatusOK)
	if !strings.Contains(body, `"role":"leader","leader":"self"`) {
		t.Errorf("/readyz on the leader = %s; want role leader naming itself", body)
	}
}

func TestHA_ReconcileBrokerLayoutNeverOnStandby(t *testing.T) {
	var taskIDs []string
	h := startHarness(t, haSetup{ha: true, seed: func(st *haStore) {
		taskIDs = seedOpenAttempts(t, st.Store)
		if err := st.SetBrokerIdentity(context.Background(), "a-broker-this-server-never-saw", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		holdLease(t, st, "alpha", "")
	}})
	h.waitHTTP("/readyz", http.StatusOK)
	waitFor(t, 30*time.Second, "the standby to campaign", func() bool { return h.rec.count("AcquireAttempt") >= 2 })

	if n := h.rec.count("reconcileBrokerLayout"); n != 0 {
		t.Fatalf("a standby read the broker identity %d times; the reconcile is the leader's", n)
	}
	open, err := h.st.ListOpenAttempts(context.Background(), time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || len(open) != len(taskIDs) {
		t.Fatalf("open attempts = %v, %v; want the leader's %d untouched", open, err, len(taskIDs))
	}
	if id, err := h.st.Store.GetBrokerIdentity(context.Background()); err != nil || id != "a-broker-this-server-never-saw" {
		t.Fatalf("broker identity = %q, %v; a standby must not record its own", id, err)
	}
}

func TestHA_PromotionFailureReleasesAndDemotes(t *testing.T) {
	h := startHarness(t, haSetup{ha: true, mutateBr: func(b *fakeHABroker) {
		b.takeErr = errors.New("bus: the guard is held by another server")
	}})
	err := h.wait(30 * time.Second)
	requireDemoted(t, err, ha.ReasonPromotion)

	if got := h.rec.order("Acquire", "TakeLeadership", "Release"); !slices.Equal(got, []string{"Acquire", "TakeLeadership", "Release"}) {
		t.Fatalf("order = %v; want the lease released after the failed step", got)
	}
	if n := h.rec.count("reconcileBrokerLayout") + h.rec.count("scheduler.Run"); n != 0 {
		t.Fatal("promotion went on past the failed step")
	}
	lease, err := h.st.GetLeadership(context.Background())
	if err != nil || lease.Holder != "" {
		t.Fatalf("lease = %+v, %v; want it released", lease, err)
	}
	if got := demotions(t, h.srv.metrics, ha.ReasonPromotion); got != 1 {
		t.Errorf("demotions{promotion_failed} = %v, want 1", got)
	}
}

// TestHA_SlowPromotionKeepsTheLease: the lease is renewed while promotion
// runs, so a promotion outlasting the lease (taking the broker's guard can
// watch another holder's key for a guard TTL) does not lose it.
func TestHA_SlowPromotionKeepsTheLease(t *testing.T) {
	h := startHarness(t, haSetup{ha: true, mutateBr: func(b *fakeHABroker) {
		b.takeWait = 4 * time.Second // longer than the 3s lease
	}})
	h.waitLeader()
	if n := h.rec.count("Acquire"); n != 1 {
		t.Fatalf("won the lease %d times, want once", n)
	}
	lease, err := h.st.GetLeadership(context.Background())
	if err != nil || lease.Holder != h.srv.ha.self.InstanceID || lease.Epoch != 1 {
		t.Fatalf("lease = %+v, %v; want this server still holding epoch 1", lease, err)
	}
	if strings.Contains(h.logs.String(), "level=ERROR") {
		t.Fatalf("a slow promotion logged an ERROR:\n%s", h.logs.String())
	}
}

// TestHA_LossDuringPromotionStopsIt: a loss of authority while promotion is
// under way ends it there.
func TestHA_LossDuringPromotionStopsIt(t *testing.T) {
	h := startHarness(t, haSetup{ha: true, mutateBr: func(b *fakeHABroker) { b.takeWait = time.Minute }})
	waitFor(t, 30*time.Second, "promotion to start", func() bool { return h.rec.count("TakeLeadership") > 0 })

	h.srv.ha.elector.Lose(ha.ReasonRenewRefused, errors.New("induced by the test"))
	err := h.wait(30 * time.Second)
	requireDemoted(t, err, ha.ReasonRenewRefused)
	if n := h.rec.count("reconcileBrokerLayout"); n != 0 {
		t.Fatal("promotion went on after the loss")
	}
}

func TestHA_LossExitsWithDemoted(t *testing.T) {
	for _, reason := range []ha.Reason{
		ha.ReasonRenewRefused, ha.ReasonDeadline, ha.ReasonFenced,
		ha.ReasonGuardLost, ha.ReasonBrokerLost, ha.ReasonPromotion,
	} {
		t.Run(string(reason), func(t *testing.T) {
			t.Parallel()
			h := startHarness(t, haSetup{ha: true})
			h.waitLeader()

			h.srv.ha.elector.Lose(reason, errors.New("induced by the test"))
			err := h.wait(30 * time.Second)
			requireDemoted(t, err, reason)

			if got := demotions(t, h.srv.metrics, reason); got != 1 {
				t.Errorf("demotions{%s} = %v, want 1", reason, got)
			}
			if got := metricValue(t, h.srv.metrics, "sqi_server_ha_leader"); got != 0 {
				t.Errorf("sqi_server_ha_leader = %v after a demotion, want 0", got)
			}
			lease, err := h.st.GetLeadership(context.Background())
			if err != nil || lease.Holder != "" {
				t.Errorf("lease = %+v, %v; a demoted leader releases it", lease, err)
			}
			if n := strings.Count(h.logs.String(), "level=ERROR msg=\"ha: lost leadership\""); n != 1 {
				t.Errorf("the loss was logged at ERROR %d times, want once (by the elector)", n)
			}
		})
	}
}

func TestHA_FencedSchedulerDemotes(t *testing.T) {
	h := startHarness(t, haSetup{ha: true, mutateCfg: func(c *Config) {
		// A long lease keeps the renewal from noticing the takeover first.
		c.HA.LeaseTTL = 60 * time.Second
		c.Scheduler.HeartbeatSweepInterval = 100 * time.Millisecond
	}})
	h.waitLeader()

	// Another server takes over: the lease lapses on the database clock and
	// is won at the next epoch.
	h.st.SetClock(func() int64 { return time.Now().Add(2 * time.Hour).UnixMicro() })
	if _, ok, err := h.st.Store.AcquireLeadership(context.Background(), "usurper", 4*time.Hour); err != nil || !ok {
		t.Fatalf("take the lease: ok %v, %v", ok, err)
	}
	h.st.SetClock(nil)

	err := h.wait(30 * time.Second)
	requireDemoted(t, err, ha.ReasonFenced)
	requireOneError(t, h.logs.String())
	if got := demotions(t, h.srv.metrics, ha.ReasonFenced); got != 1 {
		t.Errorf("demotions{fenced} = %v, want 1", got)
	}
	lease, err := h.st.GetLeadership(context.Background())
	if err != nil || lease.Holder != "usurper" {
		t.Fatalf("lease = %+v, %v; the deposed leader's release must not touch the new holder's", lease, err)
	}
}

func TestHA_GuardLostDemotes(t *testing.T) {
	h := startHarness(t, haSetup{ha: true})
	h.waitLeader()

	h.br.loseGuard(errors.New("bus: another server took the account's guard"))
	err := h.wait(30 * time.Second)
	requireDemoted(t, err, ha.ReasonGuardLost)
	requireOneError(t, h.logs.String())
	if got := demotions(t, h.srv.metrics, ha.ReasonGuardLost); got != 1 {
		t.Errorf("demotions{guard_lost} = %v, want 1", got)
	}
}

// TestHA_LossAtDiscoveryNeverLeads: a loss while the last promotion steps
// run (here, while the mDNS responder starts) stops the promotion before the
// server reports itself leader, and takes the advertisement down again.
func TestHA_LossAtDiscoveryNeverLeads(t *testing.T) {
	h := startHarness(t, haSetup{
		ha:        true,
		mutateCfg: func(c *Config) { c.AuthEnabled = true },
		onDiscoveryStart: func(s *Server) {
			s.ha.elector.Lose(ha.ReasonFenced, errors.New("induced while discovery starts"))
		},
	})
	err := h.wait(30 * time.Second)
	requireDemoted(t, err, ha.ReasonFenced)

	logs := h.logs.String()
	if strings.Contains(logs, "sqi-server ready as the leader") {
		t.Error("the server reported itself leader after the loss")
	}
	if got := metricValue(t, h.srv.metrics, "sqi_server_ha_promotions_total"); got != 0 {
		t.Errorf("sqi_server_ha_promotions_total = %v, want 0", got)
	}
	h.disc.mu.Lock()
	stopped, leaderAt := h.disc.stopped, h.disc.leaderAt
	h.disc.mu.Unlock()
	if !stopped || leaderAt {
		t.Errorf("mDNS stopped = %v, leader at its start = %v; want stopped, never leader", stopped, leaderAt)
	}
	requireOneError(t, logs)
}

// TestHandOver_ReleasesWhenSchedulerHangs: a graceful stop releases the lease
// even when the scheduler does not stop, and even when the shutdown budget is
// already spent, so a standby takes over at its next poll rather than a full
// lease later.
func TestHandOver_ReleasesWhenSchedulerHangs(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
	}{
		{"shutdown budget left", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 30*time.Second)
		}},
		{"shutdown budget spent", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newHAStore(newRecorder())
			s, _ := observeServer(t, st, &fakeHABroker{rec: newRecorder()}, "")
			s.ha.elector = ha.New(ha.Config{Self: s.ha.self, LeaseTTL: 3 * time.Second, Logger: s.logger}, st)
			if _, err := s.ha.elector.Campaign(t.Context()); err != nil {
				t.Fatal(err)
			}
			s.ha.role.promote("self", 1)
			s.cancelSched = func() {}
			s.ha.schedDone = make(chan struct{}) // a scheduler that never returns

			ctx, cancel := tc.ctx()
			defer cancel()
			start := time.Now()
			if err := s.handOver(ctx); err != nil {
				t.Fatalf("handOver = %v", err)
			}
			if took := time.Since(start); took > demoteTimeout+time.Second {
				t.Errorf("handOver took %v waiting for the scheduler, want at most about %v", took, demoteTimeout)
			}
			lease, err := st.GetLeadership(context.Background())
			if err != nil || lease.Holder != "" {
				t.Fatalf("lease = %+v, %v; want it released", lease, err)
			}
			if s.ha.role.isLeader() {
				t.Error("still reporting leader after the hand-over")
			}
		})
	}
}

func TestPromotionReason(t *testing.T) {
	if got := promotionReason(fmt.Errorf("seed: %w", store.ErrFenced)); got != ha.ReasonFenced {
		t.Errorf("fenced error → %q, want fenced", got)
	}
	if got := promotionReason(errors.New("consumer: boom")); got != ha.ReasonPromotion {
		t.Errorf("other error → %q, want promotion_failed", got)
	}
}

func TestHA_GracefulStopReleases(t *testing.T) {
	h := startHarness(t, haSetup{ha: true})
	h.waitLeader()
	waitFor(t, 10*time.Second, "discovery", func() bool { return h.rec.count("discovery.Start") > 0 })

	h.cancel()
	if err := h.wait(30 * time.Second); err != nil {
		t.Fatalf("Run = %v; a graceful stop of the leader exits cleanly", err)
	}
	lease, err := h.st.GetLeadership(context.Background())
	if err != nil || lease.Holder != "" {
		t.Fatalf("lease = %+v, %v; want the holder released", lease, err)
	}
	h.disc.mu.Lock()
	stopped := h.disc.stopped
	h.disc.mu.Unlock()
	if !stopped {
		t.Error("the mDNS responder was not stopped")
	}
	if got := h.rec.order("discovery.Shutdown", "Release"); !slices.Equal(got, []string{"discovery.Shutdown", "Release"}) {
		t.Errorf("stop order = %v; want advertising stopped before the lease is handed over", got)
	}
	if strings.Contains(h.logs.String(), "level=ERROR") {
		t.Errorf("a graceful stop logged an ERROR:\n%s", h.logs.String())
	}
}

func TestStandby_NoBrokerNeverCampaigns(t *testing.T) {
	h := startHarness(t, haSetup{ha: true, brokerDown: true})

	// The listener is up without a broker, so a balancer can probe it.
	h.waitHTTP("/healthz", http.StatusOK)
	time.Sleep(2500 * time.Millisecond)
	if n := h.rec.count("AcquireAttempt"); n != 0 {
		t.Fatalf("a standby without a broker tried the lease %d times", n)
	}
	if n := h.rec.count("UpsertServerMember"); n == 0 {
		t.Fatal("a standby without a broker wrote no membership row")
	}

	up := time.Now()
	h.br.bringUp()
	waitFor(t, 1500*time.Millisecond, "a campaign once the broker is up", func() bool {
		return h.rec.count("AcquireAttempt") > 0
	})
	t.Logf("first acquire %v after the broker came up", time.Since(up))
}

func TestHA_WarnsWithoutSupervisorHint(t *testing.T) {
	h := startHarness(t, haSetup{ha: true, seed: func(st *haStore) { holdLease(t, st, "alpha", "") }})
	h.waitHTTP("/readyz", http.StatusOK)

	var warns []string
	for line := range strings.SplitSeq(h.logs.String(), "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "exit_code=75") {
			warns = append(warns, line)
		}
	}
	if len(warns) != 1 {
		t.Fatalf("got %d WARN lines naming exit code 75, want 1:\n%s", len(warns), strings.Join(warns, "\n"))
	}
	if !strings.Contains(warns[0], "restart") {
		t.Errorf("the WARN %q does not name the restart requirement", warns[0])
	}
}

// observeServer is a Server holding only what observeGroup reads.
func observeServer(t *testing.T, st *haStore, br *fakeHABroker, version string) (*Server, *syncBuffer) {
	t.Helper()
	buf := &syncBuffer{}
	s := &Server{
		cfg:     Config{HA: config.HAConfig{Enabled: true, LeaseTTL: config.DefaultHALeaseTTL}},
		logger:  slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		store:   st,
		metrics: metrics.New(),
		ha: newHAState(store.ServerMember{
			InstanceID: "self-instance", Name: "self", Version: version, GrantsFingerprint: bus.GrantsFingerprint(),
		}),
	}
	s.ha.broker = br
	s.metrics.RegisterHA() // as runHA does
	return s, buf
}

func TestHA_StandbyWarnsOnVersionMismatch(t *testing.T) {
	tests := []struct {
		name, leaderVersion, selfVersion string
		wantWarn                         bool
	}{
		{"different versions", "0.4.0", "0.4.1", true},
		{"equal versions", "0.4.1", "0.4.1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newHAStore(newRecorder())
			holdLease(t, st, "alpha", tt.leaderVersion)
			s, buf := observeServer(t, st, &fakeHABroker{rec: newRecorder()}, tt.selfVersion)

			for range 3 {
				s.observeGroup(t.Context())
			}
			var warns []string
			for line := range strings.SplitSeq(buf.String(), "\n") {
				if strings.Contains(line, "level=WARN") && strings.Contains(line, "version") {
					warns = append(warns, line)
				}
			}
			if !tt.wantWarn {
				if len(warns) != 0 {
					t.Fatalf("equal versions warned:\n%s", strings.Join(warns, "\n"))
				}
				return
			}
			if len(warns) != 1 {
				t.Fatalf("got %d version WARNs over three ticks, want 1:\n%s", len(warns), strings.Join(warns, "\n"))
			}
			if !strings.Contains(warns[0], "0.4.0") || !strings.Contains(warns[0], "0.4.1") {
				t.Errorf("the WARN %q does not name both versions", warns[0])
			}

			// A different pair warns again, once.
			if err := st.Store.UpsertServerMember(t.Context(), store.ServerMember{
				InstanceID: "other-instance", Name: "alpha", Version: "0.5.0", GrantsFingerprint: bus.GrantsFingerprint(),
			}); err != nil {
				t.Fatal(err)
			}
			s.observeGroup(t.Context())
			s.observeGroup(t.Context())
			if n := strings.Count(buf.String(), "0.5.0"); n != 1 {
				t.Errorf("the new pair was named %d times, want once", n)
			}
		})
	}
}

func TestHA_StandbyLeavesCalloutGroupOnFingerprintMismatch(t *testing.T) {
	st := newHAStore(newRecorder())
	holdLease(t, st, "alpha", "")
	br := &fakeHABroker{rec: newRecorder()}
	s, buf := observeServer(t, st, br, "")

	s.observeGroup(t.Context())
	if got := br.calloutStates(); !slices.Equal(got, []bool{true}) {
		t.Fatalf("callout states = %v with matching fingerprints, want [true]", got)
	}

	if err := st.Store.UpsertServerMember(t.Context(), store.ServerMember{
		InstanceID: "other-instance", Name: "alpha", GrantsFingerprint: "different",
	}); err != nil {
		t.Fatal(err)
	}
	s.observeGroup(t.Context())
	s.observeGroup(t.Context())
	if got := br.calloutStates(); !slices.Equal(got, []bool{true, false, false}) {
		t.Fatalf("callout states = %v after the leader's grants changed, want the standby out", got)
	}
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 {
		t.Errorf("logged %d WARNs for one fingerprint mismatch, want 1:\n%s", n, buf.String())
	}

	// This server leads now: it answers callouts whatever the others grant.
	s.ha.role.promote("self", 9)
	s.observeGroup(t.Context())
	if got := br.calloutStates(); got[len(got)-1] != true {
		t.Fatalf("callout states = %v; the leader keeps answering callouts", got)
	}
}

// TestHA_PromotionResumesPausedCallout: a standby that left the callout queue
// group because the leader granted workers differently answers worker connects
// as soon as it leads, not at its group observer's next tick. The lease is long
// here, so that tick is ten seconds after the first observation, well after the
// promotion.
func TestHA_PromotionResumesPausedCallout(t *testing.T) {
	h := startHarness(t, haSetup{
		ha:        true,
		mutateCfg: func(c *Config) { c.HA.LeaseTTL = 30 * time.Second },
		seed: func(st *haStore) {
			ctx := context.Background()
			if err := st.Store.UpsertServerMember(ctx, store.ServerMember{
				InstanceID: "other-instance", Name: "alpha", Host: "elsewhere",
				GrantsFingerprint: "different", StartedAt: time.Now().UTC(), BrokerOK: true,
			}); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := st.Store.AcquireLeadership(ctx, "other-instance", 2*time.Second); err != nil || !ok {
				t.Fatalf("hold the lease: ok %v, %v", ok, err)
			}
		},
	})
	waitFor(t, 10*time.Second, "the standby to leave the callout group", func() bool {
		return slices.Contains(h.br.calloutStates(), false)
	})
	h.waitLeader()
	if got := h.br.calloutStates(); got[len(got)-1] != true {
		t.Fatalf("callout states = %v once leading; the new leader must answer callouts at once", got)
	}
}

func TestHA_StandbyTracksLeaderName(t *testing.T) {
	st := newHAStore(newRecorder())
	s, _ := observeServer(t, st, &fakeHABroker{rec: newRecorder()}, "")

	s.observeGroup(t.Context())
	if got := s.ha.role.info(); got.Role != "standby" || got.Leader != "" || got.Epoch != 0 {
		t.Fatalf("role with no lease = %+v; want a standby naming no leader", got)
	}
	holdLease(t, st, "alpha", "")
	s.observeGroup(t.Context())
	if got := s.ha.role.info(); got.Role != "standby" || got.Leader != "alpha" || got.Epoch != 1 {
		t.Fatalf("role = %+v; want a standby naming alpha at epoch 1", got)
	}
	if got := metricValue(t, s.metrics, "sqi_server_ha_epoch"); got != 1 {
		t.Errorf("sqi_server_ha_epoch = %v, want the observed 1", got)
	}

	// A lapsed lease names no leader.
	st.SetClock(func() int64 { return time.Now().Add(2 * time.Hour).UnixMicro() })
	s.observeGroup(t.Context())
	if got := s.ha.role.info(); got.Leader != "" {
		t.Fatalf("role with a lapsed lease = %+v; want no leader", got)
	}
}

func TestHA_StandbyDiagSubscribed(t *testing.T) {
	buf := diag.NewBuffer(100, nil)
	h := startHarness(t, haSetup{ha: true, diagBuf: buf, seed: func(st *haStore) { holdLease(t, st, "alpha", "") }})
	h.waitHTTP("/readyz", http.StatusOK)

	nc, err := nats.Connect(h.br.emb.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	msg, err := json.Marshal(protocol.DiagLogMsg{Ts: time.Now().UTC(), Level: "INFO", Msg: "hello from a worker"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "a worker diagnostic on the standby", func() bool {
		if err := nc.Publish(bus.SubjectWorkerDiagPrefix+".w1", msg); err != nil {
			t.Fatal(err)
		}
		return len(buf.Query(diag.Filter{Component: "worker:w1"})) > 0
	})
	if h.srv.ha.role.isLeader() || h.rec.count("Acquire") != 0 {
		t.Fatal("the server under test led; the record must have reached a standby")
	}
}

// TestHA_LeaderPrunesMembers: the leader deletes members unseen for a day on
// its sweeper's tick.
func TestHA_LeaderPrunesMembers(t *testing.T) {
	h := startHarness(t, haSetup{ha: true, seed: func(st *haStore) {
		st.SetClock(func() int64 { return time.Now().Add(-48 * time.Hour).UnixMicro() })
		if err := st.Store.UpsertServerMember(context.Background(), store.ServerMember{InstanceID: "long-gone", Name: "old"}); err != nil {
			t.Fatal(err)
		}
		st.SetClock(nil)
	}})
	h.waitLeader()
	waitFor(t, 10*time.Second, "member pruning", func() bool {
		members, _, err := h.st.ListServerMembers(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return !slices.ContainsFunc(members, func(m store.ServerMember) bool { return m.InstanceID == "long-gone" })
	})
	members, _, err := h.st.ListServerMembers(context.Background())
	if err != nil || len(members) != 1 || members[0].Name != "self" {
		t.Fatalf("members = %+v, %v; want only this server", members, err)
	}
}

// TestHA_RefusedStandbyExits: a broker check that refuses the standby is
// final, and the server exits like an at-start refusal, not a demotion.
func TestHA_RefusedStandbyExits(t *testing.T) {
	h := startHarness(t, haSetup{ha: true, brokerDown: true})
	h.waitHTTP("/healthz", http.StatusOK)

	refused := errors.New("bus: the account is held by a server of another database")
	h.br.mu.Lock()
	h.br.readyErr = refused
	close(h.br.checked)
	h.br.mu.Unlock()

	err := h.wait(30 * time.Second)
	if !errors.Is(err, refused) || errors.Is(err, ErrDemoted) {
		t.Fatalf("Run = %v; want the refusal, not a demotion", err)
	}
	if n := h.rec.count("AcquireAttempt"); n != 0 {
		t.Fatalf("a refused standby tried the lease %d times", n)
	}
}

// TestBounded: f runs with a deadline d from the call, under the parent's
// values, and the context ends when f returns.
func TestBounded(t *testing.T) {
	type key struct{}
	parent := context.WithValue(context.Background(), key{}, "v")
	var done <-chan struct{}
	start := time.Now()
	bounded(parent, time.Minute, func(ctx context.Context) {
		done = ctx.Done()
		deadline, ok := ctx.Deadline()
		if !ok || deadline.Before(start.Add(time.Minute)) || deadline.After(time.Now().Add(time.Minute)) {
			t.Errorf("deadline = %v (set %v), want a minute from the call", deadline, ok)
		}
		if ctx.Value(key{}) != "v" {
			t.Error("the parent's values are lost")
		}
	})
	select {
	case <-done:
	default:
		t.Error("the context outlived f")
	}
}
