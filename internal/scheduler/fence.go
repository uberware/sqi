// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// What the scheduler does when a store write comes back fenced.
//
// Under scheduler high availability the server runs the scheduler with a
// leadership context ([store.WithLeaderEpoch]), and every store write made
// under it commits only while that epoch is still current. A write that
// fails with [store.ErrFenced] means another server has taken over: whatever
// this one does next would race the new leader. So the work in hand stops
// (a sweep tick, a start-up repair, a lease batch), the message being handled
// is neither acked nor nacked, which leaves JetStream to redeliver it to the
// new leader once its ack wait runs out, and the loss is reported once, through
// [Config.OnFenced]. From then on this scheduler acknowledges no message and
// leases no task.
//
// Without HA no context carries an epoch, nothing is ever fenced, and none of
// this changes anything.

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"

	"github.com/uberware/sqi/internal/store"
)

// fenceState is the scheduler's record of a fenced write.
type fenceState struct {
	// deposed is set by the first fenced write and never cleared: a fenced
	// server stays fenced until it exits. The write that sets it is the one
	// that logs the loss and calls OnFenced.
	deposed atomic.Bool
}

// stopIfFenced reports whether err is (or wraps) [store.ErrFenced], recording
// the fence when it is. The caller stops what it is doing and, in a message
// handler, returns without acknowledging the message.
func (s *Scheduler) stopIfFenced(ctx context.Context, err error) bool {
	if !errors.Is(err, store.ErrFenced) {
		return false
	}
	s.fenced(ctx, err)
	return true
}

// warnUnlessStopped logs err at WARN as msg, with attrs and then the error,
// unless err is nil, the context's cancellation, or a fenced write, which
// [Scheduler.stopIfFenced] records instead.
func (s *Scheduler) warnUnlessStopped(ctx context.Context, err error, msg string, attrs ...slog.Attr) {
	if err == nil || errors.Is(err, context.Canceled) || s.stopIfFenced(ctx, err) {
		return
	}
	args := make([]any, 0, len(attrs)+1)
	for _, a := range attrs {
		args = append(args, a)
	}
	s.logger.WarnContext(ctx, msg, append(args, slog.Any("error", err))...)
}

// fenced records that a store write was fenced. The first call logs it, at
// ERROR without [Config.OnFenced] and at WARN with it, and invokes the
// callback on a goroutine of its own, so the handler or
// sweep that hit the fence is not held up by it, and a callback that stops the
// scheduler and waits for [Scheduler.Run] to return cannot deadlock against
// Run waiting for that same goroutine. Later calls log at Debug.
func (s *Scheduler) fenced(ctx context.Context, err error) {
	if !s.fence.deposed.CompareAndSwap(false, true) {
		s.logger.DebugContext(ctx, "scheduler: store write fenced", slog.Any("error", err))
		return
	}
	const msg = "scheduler: this server is no longer the leader; stopping"
	if s.cfg.OnFenced == nil {
		s.logger.ErrorContext(ctx, msg, slog.Any("error", err))
		return
	}
	// The callback's owner reports the loss at ERROR, once; a second
	// ERROR for the same loss would read as two failures.
	s.logger.WarnContext(ctx, msg, slog.Any("error", err))
	go s.cfg.OnFenced(err)
}

// deposed reports whether a store write of this scheduler has been fenced.
func (s *Scheduler) deposed() bool {
	return s.fence.deposed.Load()
}
