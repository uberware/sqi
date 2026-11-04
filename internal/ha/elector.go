// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ha elects one leader among the sqi servers sharing a database. The
// authority is the lease row the store keeps ([store.LeadershipStore]): a
// standby polls to take it, the leader renews it, and every judgement of
// whether it has lapsed is made in SQL on the database's clock, so servers
// whose clocks disagree still agree on who leads.
//
// The leader also fences itself. It notes its own monotonic clock before
// sending each renewal and treats a success as authority for 80% of the lease
// from that instant. The database set the expiry no earlier than the request
// was sent, so a standby cannot take over before the full lease has passed;
// the remaining 20% absorbs clocks that run at slightly different rates and
// the leader's own reaction time. A leader whose deadline passes without a
// successful renewal stops on its own, before a standby can have started.
//
// A loss of authority is final: [Elector.Hold] returns a [*LostError] and the
// server exits, to be restarted as a standby.
package ha

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// campaignPoll is how often a standby tries to take the lease.
const campaignPoll = time.Second

// Clock is the elector's time source. Its readings are only ever compared
// with each other, never with another server's or the database's, so it
// needs to run at the right rate but not to agree with anyone; the real
// clock's readings carry a monotonic component for exactly that.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

// Timer is a one-shot timer from a [Clock], as [time.Timer] is.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

type realTimer struct{ t *time.Timer }

func (t realTimer) C() <-chan time.Time { return t.t.C }

func (t realTimer) Stop() bool { return t.t.Stop() }

// Config is an elector's identity and timing.
type Config struct {
	// Self is this server's membership row. Its InstanceID is the lease
	// holder name; LastSeenUS, BrokerOK and BrokerOKUS are set by the elector
	// and the store.
	Self store.ServerMember
	// LeaseTTL is the lease lifetime. Renewals run every third of it and a
	// renewal's authority lasts four fifths of it. It must be positive.
	LeaseTTL time.Duration
	// BrokerUp reports whether the bus connection is up now. Nil means always
	// up. The elector reads only up: it times an outage itself, on Clock,
	// from the first time it sees the connection down, so since may be
	// anything (bus.Client.BusUpSince reports zero while down, and a UTC wall
	// time while up) and is not used.
	BrokerUp func() (up bool, since time.Time)
	// Clock is nil for the real clock.
	Clock Clock
	// Logger is nil for [slog.Default].
	Logger *slog.Logger
}

// Elector campaigns for, holds and gives up the leader lease for one server.
// Its methods are safe for concurrent use; Campaign and Hold are meant to be
// called one after the other from a single goroutine.
type Elector struct {
	cfg   Config
	st    store.LeadershipStore
	clock Clock
	log   *slog.Logger

	// waitHook, set only by tests, is told before each wait in Hold whether
	// a renewal is in flight, and false when Hold returns.
	waitHook func(inFlight bool)

	mu        sync.Mutex
	epoch     int64     // the epoch led, 0 when not leading
	won       int64     // the last epoch won and not yet released, kept after a loss
	authAt    time.Time // when the last successful acquire or renewal was sent
	downSince time.Time // when the broker was first seen down, on clock; zero while up
	lost      *LostError
	lostCh    chan struct{} // closed when lost is set
}

// New returns an elector for cfg.Self over st. It panics when cfg.LeaseTTL is
// not positive, which configuration validation rules out.
func New(cfg Config, st store.LeadershipStore) *Elector {
	if cfg.LeaseTTL <= 0 {
		panic(fmt.Sprintf("ha: lease ttl %v must be positive", cfg.LeaseTTL))
	}
	if cfg.Clock == nil {
		cfg.Clock = realClock{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Elector{cfg: cfg, st: st, clock: cfg.Clock, log: cfg.Logger, lostCh: make(chan struct{})}
}

// RenewInterval is the time between renewals, and between membership
// upserts, for a group whose lease lasts ttl: a third of it. A server
// watching its group observes at the same cadence.
func RenewInterval(ttl time.Duration) time.Duration { return ttl / 3 }

// renewInterval is [RenewInterval] for this elector's lease.
func (e *Elector) renewInterval() time.Duration { return RenewInterval(e.cfg.LeaseTTL) }

// validity is how long a successful renewal, or the winning acquire, keeps
// the leader's authority, counted from when the request was sent.
func (e *Elector) validity() time.Duration { return e.cfg.LeaseTTL * 4 / 5 }

// retryInterval is how soon a failed renewal is retried. A failure waits less
// than a full renewInterval: with renewals every third of the lease and
// authority lasting four fifths of it, retrying only on the next tick would
// let a single failure followed by one more cost the lease. At this interval a
// leader rides out several consecutive failures inside its window.
func (e *Elector) retryInterval() time.Duration { return e.cfg.LeaseTTL / 10 }

// observeBroker reads the bus connection state at now and returns whether it
// is up and, when it is not, how long it has been seen down: from the first
// observation of it down, on the elector's own clock. Seeing it up resets
// that, so an outage is only as old as the elector has watched it
// continuously. Observations come from campaigning, membership upserts and
// renewals, so an outage is noticed within a third of the lease.
func (e *Elector) observeBroker(now time.Time) (up bool, downFor time.Duration) {
	up = true
	if e.cfg.BrokerUp != nil {
		up, _ = e.cfg.BrokerUp()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if up {
		e.downSince = time.Time{}
		return true, 0
	}
	if e.downSince.IsZero() {
		e.downSince = now
	}
	return false, now.Sub(e.downSince)
}

// Epoch returns the epoch this server leads at, or 0 when it does not lead.
func (e *Elector) Epoch() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.epoch
}

// Campaign tries to take the lease every second while the broker is up, and
// returns the epoch won. A standby without a broker never tries: it could do
// no work as leader. Campaign returns ctx.Err() when ctx ends first.
func (e *Elector) Campaign(ctx context.Context) (int64, error) {
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if up, _ := e.observeBroker(e.clock.Now()); up {
			if epoch, ok := e.tryAcquire(ctx); ok {
				return epoch, nil
			}
		}
		if err := e.sleep(ctx, campaignPoll); err != nil {
			return 0, err
		}
	}
}

// tryAcquire makes one attempt at the lease. A win that comes back after its
// own self-fence deadline (the acquire can wait behind the old leader's
// in-flight commits) carries no usable authority: it is released, and the
// next poll tries again.
func (e *Elector) tryAcquire(ctx context.Context) (int64, bool) {
	t0 := e.clock.Now()
	deadline := t0.Add(e.validity())
	actx, cancel := context.WithTimeout(ctx, e.validity())
	epoch, ok, err := e.st.AcquireLeadership(actx, e.cfg.Self.InstanceID, e.cfg.LeaseTTL)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			e.log.WarnContext(ctx, "ha: lease acquire failed", slog.Any("error", err))
		}
		return 0, false
	}
	if !ok {
		return 0, false
	}
	if now := e.clock.Now(); !now.Before(deadline) {
		e.log.WarnContext(ctx, "ha: lease won too late to act on; releasing it",
			slog.Int64("epoch", epoch), slog.Duration("took", now.Sub(t0)))
		e.release(ctx, epoch)
		return 0, false
	}
	e.mu.Lock()
	e.epoch, e.won, e.authAt = epoch, epoch, t0
	e.mu.Unlock()
	e.log.InfoContext(ctx, "ha: won leadership", slog.Int64("epoch", epoch))
	return epoch, true
}

// release gives up the lease at epoch, best effort: on failure the lease
// lapses on its own.
func (e *Elector) release(ctx context.Context, epoch int64) {
	rctx, cancel := context.WithTimeout(ctx, e.renewInterval())
	defer cancel()
	if err := e.st.ReleaseLeadership(rctx, e.cfg.Self.InstanceID, epoch); err != nil {
		e.log.WarnContext(ctx, "ha: lease release failed", slog.Int64("epoch", epoch), slog.Any("error", err))
	}
}

// sleep waits d on the elector's clock, or until ctx ends.
func (e *Elector) sleep(ctx context.Context, d time.Duration) error {
	t := e.clock.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C():
		return nil
	}
}

// holding is the state of one Hold.
type holding struct {
	epoch      int64
	validUntil time.Time // authority ends here unless a renewal extends it
	nextRenew  time.Time
	inFlight   bool  // a renewal has been sent and not answered
	lastErr    error // the last renewal's error, nil after a success
	results    chan renewal
}

// renewal is the answer to one renewal.
type renewal struct {
	t0         time.Time // when it was sent
	ok         bool
	err        error
	brokerLost bool // the broker-loss rule says step down
}

// Hold keeps the lease at epoch, which Campaign must have won, renewing it
// every third of the lease. It returns nil when ctx ends with no loss
// recorded, leaving the lease to lapse unless the caller calls Release, and
// otherwise a [*LostError] the moment authority is gone (the caller then
// calls Release and exits):
//
//   - a renewal is refused ([ReasonRenewRefused]);
//   - the self-fence deadline passes with no successful renewal
//     ([ReasonDeadline]); a renewal that errors is retried, and is not a loss
//     on its own;
//   - the broker has been seen down continuously for longer than the lease
//     while some standby reaches it ([ReasonBrokerLost]); the lease is
//     released first, so that standby takes over at its next poll;
//   - [Elector.Lose] reports a loss found elsewhere.
//
// A renewal that has not answered is never stacked with another, and the
// deadline is judged whether or not one is in flight.
func (e *Elector) Hold(ctx context.Context, epoch int64) error {
	e.mu.Lock()
	h := &holding{epoch: epoch, results: make(chan renewal, 1)}
	if epoch != 0 && epoch == e.epoch {
		h.validUntil = e.authAt.Add(e.validity())
		h.nextRenew = e.authAt.Add(e.renewInterval())
	}
	e.mu.Unlock()

	renewCtx, cancel := context.WithCancel(ctx)
	defer cancel() // abandons a renewal still in flight
	defer e.hookWait(false)
	for {
		if lost := e.loss(); lost != nil {
			return lost
		}
		select {
		case r := <-h.results:
			if err := e.settle(ctx, h, r); err != nil {
				return err
			}
		default:
		}
		now := e.clock.Now()
		if !now.Before(h.validUntil) {
			e.markLost(ctx, ReasonDeadline, h.deadlineErr(e.validity()))
			return e.loss()
		}
		if !h.inFlight && !now.Before(h.nextRenew) {
			h.inFlight = true
			go e.renew(renewCtx, h.epoch, now, h.results)
		}
		if done, err := e.wait(ctx, h, now); done {
			return err
		}
	}
}

// wait blocks until the next thing Hold must act on: the next renewal, the
// deadline, a renewal's answer, a loss, or the end of ctx. done is true when
// Hold must return err.
func (e *Elector) wait(ctx context.Context, h *holding, now time.Time) (done bool, err error) {
	wake := h.validUntil
	if !h.inFlight && h.nextRenew.Before(wake) {
		wake = h.nextRenew
	}
	e.hookWait(h.inFlight)
	t := e.clock.NewTimer(wake.Sub(now))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true, e.loss() // a loss recorded as ctx ended still counts
	case <-e.lostCh:
		return false, nil // the next pass returns the loss
	case r := <-h.results:
		if err := e.settle(ctx, h, r); err != nil {
			return true, err
		}
		return false, nil
	case <-t.C():
		return false, nil
	}
}

// hookWait calls the test hook, when there is one.
func (e *Elector) hookWait(inFlight bool) {
	if e.waitHook != nil {
		e.waitHook(inFlight)
	}
}

// settle applies a renewal's answer, returning the loss it means, if any.
func (e *Elector) settle(ctx context.Context, h *holding, r renewal) error {
	h.inFlight = false
	if r.err != nil {
		h.lastErr = r.err
		h.nextRenew = r.t0.Add(e.retryInterval())
		if ctx.Err() == nil {
			e.log.WarnContext(ctx, "ha: lease renewal failed",
				slog.Int64("epoch", h.epoch), slog.Any("error", r.err))
		}
		return nil
	}
	if !r.ok {
		e.markLost(ctx, ReasonRenewRefused, fmt.Errorf("the lease is no longer held at epoch %d", h.epoch))
		return e.loss()
	}
	h.lastErr = nil
	if v := r.t0.Add(e.validity()); v.After(h.validUntil) {
		h.validUntil = v
		e.mu.Lock()
		e.authAt = r.t0
		e.mu.Unlock()
	}
	h.nextRenew = r.t0.Add(e.renewInterval())
	if r.brokerLost {
		e.release(ctx, h.epoch)
		e.markLost(ctx, ReasonBrokerLost,
			fmt.Errorf("broker unreachable for longer than %v while a standby reaches it", e.cfg.LeaseTTL))
		return e.loss()
	}
	return nil
}

// deadlineErr describes a lapsed authority window.
func (h *holding) deadlineErr(validity time.Duration) error {
	if h.lastErr != nil {
		return fmt.Errorf("no lease renewal succeeded within %v: %w", validity, h.lastErr)
	}
	if h.validUntil.IsZero() {
		return fmt.Errorf("no authority at epoch %d: it was not won by this elector", h.epoch)
	}
	return fmt.Errorf("no lease renewal succeeded within %v", validity)
}

// renew sends one renewal, stamped t0, and, when it succeeds, applies the
// broker-loss rule, delivering the answer on out. An attempt that has not
// answered by the next renewal is abandoned, so a hung connection does not
// keep the next one from being sent.
func (e *Elector) renew(ctx context.Context, epoch int64, t0 time.Time, out chan<- renewal) {
	ctx, cancel := context.WithTimeout(ctx, e.renewInterval())
	defer cancel()
	r := renewal{t0: t0}
	r.ok, r.err = e.st.RenewLeadership(ctx, e.cfg.Self.InstanceID, epoch, e.cfg.LeaseTTL)
	if r.err == nil && r.ok {
		r.brokerLost = e.brokerLost(ctx, t0)
	}
	out <- r
}

// brokerLost applies the broker-loss rule at now: the leader has seen its
// broker down continuously for longer than the lease, and some other member
// has reached its broker within the lease, judged on the database clock.
// Stepping down when no one can reach the broker would only turn a broker
// outage into a restart loop on every server.
func (e *Elector) brokerLost(ctx context.Context, now time.Time) bool {
	if up, downFor := e.observeBroker(now); up || downFor <= e.cfg.LeaseTTL {
		return false
	}
	members, nowUS, err := e.st.ListServerMembers(ctx)
	if err != nil {
		e.log.WarnContext(ctx, "ha: list server members failed", slog.Any("error", err))
		return false
	}
	return standbyHasBroker(members, e.cfg.Self.InstanceID, nowUS, e.cfg.LeaseTTL)
}

// Lose reports a loss of authority found outside the elector, such as a
// fenced store write or a lost broker guard. Hold returns it promptly, and
// any later Hold returns it at once. Only the first loss is kept.
func (e *Elector) Lose(r Reason, err error) {
	e.markLost(context.Background(), r, err)
}

// markLost records a loss unless one is already recorded. Recording it ends
// leadership and logs the one ERROR; [Elector.loss] returns it. The epoch won
// is kept for [Elector.Release].
func (e *Elector) markLost(ctx context.Context, r Reason, err error) {
	e.mu.Lock()
	if e.lost != nil {
		e.mu.Unlock()
		return
	}
	e.lost = &LostError{Reason: r, Err: err}
	epoch := e.epoch
	e.epoch = 0
	close(e.lostCh)
	e.mu.Unlock()

	attrs := []any{slog.String("reason", string(r)), slog.Int64("epoch", epoch)}
	if err != nil {
		attrs = append(attrs, slog.Any("error", err))
	}
	e.log.ErrorContext(ctx, "ha: lost leadership", attrs...)
}

// loss returns the recorded loss, or nil.
func (e *Elector) loss() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lost == nil {
		return nil // never a typed nil *LostError
	}
	return e.lost
}

// Release gives up the last epoch this elector won, for a graceful stop or
// after a loss of authority (release, then exit): releasing after a loss
// lets a standby take over at its next poll rather than when the lease
// lapses, and neutralizes a late-committing renewal Hold abandoned. It is
// safe whoever holds the lease now, because the store releases only for this
// holder at that epoch. Stop Hold first, or its next renewal is refused.
// Release does nothing when nothing was won or it was already released, and
// either way this server no longer leads afterwards.
func (e *Elector) Release(ctx context.Context) error {
	e.mu.Lock()
	epoch := e.won
	e.epoch, e.won = 0, 0
	e.mu.Unlock()
	if epoch == 0 {
		return nil
	}
	if err := e.st.ReleaseLeadership(ctx, e.cfg.Self.InstanceID, epoch); err != nil {
		return fmt.Errorf("ha: release leadership at epoch %d: %w", epoch, err)
	}
	e.log.InfoContext(ctx, "ha: released leadership", slog.Int64("epoch", epoch))
	return nil
}
