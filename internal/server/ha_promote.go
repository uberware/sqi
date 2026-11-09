// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/uberware/sqi/internal/ha"
	"github.com/uberware/sqi/internal/store"
)

// promote takes up leadership at epoch, the steps in order, every one under
// the leadership context so a write after another server took over is
// fenced:
//
//  1. take the broker's guard and provision the streams;
//  2. fence the attempts a changed broker stranded (reconcileBrokerLayout);
//  3. seed the default farm and the first admin;
//  4. run the scheduler;
//  5. start the session sweeper (which also prunes members) and the mDNS
//     responder;
//  6. report this server leader, which opens the API and /leaderz, and
//     answer worker connects (a standby may have stopped, when the previous
//     leader granted workers differently).
//
// The one write those steps make unfenced is the member prune
// (DeleteServerMembersUnseenFor, a leadership-store method, which the fence
// never applies to): deleting rows unseen for a day is idempotent, and a
// deposed leader that ran it would delete only what its successor would.
//
// A failure at any step is a loss of authority: the caller records it and
// the server exits.
func (s *Server) promote(ctx context.Context, epoch int64) error {
	lctx := store.WithLeaderEpoch(ctx, epoch)
	s.logger.InfoContext(ctx, "ha: this sqi-server is now the leader",
		slog.Int64("epoch", epoch), slog.String("previous_leader", s.ha.role.leaderName()))

	if err := s.ha.broker.TakeLeadership(lctx, s.guardOptions()); err != nil {
		return fmt.Errorf("take the broker: %w", err)
	}
	if err := s.reconcileBrokerLayout(lctx); err != nil {
		return fmt.Errorf("reconcile broker layout: %w", err)
	}
	if err := seedDefaults(lctx, s.store, s.cfg, s.logger); err != nil {
		return fmt.Errorf("seed defaults: %w", err)
	}
	if s.cfg.AuthEnabled {
		if err := s.bootstrapAuth(lctx); err != nil {
			return err
		}
	}
	// The steps from here on make no store call that a loss would fail, so
	// they check for one themselves.
	if err := s.stillLeading(ctx, epoch); err != nil {
		return err
	}
	s.runScheduler(lctx)
	s.startLeaderSweeper(lctx)
	if err := s.stillLeading(ctx, epoch); err != nil {
		return err
	}
	if err := s.startDiscovery(ctx); err != nil {
		return err
	}
	if err := s.stillLeading(ctx, epoch); err != nil {
		// The responder registers whatever its context says; take it down.
		s.discovery.Shutdown()
		return err
	}

	// The callout resumes before the role flips, so whoever observes the
	// leader role sees the callout already answering.
	s.ha.broker.SetCalloutActive(true)
	s.ha.role.promote(s.ha.self.Name, epoch)
	s.metrics.HALeader.Set(1)
	s.metrics.HAEpoch.Set(float64(epoch))
	s.metrics.HAPromotions.Inc()
	s.logger.InfoContext(ctx, "sqi-server ready as the leader", slog.Int64("epoch", epoch))
	return nil
}

// errLostDuringPromotion is what promote returns when authority was lost
// while it ran; the loss itself is already recorded.
var errLostDuringPromotion = errors.New("leadership lost during promotion")

// stillLeading returns ctx's error, or [errLostDuringPromotion] once the
// elector no longer leads at epoch. The elector drops its epoch as it records
// a loss, so this sees one at once, before Hold has returned and ended ctx.
func (s *Server) stillLeading(ctx context.Context, epoch int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.ha.elector.Epoch() != epoch {
		return errLostDuringPromotion
	}
	return nil
}

// promotionReason is the reason a failed promotion is recorded under: a
// fenced write means another server already took over.
func promotionReason(err error) ha.Reason {
	if errors.Is(err, store.ErrFenced) {
		return ha.ReasonFenced
	}
	return ha.ReasonPromotion
}

// runScheduler runs the scheduler under the leadership context. A scheduler
// that fails to start leaves a leader that cannot schedule, which is a loss
// of authority like any other failed promotion step.
func (s *Server) runScheduler(lctx context.Context) {
	schedCtx, cancelSched := context.WithCancel(lctx)
	s.cancelSched = cancelSched
	done := make(chan struct{})
	s.schedDone = done
	go func() {
		defer close(done)
		if err := s.sched.Run(schedCtx); err != nil && !errors.Is(err, context.Canceled) {
			s.ha.elector.Lose(promotionReason(err), fmt.Errorf("scheduler: %w", err))
		}
	}()
	s.logger.InfoContext(lctx, "scheduler: started")
}

// memberRetention is how long a member row outlives its server's last
// upsert before the leader deletes it.
const memberRetention = 24 * time.Hour

// startLeaderSweeper runs the leader's housekeeping once, before it returns,
// and then on the session sweeper's interval: expired sessions (with auth on) and member
// rows unseen for [memberRetention]. A fenced session sweep means another
// server leads.
func (s *Server) startLeaderSweeper(lctx context.Context) {
	sweep := func() {
		if s.cfg.AuthEnabled {
			if err := s.sweepSessions(lctx); err != nil {
				switch {
				case errors.Is(err, store.ErrFenced):
					s.ha.elector.Lose(ha.ReasonFenced, err)
					return
				case lctx.Err() == nil: // a sweep cut short by a loss or a stop is not a failure
					s.logger.ErrorContext(lctx, "auth: expired-session sweep failed", slog.Any("error", err))
				}
			}
		}
		s.pruneMembers(lctx)
	}
	// The first sweep runs in line, so promotion's next loss check sees a
	// fenced one.
	sweep()
	s.ha.loops.Go(func() { sweepEvery(lctx, false, sweep) })
	s.logger.InfoContext(lctx, "ha: leader sweeper started",
		slog.Duration("interval", sessionSweepInterval), slog.Bool("sessions", s.cfg.AuthEnabled))
}

// pruneMembers deletes member rows unseen for [memberRetention].
func (s *Server) pruneMembers(ctx context.Context) {
	n, err := s.store.DeleteServerMembersUnseenFor(ctx, memberRetention)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.WarnContext(ctx, "ha: prune server members failed", slog.Any("error", err))
		}
		return
	}
	if n > 0 {
		s.logger.InfoContext(ctx, "ha: pruned server members unseen for a day", slog.Int("count", n))
	}
}

// demoteTimeout bounds each step of stopping a demoted leader: it is exiting,
// and a standby is waiting.
const demoteTimeout = 2 * time.Second

// demote stops a leader that lost its authority and returns the error that
// makes the process exit with [ExitDemoted]. The loss was logged at ERROR
// where it was recorded; this logs the exit only. Each step is bounded,
// because a deposed scheduler keeps pulling from the shared consumers until
// the process is gone.
func (s *Server) demote(lost error) error {
	reason := ha.Reason("unknown")
	if le, ok := errors.AsType[*ha.LostError](lost); ok {
		reason = le.Reason
	}
	bg := context.Background()
	s.markNotLeading()
	s.metrics.HADemotions.WithLabelValues(string(reason)).Inc()
	s.logger.InfoContext(bg, "ha: this sqi-server is no longer the leader; exiting so its supervisor restarts it as a standby",
		slog.String("reason", string(reason)), slog.Int("exit_code", ExitDemoted))

	if s.discovery != nil {
		s.discovery.Shutdown()
	}
	if s.httpServer != nil {
		if err := s.httpServer.Close(); err != nil {
			s.logger.WarnContext(bg, "http: close after a demotion", slog.Any("error", err))
		}
	}
	bounded(bg, demoteTimeout, func(ctx context.Context) {
		if !s.stopScheduler(ctx) {
			s.logger.WarnContext(ctx, "scheduler: still stopping; carrying on without it")
		}
	})
	if s.busClient != nil {
		bounded(bg, demoteTimeout, func(ctx context.Context) {
			if err := s.busClient.Drain(ctx); err != nil {
				s.busClient.Close()
			}
		})
	}
	bounded(bg, demoteTimeout, func(ctx context.Context) {
		if err := s.ha.elector.Release(ctx); err != nil {
			s.logger.WarnContext(bg, "ha: lease release after a demotion failed; it lapses on its own", slog.Any("error", err))
		}
	})
	if s.broker != nil {
		s.broker.Shutdown()
	}
	s.ha.stopLoops()
	if err := s.store.Close(); err != nil {
		s.logger.WarnContext(bg, "store: close after a demotion", slog.Any("error", err))
	}
	return fmt.Errorf("%w: %w", ErrDemoted, lost)
}

// handOver gives up leadership on a graceful stop: the API and /leaderz
// close, the scheduler stops (answering parked lease requests empty), and the
// lease is released so a standby takes over at its next poll rather than
// when the lease lapses. On a standby only the role is touched; without HA
// nothing is.
func (s *Server) handOver(ctx context.Context) error {
	if s.ha == nil {
		return nil
	}
	s.markNotLeading()
	// Each step has its own bound: neither may spend the whole shutdown
	// budget, nor depend on what is left of it, since a release that fails
	// leaves the standby waiting a full lease. A scheduler still draining
	// capacity provider calls when the bound ends is waited for again, for
	// as long as its drain takes, before the store closes ([Server.shutdown]);
	// the release does not wait for it, and its later writes are fenced.
	bounded(context.WithoutCancel(ctx), demoteTimeout, func(ctx context.Context) { s.stopScheduler(ctx) })
	if s.ha.elector == nil {
		return nil
	}
	var err error
	bounded(context.Background(), releaseTimeout, func(rctx context.Context) { err = s.ha.elector.Release(rctx) })
	return err
}

// markNotLeading reports this server no longer the leader: the role, which
// closes the API and /leaderz, and the leader gauge.
func (s *Server) markNotLeading() {
	s.ha.role.demote()
	s.metrics.HALeader.Set(0)
}

// bounded runs f with a context derived from parent that ends after d.
func bounded(parent context.Context, d time.Duration, f func(ctx context.Context)) {
	ctx, cancel := context.WithTimeout(parent, d)
	defer cancel()
	f(ctx)
}

// releaseTimeout bounds releasing the lease on a graceful stop.
const releaseTimeout = 5 * time.Second

// stopScheduler stops a running scheduler and waits for it until ctx ends. It
// reports whether the scheduler has returned (or never ran).
func (s *Server) stopScheduler(ctx context.Context) bool {
	if s.cancelSched != nil {
		s.cancelSched()
	}
	if s.schedDone == nil {
		return true
	}
	select {
	case <-s.schedDone:
		return true
	case <-ctx.Done():
		return false
	}
}
