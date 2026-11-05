// SPDX-License-Identifier: AGPL-3.0-or-later

package ha_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/ha"
	"github.com/uberware/sqi/internal/store"
)

// validity is how long one successful renewal keeps a leader's authority.
const validity = testTTL * 4 / 5

func TestElector_ExactlyOneLeaderUnderContention(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const n = 5
	electors := make([]*ha.Elector, n)
	results := make([]<-chan campaignResult, n)
	for i := range electors {
		electors[i] = h.elector(string(rune('a' + i)))
	}
	var start sync.WaitGroup
	start.Add(1)
	for i, e := range electors {
		out := make(chan campaignResult, 1)
		results[i] = out
		go func() {
			start.Wait()
			epoch, err := e.Campaign(ctx)
			out <- campaignResult{epoch, err}
		}()
	}
	start.Done()

	// The losers wait on their poll timers; keep polling past several rounds
	// while the winner's lease is still current.
	h.step(t, 3*time.Second, 500*time.Millisecond, n-1, nil, nil)

	// The winner's result may still be on its way; the losers are parked
	// on their poll timers and have none.
	winners := 0
	winner := -1
	deadline := time.Now().Add(settleTimeout)
	for winners == 0 && time.Now().Before(deadline) {
		for i, ch := range results {
			select {
			case r := <-ch:
				if r.err != nil {
					t.Fatalf("elector %d: Campaign: %v", i, r.err)
				}
				if r.epoch != 1 {
					t.Errorf("elector %d won epoch %d, want 1", i, r.epoch)
				}
				winners++
				winner = i
			default:
			}
		}
		time.Sleep(time.Millisecond)
	}
	if winners != 1 {
		t.Fatalf("%d electors won the lease, want exactly 1", winners)
	}
	if got := h.lease(t); got.Holder != string(rune('a'+winner)) || got.Epoch != 1 {
		t.Errorf("lease = %+v, want held by the winner at epoch 1", got)
	}
	for i, e := range electors {
		want := int64(0)
		if i == winner {
			want = 1
		}
		if got := e.Epoch(); got != want {
			t.Errorf("elector %d Epoch() = %d, want %d", i, got, want)
		}
	}

	cancel()
	for i, ch := range results {
		if i == winner {
			continue
		}
		select {
		case r := <-ch:
			if !errors.Is(r.err, context.Canceled) || r.epoch != 0 {
				t.Errorf("elector %d after cancel: epoch %d, err %v; want 0, context.Canceled", i, r.epoch, r.err)
			}
		case <-time.After(settleTimeout):
			t.Fatalf("elector %d did not return after cancel", i)
		}
	}
}

func TestElector_CampaignWinsAfterExpiry(t *testing.T) {
	h := newHarness(t)
	failover(t, h, h.elector("a"), h.elector("b"))
}

func TestElector_WallClockSkewIrrelevant(t *testing.T) {
	const skew = 10 * time.Minute
	for _, tc := range []struct {
		name          string
		leader, other time.Duration
	}{
		{"leader ahead", skew, -skew},
		{"leader behind", -skew, skew},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			a := h.elector("a", withClock(skewedClock{h.clk, tc.leader}))
			b := h.elector("b", withClock(skewedClock{h.clk, tc.other}))
			failover(t, h, a, b)
		})
	}
}

// failover has a win and hold the lease while b campaigns, shows b cannot
// win while a renews, then stops a's renewals and shows b takes over on the
// first poll after the lease lapses on the database clock: within ttl + 1s.
func failover(t *testing.T, h *harness, a, b *ha.Elector) {
	t.Helper()
	epochA := campaign(t, a)

	holdCtx, stopHold := context.WithCancel(t.Context())
	defer stopHold()
	held := holdAsync(holdCtx, a, epochA)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	won := campaignAsync(ctx, b)

	// a's hold timer and b's poll timer: two lease lifetimes of renewal.
	h.step(t, 2*testTTL, 500*time.Millisecond, 2, nil, returned(won))
	select {
	case r := <-won:
		t.Fatalf("b won epoch %d (err %v) while a was renewing", r.epoch, r.err)
	default:
	}
	notReturned(t, held, "while renewing")

	// a stops renewing without releasing: a crash, as far as b can tell.
	stopHold()
	select {
	case err := <-held:
		if err != nil {
			t.Fatalf("Hold after cancel = %v, want nil", err)
		}
	case <-time.After(settleTimeout):
		t.Fatal("Hold did not return after cancel")
	}
	h.st.waitIdle(t)
	expires := h.lease(t).ExpiresUS

	h.step(t, 2*testTTL, 250*time.Millisecond, 1, nil, returned(won))
	var r campaignResult
	select {
	case r = <-won:
	case <-time.After(settleTimeout):
		t.Fatal("b never won the lapsed lease")
	}
	if r.err != nil || r.epoch != epochA+1 {
		t.Fatalf("b's Campaign = %d, %v; want epoch %d", r.epoch, r.err, epochA+1)
	}
	l := h.lease(t)
	acquiredUS := l.ExpiresUS - testTTL.Microseconds()
	if l.Holder != "b" {
		t.Fatalf("lease holder = %q, want b", l.Holder)
	}
	if acquiredUS <= expires {
		t.Errorf("b took the lease %v before a's expired", time.Duration(expires-acquiredUS)*time.Microsecond)
	}
	if late := time.Duration(acquiredUS-expires) * time.Microsecond; late > time.Second {
		t.Errorf("b took the lease %v after a's expired, want within one 1s poll", late)
	}
}

func TestElector_HoldSelfFencesAtEightyPercent(t *testing.T) {
	h := newHarness(t)
	started := make(chan struct{}, 8)
	h.st.renew = func(ctx context.Context, _ string, _ int64, _ time.Duration) (bool, error) {
		started <- struct{}{}
		<-ctx.Done() // the database never answers
		return false, ctx.Err()
	}
	a := h.elector("a")
	acquiredAt := h.clk.Now()
	epoch := campaign(t, a)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := holdAsync(ctx, a, epoch)

	h.clk.waitTimers(t, 1)
	h.clk.Advance(testTTL / 3) // first renew sent, and never answered
	select {
	case <-started:
	case <-time.After(settleTimeout):
		t.Fatal("no renew was sent at ttl/3")
	}
	h.clk.waitPending(t, 1)
	h.clk.Advance(acquiredAt.Add(validity).Sub(h.clk.Now()) - time.Millisecond)
	h.clk.waitPending(t, 1)
	notReturned(t, done, "1ms before the self-fence deadline")
	if got := a.Epoch(); got != epoch {
		t.Errorf("Epoch() before the deadline = %d, want %d", got, epoch)
	}

	h.clk.Advance(time.Millisecond)
	lost := lostErr(t, done, settleTimeout)
	if lost.Reason != ha.ReasonDeadline {
		t.Fatalf("reason = %q, want %q (err %v)", lost.Reason, ha.ReasonDeadline, lost)
	}
	if !h.clk.Now().Equal(acquiredAt.Add(validity)) {
		t.Errorf("self-fenced at +%v, want +%v", h.clk.Now().Sub(acquiredAt), validity)
	}
	if got := a.Epoch(); got != 0 {
		t.Errorf("Epoch() after losing = %d, want 0", got)
	}
	if n := len(started); n != 0 {
		t.Errorf("%d more renews were sent while one was in flight, want none", n)
	}
	h.st.waitIdle(t) // the abandoned renew's context was canceled
}

func TestElector_HoldToleratesTwoFailedRenews(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	calls := 0
	h.st.renew = func(ctx context.Context, holder string, epoch int64, ttl time.Duration) (bool, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n <= 2 {
			return false, errors.New("connection refused")
		}
		return h.db.RenewLeadership(ctx, holder, epoch, ttl)
	}
	a := h.elector("a")
	epoch := campaign(t, a)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := holdAsync(ctx, a, epoch)

	h.step(t, 3*testTTL, 250*time.Millisecond, 1, nil, returned(done))
	notReturned(t, done, "after two failed renews")
	if n := h.st.renews.Load(); n < 5 {
		t.Errorf("%d renews over three lease lifetimes, want renewal to resume", n)
	}
	l := h.lease(t)
	if l.Holder != "a" || l.Epoch != epoch || l.ExpiresUS <= l.NowUS {
		t.Errorf("lease = %+v, want a still holding epoch %d, unexpired", l, epoch)
	}
}

func TestElector_HoldDeadlineWrapsLastRenewError(t *testing.T) {
	h := newHarness(t)
	unreachable := errors.New("connection refused")
	h.st.renew = func(context.Context, string, int64, time.Duration) (bool, error) {
		return false, unreachable
	}
	a := h.elector("a")
	acquiredAt := h.clk.Now()
	epoch := campaign(t, a)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := holdAsync(ctx, a, epoch)

	h.step(t, testTTL, 250*time.Millisecond, 1, nil, returned(done))
	lost := lostErr(t, done, settleTimeout)
	if lost.Reason != ha.ReasonDeadline {
		t.Fatalf("reason = %q, want %q", lost.Reason, ha.ReasonDeadline)
	}
	if !errors.Is(lost, unreachable) {
		t.Errorf("err = %v, want it to wrap the last renew error", lost)
	}
	if elapsed := h.clk.Now().Sub(acquiredAt); elapsed != validity {
		t.Errorf("self-fenced at +%v, want +%v", elapsed, validity)
	}
	// The first renewal at 5s, then retries every 1.5s: 6.5s, 8s, 9.5s, 11s.
	if n := h.st.renews.Load(); n != 5 {
		t.Errorf("%d renews before the deadline, want 5 (failed renews retried every ttl/10)", n)
	}
}

func TestElector_HoldRenewRefused(t *testing.T) {
	h := newHarness(t)
	a := h.elector("a")
	epoch := campaign(t, a)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := holdAsync(ctx, a, epoch)
	h.clk.waitTimers(t, 1)

	// Another server holds the lease now, at a later epoch.
	bg := context.Background()
	if err := h.db.ReleaseLeadership(bg, "a", epoch); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := h.db.AcquireLeadership(bg, "b", testTTL); err != nil || !ok {
		t.Fatalf("b's acquire: ok %v, err %v", ok, err)
	}

	h.clk.Advance(testTTL / 3)
	lost := lostErr(t, done, settleTimeout)
	if lost.Reason != ha.ReasonRenewRefused {
		t.Fatalf("reason = %q, want %q (err %v)", lost.Reason, ha.ReasonRenewRefused, lost)
	}
	if got := a.Epoch(); got != 0 {
		t.Errorf("Epoch() = %d, want 0", got)
	}
	if l := h.lease(t); l.Holder != "b" {
		t.Errorf("lease holder = %q, want b untouched", l.Holder)
	}
}

func TestElector_LoseStopsHold(t *testing.T) {
	h := newHarness(t)
	var logs bytes.Buffer
	a := h.elector("a", withLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	epoch := campaign(t, a)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := holdAsync(ctx, a, epoch)
	h.clk.waitTimers(t, 1)

	a.Lose(ha.ReasonFenced, store.ErrFenced)
	a.Lose(ha.ReasonGuardLost, errors.New("ignored: the first loss stands"))
	lost := lostErr(t, done, 100*time.Millisecond)
	if lost.Reason != ha.ReasonFenced {
		t.Fatalf("reason = %q, want %q", lost.Reason, ha.ReasonFenced)
	}
	if !errors.Is(lost, store.ErrFenced) {
		t.Errorf("err = %v, want it to wrap store.ErrFenced", lost)
	}
	if got := a.Epoch(); got != 0 {
		t.Errorf("Epoch() = %d, want 0", got)
	}
	out := logs.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, string(ha.ReasonFenced)) {
		t.Errorf("logs = %q, want one ERROR naming the reason", out)
	}
	if n := strings.Count(out, "level=ERROR"); n != 1 {
		t.Errorf("%d ERROR lines, want 1", n)
	}

	// A loss is final: a later Hold reports the same loss at once.
	if reason := lostErr(t, holdAsync(ctx, a, epoch), 100*time.Millisecond).Reason; reason != ha.ReasonFenced {
		t.Errorf("Hold after the loss: reason %q, want %q", reason, ha.ReasonFenced)
	}
}

func TestElector_LoseBeforeHold(t *testing.T) {
	h := newHarness(t)
	a := h.elector("a")
	epoch := campaign(t, a)
	a.Lose(ha.ReasonPromotion, errors.New("promotion step failed"))
	if got := a.Epoch(); got != 0 {
		t.Errorf("Epoch() after Lose = %d, want 0", got)
	}
	reason := lostErr(t, holdAsync(context.Background(), a, epoch), 100*time.Millisecond).Reason
	if reason != ha.ReasonPromotion {
		t.Errorf("reason = %q, want %q", reason, ha.ReasonPromotion)
	}
}

func TestElector_BrokerLostStepsDownOnlyIfStandbyHasBroker(t *testing.T) {
	upsert := func(t *testing.T, h *harness, id string, brokerOK bool) {
		t.Helper()
		if err := h.db.UpsertServerMember(context.Background(), store.ServerMember{InstanceID: id, Name: id, BrokerOK: brokerOK}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("no standby with a broker keeps holding", func(t *testing.T) {
		h := newHarness(t)
		bus := &broker{up: true, since: h.t0}
		a := h.elector("a", withBroker(bus))
		epoch := campaign(t, a)
		bus.set(false, h.t0) // the leader's broker drops as it wins

		upsert(t, h, "a", true)   // the leader's own stamp never counts
		upsert(t, h, "s1", false) // a standby that never reached the broker
		upsert(t, h, "s2", true)  // one whose stamp is stale before it matters

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := holdAsync(ctx, a, epoch)
		h.step(t, 3*testTTL, 500*time.Millisecond, 1, nil, returned(done))
		notReturned(t, done, "with no standby able to reach the broker")
		if l := h.lease(t); l.Holder != "a" || l.Epoch != epoch {
			t.Fatalf("lease = %+v, want a still holding", l)
		}

		// A standby reaches the broker: stepping down now helps.
		upsert(t, h, "s1", true)
		h.step(t, testTTL/3+time.Second, 250*time.Millisecond, 1, nil, returned(done))
		lost := lostErr(t, done, settleTimeout)
		if lost.Reason != ha.ReasonBrokerLost {
			t.Fatalf("reason = %q, want %q (err %v)", lost.Reason, ha.ReasonBrokerLost, lost)
		}
		if l := h.lease(t); l.Holder != "" {
			t.Errorf("lease holder = %q, want released for the standby", l.Holder)
		}
		if got := h.st.released(); len(got) != 1 || got[0] != epoch {
			t.Errorf("released epochs = %v, want [%d]", got, epoch)
		}
		if got := a.Epoch(); got != 0 {
			t.Errorf("Epoch() = %d, want 0", got)
		}
	})

	// stepDownAfter runs a leader whose broker follows script (consulted
	// after every 250ms step, by elapsed time) with a standby that always has
	// a fresh broker stamp, and returns how long after the win it released.
	stepDownAfter := func(t *testing.T, script func(elapsed time.Duration) bool) time.Duration {
		t.Helper()
		h := newHarness(t)
		bus := &broker{up: true, since: h.t0}
		a := h.elector("a", withBroker(bus))
		epoch := campaign(t, a)
		// What bus.Client.BusUpSince reports while disconnected: up false,
		// since zero. A zero since must not read as an outage of forever.
		bus.set(script(0), time.Time{})
		var releasedAt time.Time
		h.st.release = func(ctx context.Context, holder string, e int64) error {
			releasedAt = h.clk.Now()
			return h.db.ReleaseLeadership(ctx, holder, e)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := holdAsync(ctx, a, epoch)
		upsert(t, h, "s1", true)
		h.step(t, 3*testTTL, 250*time.Millisecond, 1,
			func() {
				upsert(t, h, "s1", true) // a standby with a broker throughout
				bus.set(script(h.clk.Now().Sub(h.t0)), time.Time{})
			},
			returned(done))
		lost := lostErr(t, done, settleTimeout)
		if lost.Reason != ha.ReasonBrokerLost {
			t.Fatalf("reason = %q, want %q (err %v)", lost.Reason, ha.ReasonBrokerLost, lost)
		}
		return releasedAt.Sub(h.t0)
	}

	t.Run("only after the broker is seen down longer than ttl", func(t *testing.T) {
		// Down from the start. Renewals run every 5s, and the first to see
		// the broker down is at 5s, which starts the outage clock. At 20s
		// it has been seen down exactly ttl, which is not longer than it;
		// the renewal at 25s steps down.
		got := stepDownAfter(t, func(time.Duration) bool { return false })
		if want := testTTL/3 + testTTL + testTTL/3; got != want {
			t.Errorf("stepped down %v after the win, want %v (ttl after the first observation, at the next renewal)", got, want)
		}
	})

	t.Run("a broker that comes back resets the outage", func(t *testing.T) {
		// Seen down at 5s, seen up at 10s, down again from 12s: first seen
		// down again at 15s, so the renewal at 35s is the first past ttl.
		got := stepDownAfter(t, func(elapsed time.Duration) bool {
			return elapsed >= 7*time.Second && elapsed < 12*time.Second
		})
		if want := 15*time.Second + testTTL + testTTL/3; got != want {
			t.Errorf("stepped down %v after the win, want %v (ttl after the outage seen to restart)", got, want)
		}
	})

	t.Run("a standby stamp older than ttl does not count", func(t *testing.T) {
		h := newHarness(t)
		bus := &broker{up: true, since: h.t0}
		a := h.elector("a", withBroker(bus))
		epoch := campaign(t, a)
		bus.set(false, h.t0)
		upsert(t, h, "s1", true)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := holdAsync(ctx, a, epoch)
		// s1 keeps checking in, but has not seen the broker since t0.
		h.step(t, 3*testTTL, 500*time.Millisecond, 1,
			func() { upsert(t, h, "s1", false) },
			returned(done))
		notReturned(t, done, "with only a stale standby broker stamp")
	})

	t.Run("a leader whose broker is up never steps down", func(t *testing.T) {
		h := newHarness(t)
		a := h.elector("a")
		epoch := campaign(t, a)
		upsert(t, h, "s1", true)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := holdAsync(ctx, a, epoch)
		h.step(t, 3*testTTL, 500*time.Millisecond, 1,
			func() { upsert(t, h, "s1", true) },
			returned(done))
		notReturned(t, done, "with its broker up")
	})
}

func TestElector_NoCampaignWithoutBroker(t *testing.T) {
	h := newHarness(t)
	bus := &broker{up: false, since: h.t0}
	a := h.elector("a", withBroker(bus))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	won := campaignAsync(ctx, a)

	h.step(t, 10*time.Second, time.Second, 1, nil, returned(won))
	if n := h.st.acquires.Load(); n != 0 {
		t.Fatalf("%d acquire attempts without a broker, want none", n)
	}
	select {
	case r := <-won:
		t.Fatalf("Campaign returned %d, %v without a broker", r.epoch, r.err)
	default:
	}

	bus.set(true, h.clk.Now())
	h.clk.Advance(time.Second)
	select {
	case r := <-won:
		if r.err != nil || r.epoch != 1 {
			t.Fatalf("Campaign = %d, %v; want epoch 1", r.epoch, r.err)
		}
	case <-time.After(settleTimeout):
		t.Fatal("Campaign did not acquire within one poll of the broker returning")
	}
	if n := h.st.acquires.Load(); n != 1 {
		t.Errorf("%d acquire attempts, want 1", n)
	}
}

func TestElector_CampaignDiscardsSlowWin(t *testing.T) {
	for _, tc := range []struct {
		name      string
		slow      time.Duration
		wantEpoch int64
		released  []int64
	}{
		{"answered just inside the window", validity - time.Millisecond, 1, nil},
		{"answered at the self-fence deadline", validity, 2, []int64{1}},
		{"answered after the lease lapsed", testTTL + time.Second, 2, []int64{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			var mu sync.Mutex
			first := true
			h.st.acquire = func(ctx context.Context, holder string, ttl time.Duration) (int64, bool, error) {
				epoch, ok, err := h.db.AcquireLeadership(ctx, holder, ttl)
				mu.Lock()
				defer mu.Unlock()
				if first {
					first = false
					h.clk.Advance(tc.slow) // the win comes back late
				}
				return epoch, ok, err
			}
			a := h.elector("a")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			won := campaignAsync(ctx, a)
			h.step(t, 2*time.Second, 500*time.Millisecond, 1, nil, returned(won))

			r := <-won
			if r.err != nil || r.epoch != tc.wantEpoch {
				t.Fatalf("Campaign = %d, %v; want epoch %d", r.epoch, r.err, tc.wantEpoch)
			}
			if got := h.st.released(); !equalInts(got, tc.released) {
				t.Errorf("released epochs = %v, want %v", got, tc.released)
			}
			if got := a.Epoch(); got != tc.wantEpoch {
				t.Errorf("Epoch() = %d, want %d", got, tc.wantEpoch)
			}
		})
	}
}

func equalInts(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestElector_CampaignSurvivesAcquireErrors(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	calls := 0
	h.st.acquire = func(ctx context.Context, holder string, ttl time.Duration) (int64, bool, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			return 0, false, errors.New("connection refused")
		}
		return h.db.AcquireLeadership(ctx, holder, ttl)
	}
	a := h.elector("a")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	won := campaignAsync(ctx, a)
	h.clk.waitTimers(t, 1)
	h.clk.Advance(time.Second)
	select {
	case r := <-won:
		if r.err != nil || r.epoch != 1 {
			t.Fatalf("Campaign = %d, %v; want epoch 1", r.epoch, r.err)
		}
	case <-time.After(settleTimeout):
		t.Fatal("Campaign did not retry after an acquire error")
	}
}

func TestElector_CampaignCanceled(t *testing.T) {
	h := newHarness(t)
	if _, ok, err := h.db.AcquireLeadership(context.Background(), "other", testTTL); err != nil || !ok {
		t.Fatalf("seed lease: ok %v, err %v", ok, err)
	}
	a := h.elector("a")
	ctx, cancel := context.WithCancel(t.Context())
	won := campaignAsync(ctx, a)
	h.clk.waitTimers(t, 1)
	cancel()
	select {
	case r := <-won:
		if !errors.Is(r.err, context.Canceled) || r.epoch != 0 {
			t.Errorf("Campaign = %d, %v; want 0, context.Canceled", r.epoch, r.err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Campaign did not return promptly on cancel")
	}

	// An already-ended context never reaches the store.
	before := h.st.acquires.Load()
	if _, err := a.Campaign(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Campaign on a canceled context = %v, want context.Canceled", err)
	}
	if h.st.acquires.Load() != before {
		t.Error("Campaign on a canceled context tried to acquire")
	}
}

func TestElector_HoldEndsWithContextAndRelease(t *testing.T) {
	h := newHarness(t)
	var logs bytes.Buffer
	a := h.elector("a", withLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	epoch := campaign(t, a)
	ctx, cancel := context.WithCancel(t.Context())
	done := holdAsync(ctx, a, epoch)
	h.step(t, testTTL, 500*time.Millisecond, 1, nil, returned(done))
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Hold after cancel = %v (%T), want nil", err, err)
		}
	case <-time.After(settleTimeout):
		t.Fatal("Hold did not return after cancel")
	}
	if got := a.Epoch(); got != epoch {
		t.Errorf("Epoch() after a graceful Hold end = %d, want %d until Release", got, epoch)
	}

	if err := a.Release(context.Background()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if l := h.lease(t); l.Holder != "" || l.Epoch != epoch {
		t.Errorf("lease after Release = %+v, want released at epoch %d", l, epoch)
	}
	if got := a.Epoch(); got != 0 {
		t.Errorf("Epoch() after Release = %d, want 0", got)
	}
	if err := a.Release(context.Background()); err != nil {
		t.Errorf("second Release: %v", err)
	}
	if got := h.st.released(); len(got) != 1 {
		t.Errorf("released epochs = %v, want one release", got)
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("a graceful end logged an ERROR: %q", logs.String())
	}
}

func TestElector_ReleaseError(t *testing.T) {
	h := newHarness(t)
	boom := errors.New("connection refused")
	h.st.release = func(context.Context, string, int64) error { return boom }
	a := h.elector("a")
	campaign(t, a)
	if err := a.Release(context.Background()); !errors.Is(err, boom) {
		t.Errorf("Release = %v, want it to wrap the store error", err)
	}
}

func TestElector_ReleaseWithoutLeading(t *testing.T) {
	h := newHarness(t)
	if err := h.elector("a").Release(context.Background()); err != nil {
		t.Errorf("Release without leading = %v, want nil", err)
	}
	if got := h.st.released(); len(got) != 0 {
		t.Errorf("released epochs = %v, want no store call", got)
	}
}

func TestElector_HoldWithoutAuthority(t *testing.T) {
	h := newHarness(t)
	a := h.elector("a")
	reason := lostErr(t, holdAsync(context.Background(), a, 7), 100*time.Millisecond).Reason
	if reason != ha.ReasonDeadline {
		t.Errorf("Hold of an epoch never won: reason %q, want %q", reason, ha.ReasonDeadline)
	}
	if n := h.st.renews.Load(); n != 0 {
		t.Errorf("%d renews of an epoch never won, want none", n)
	}
}

func TestLostError(t *testing.T) {
	cause := errors.New("cause")
	withCause := &ha.LostError{Reason: ha.ReasonGuardLost, Err: cause}
	if !strings.Contains(withCause.Error(), "guard_lost") || !strings.Contains(withCause.Error(), "cause") {
		t.Errorf("Error() = %q, want the reason and the cause", withCause.Error())
	}
	if !errors.Is(withCause, cause) {
		t.Error("errors.Is does not reach the cause")
	}
	bare := &ha.LostError{Reason: ha.ReasonRenewRefused}
	if !strings.Contains(bare.Error(), "renew_refused") {
		t.Errorf("Error() = %q, want the reason", bare.Error())
	}
	if bare.Unwrap() != nil {
		t.Error("Unwrap of a loss without a cause is not nil")
	}
}

func TestElector_RealClock(t *testing.T) {
	db := newHarness(t).db
	db.SetClock(nil) // the real clock, on both sides
	a := ha.New(ha.Config{
		Self:     store.ServerMember{InstanceID: "a"},
		LeaseTTL: 900 * time.Millisecond, // renewals every 300ms, authority 720ms
		Logger:   slog.New(slog.DiscardHandler),
	}, db)
	ctx, cancel := context.WithTimeout(context.Background(), settleTimeout)
	defer cancel()
	epoch, err := a.Campaign(ctx)
	if err != nil {
		t.Fatalf("Campaign: %v", err)
	}
	won, err := db.GetLeadership(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Ending before the first authority window closes keeps the test immune
	// to a slow host; the lease shows whether renewals ran.
	holdCtx, stop := context.WithTimeout(ctx, 700*time.Millisecond)
	defer stop()
	if err := a.Hold(holdCtx, epoch); err != nil {
		t.Fatalf("Hold = %v, want nil", err)
	}
	if got := a.Epoch(); got != epoch {
		t.Errorf("Epoch() = %d, want %d", got, epoch)
	}
	if l, err := db.GetLeadership(ctx); err != nil || l.ExpiresUS <= won.ExpiresUS {
		t.Errorf("lease expiry %d after holding, want later than %d at the win (err %v)", l.ExpiresUS, won.ExpiresUS, err)
	}
}

func TestNew_RejectsNonPositiveTTL(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("New with a zero lease ttl did not panic")
		}
	}()
	ha.New(ha.Config{Self: store.ServerMember{InstanceID: "a"}}, newHarness(t).db)
}

func TestElector_ReleaseAfterLoss(t *testing.T) {
	for _, tc := range []struct {
		name string
		lose func(t *testing.T, h *harness, a *ha.Elector, epoch int64)
	}{
		{"reported loss", func(_ *testing.T, _ *harness, a *ha.Elector, _ int64) {
			a.Lose(ha.ReasonPromotion, errors.New("promotion step failed"))
		}},
		{"self-fence deadline", func(t *testing.T, h *harness, a *ha.Elector, epoch int64) {
			h.st.renew = func(context.Context, string, int64, time.Duration) (bool, error) {
				return false, errors.New("connection refused")
			}
			done := holdAsync(t.Context(), a, epoch)
			h.step(t, testTTL, 250*time.Millisecond, 1, nil, returned(done))
			if reason := lostErr(t, done, settleTimeout).Reason; reason != ha.ReasonDeadline {
				t.Fatalf("reason = %q, want %q", reason, ha.ReasonDeadline)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			a := h.elector("a")
			epoch := campaign(t, a)
			tc.lose(t, h, a, epoch)
			if got := a.Epoch(); got != 0 {
				t.Errorf("Epoch() after the loss = %d, want 0", got)
			}
			if err := a.Release(t.Context()); err != nil {
				t.Fatalf("Release after the loss: %v", err)
			}
			if l := h.lease(t); l.Holder != "" || l.Epoch != epoch {
				t.Errorf("lease = %+v, want released at epoch %d", l, epoch)
			}
			if got := h.st.released(); len(got) != 1 || got[0] != epoch {
				t.Errorf("released epochs = %v, want [%d]", got, epoch)
			}
			if err := a.Release(t.Context()); err != nil || len(h.st.released()) != 1 {
				t.Errorf("second Release: err %v, released %v; want a no-op", err, h.st.released())
			}
		})
	}
}

func TestElector_ReleaseAfterTakeoverLeavesNewLeader(t *testing.T) {
	h := newHarness(t)
	a := h.elector("a")
	epoch := campaign(t, a)
	bg := t.Context()
	if err := h.db.ReleaseLeadership(bg, "a", epoch); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := h.db.AcquireLeadership(bg, "b", testTTL); err != nil || !ok {
		t.Fatalf("b's acquire: ok %v, err %v", ok, err)
	}
	a.Lose(ha.ReasonFenced, store.ErrFenced)
	if err := a.Release(bg); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if l := h.lease(t); l.Holder != "b" || l.Epoch != epoch+1 {
		t.Errorf("lease = %+v, want b still holding epoch %d", l, epoch+1)
	}
}

func TestElector_LossRacingContextEnd(t *testing.T) {
	// Cancel and Lose land while Hold is between waits, so its select finds
	// both ready and picks one at random. Either way the loss must win: a
	// nil would make the server exit as if stopped, not demoted.
	for i := range 50 {
		h := newHarness(t)
		a := h.elector("a")
		epoch := campaign(t, a)
		ctx, cancel := context.WithCancel(t.Context())
		var once sync.Once
		ha.SetWaitHook(a, func(bool) {
			once.Do(func() {
				cancel()
				a.Lose(ha.ReasonGuardLost, errors.New("guard taken"))
			})
		})
		if reason := lostErr(t, holdAsync(ctx, a, epoch), settleTimeout).Reason; reason != ha.ReasonGuardLost {
			t.Fatalf("iteration %d: reason %q, want %q", i, reason, ha.ReasonGuardLost)
		}
		cancel()
	}
}

// Authorized is what a request handler asks, so it must be false the moment
// the self-fence deadline passes, not only once Hold's loop has noticed: a
// process that was frozen and has just resumed answers requests before its
// loop runs.
func TestElector_AuthorizedLapsesAtTheDeadlineWithoutHold(t *testing.T) {
	h := newHarness(t)
	a := h.elector("a")
	if a.Authorized() {
		t.Fatal("Authorized() before winning the lease")
	}
	acquiredAt := h.clk.Now()
	campaign(t, a)
	if !a.Authorized() {
		t.Fatal("Authorized() right after winning the lease = false")
	}

	// No Hold is running, as in a process that has not been scheduled since
	// it resumed.
	h.clk.Advance(acquiredAt.Add(validity).Sub(h.clk.Now()) - time.Millisecond)
	if !a.Authorized() {
		t.Error("Authorized() 1ms before the deadline = false")
	}
	h.clk.Advance(time.Millisecond)
	if a.Authorized() {
		t.Error("Authorized() at the deadline = true, want false")
	}
}

func TestElector_AuthorizedFollowsRenewals(t *testing.T) {
	h := newHarness(t)
	a := h.elector("a")
	acquiredAt := h.clk.Now()
	epoch := campaign(t, a)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := holdAsync(ctx, a, epoch)

	// Run well past the winning acquire's deadline; renewals keep extending it.
	h.step(t, validity+3*time.Second, 250*time.Millisecond, 1, nil, returned(done))
	if h.clk.Now().Before(acquiredAt.Add(validity)) {
		t.Fatalf("test clock at +%v, want past the acquire's deadline +%v", h.clk.Now().Sub(acquiredAt), validity)
	}
	notReturned(t, done, "after renewals extended the window")
	if !a.Authorized() {
		t.Error("Authorized() after renewals extended the window = false")
	}
}

func TestElector_AuthorizedFalseAfterLoss(t *testing.T) {
	h := newHarness(t)
	a := h.elector("a")
	campaign(t, a)
	a.Lose(ha.ReasonPromotion, errors.New("promotion step failed"))
	if a.Authorized() {
		t.Error("Authorized() after Lose = true")
	}
}
