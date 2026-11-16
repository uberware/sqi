// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
)

const (
	// ingestMarkerInterval is how often the scheduler publishes an ingest
	// marker into each consumed stream.
	ingestMarkerInterval = 2 * time.Second

	// ingestPauseThreshold is the gap between two marker ticks above which
	// the process is taken to have been paused. While it was, its consumers
	// applied nothing, so the trackers are held at the tick before the gap.
	ingestPauseThreshold = 3 * ingestMarkerInterval
)

// ingestLoopState is what the marker loop keeps between ticks, a local of
// [Scheduler.runIngestMarkers].
type ingestLoopState struct {
	// prevTick is the time of the previous tick, zero before the first.
	prevTick time.Time
	// publishFailing is set while marker publishes fail, so an outage is
	// logged once.
	publishFailing bool
}

// interruptSeen is the newest bus interruption the trackers have been held
// for. The marker loop, the heartbeat sweep and the reap all read the
// interruption time, so it is guarded.
type interruptSeen struct {
	mu   sync.Mutex
	last time.Time
}

// claim reports whether at is a bus interruption newer than any claimed
// before, and records it.
func (i *interruptSeen) claim(at time.Time) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if at.IsZero() || !at.After(i.last) {
		return false
	}
	i.last = at
	return true
}

// runIngestMarkers publishes an ingest marker into SQI_WORKER and SQI_TASK
// every [ingestMarkerInterval] until ctx is canceled, the first at once. The
// consumers advance their trackers when the markers come back to them (see
// [Scheduler.receiveTracked]); publishing advances nothing.
func (s *Scheduler) runIngestMarkers(ctx context.Context) {
	ticker := time.NewTicker(ingestMarkerInterval)
	defer ticker.Stop()

	var loop ingestLoopState
	s.ingestMarkerTick(ctx, &loop)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.ingestMarkerTick(ctx, &loop)
		}
	}
}

// ingestMarkerTick is one tick of [Scheduler.runIngestMarkers]: it holds the
// trackers after a process pause or a bus interruption, then publishes the
// markers. loop is the state the loop keeps between ticks.
func (s *Scheduler) ingestMarkerTick(ctx context.Context, loop *ingestLoopState) {
	now := s.now()
	prev := loop.prevTick
	// The gap runs from the end of the previous tick's work, so a slow
	// publish is not mistaken for a pause.
	defer func() { loop.prevTick = s.now() }()
	// A gap far longer than the ticker's period means the process stopped
	// running. Measured on the monotonic clock when both times carry it.
	if !prev.IsZero() {
		if gap := now.Sub(prev); gap > ingestPauseThreshold {
			until := now.Add(s.cfg.WorkerTimeout)
			s.ingestWorker.hold(prev, until)
			s.ingestTask.hold(prev, until)
			s.logger.WarnContext(ctx, "scheduler: process pause detected", slog.Duration("gap", gap))
		}
	}
	s.checkBusInterruption(now)

	// Each publish is bounded, so a stalled broker cannot hold up the next
	// tick's checks for long, and a stall on one stream cannot keep the other
	// stream's marker back.
	if err := s.bus.PublishIngestMarkers(ctx, ingestMarkerInterval); err != nil {
		if !loop.publishFailing && ctx.Err() == nil {
			s.logger.DebugContext(ctx, "scheduler: publish ingest markers failed", slog.Any("error", err))
		}
		loop.publishFailing = true
		return
	}
	loop.publishFailing = false
}

// checkBusInterruption holds each tracker for its consumer's AckWait from the
// latest bus interruption, once per interruption: a message the broker sent
// while a consumer's pull was lost is redelivered only after that long. The
// marker loop calls it every tick, and the heartbeat sweep and the reap call
// it before reading the trackers, so a check landing between an interruption
// and the next tick is held too. Safe for concurrent use.
func (s *Scheduler) checkBusInterruption(now time.Time) {
	if s.cfg.BusInterruptedAt == nil {
		return
	}
	at := s.cfg.BusInterruptedAt()
	if !s.interrupt.claim(at) {
		return
	}
	// A hold must start no later than now; an interruption recorded after
	// now was read starts at now.
	if at.After(now) {
		at = now
	}
	s.ingestWorker.hold(at, at.Add(bus.WorkerConsumerAckWait))
	s.ingestTask.hold(at, at.Add(bus.TaskStatusConsumerAckWait))
}

// firstDeliverySeq is the state behind [Scheduler.noteWorkerDelivery].
type firstDeliverySeq struct {
	mu sync.Mutex
	// last is the highest stream sequence delivered for the first time, zero
	// before the first.
	last uint64
}

// next records a first delivery of stream sequence seq and returns how many
// messages before it, after the previous first delivery, the consumer never
// got: 0 when none were skipped or seq is the first one seen.
func (f *firstDeliverySeq) next(seq uint64) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var skipped uint64
	if f.last != 0 && seq > f.last+1 {
		skipped = seq - f.last - 1
	}
	f.last = max(f.last, seq)
	return skipped
}

// ingestMsg is a message a tracked consumer received, bound to that
// consumer's tracker, with its metadata read once on arrival. [Scheduler.ackMsg]
// and [Scheduler.nakMsg] record it in the tracker.
type ingestMsg struct {
	jetstream.Msg

	tracker *ingestTracker
	md      *jetstream.MsgMetadata
	mdErr   error
}

// newIngestMsg binds msg to t and reads its metadata.
func newIngestMsg(t *ingestTracker, msg jetstream.Msg) *ingestMsg {
	md, err := msg.Metadata()
	return &ingestMsg{Msg: msg, tracker: t, md: md, mdErr: err}
}

// Metadata returns the metadata read when the message arrived.
func (m *ingestMsg) Metadata() (*jetstream.MsgMetadata, error) { return m.md, m.mdErr }

// receiveTracked is the start of a tracked consumer's handler. It binds raw
// to t, reports a first delivery to onFirst when that is set (only the worker
// consumer checks for messages removed unread; see
// [Scheduler.noteWorkerDelivery]), then advances t and acks for the
// consumer's marker subject, and passes anything else to route.
func (s *Scheduler) receiveTracked(
	raw jetstream.Msg, t *ingestTracker, marker string,
	onFirst func(context.Context, *jetstream.MsgMetadata), route func(jetstream.Msg),
) {
	ctx := s.ctx
	msg := newIngestMsg(t, raw)
	// Before the marker branch, so a marker can reveal a skip too.
	if onFirst != nil && msg.mdErr == nil && msg.md.NumDelivered == 1 {
		onFirst(ctx, msg.md)
	}
	if raw.Subject() != marker {
		route(msg)
		return
	}
	// The consumer has handled everything stored before the marker.
	if msg.mdErr == nil {
		t.marker(msg.md.Timestamp)
	}
	s.ackMsg(ctx, msg)
}

// noteWorkerDelivery holds the worker tracker when md, the metadata of a
// first delivery, shows that SQI_WORKER removed messages before the worker
// consumer read them. Its MaxAge is two minutes, so heartbeats expire unread
// whenever the consumer falls that far behind: during a store stall that
// blocks the handler, or under a load it cannot keep up with. A marker read
// after them would otherwise move the tracker past heartbeats that never
// reached the store, and the sweep would take live workers offline.
//
// For the one consumer of a work-queue stream, first deliveries
// (NumDelivered 1) arrive in increasing stream sequence, so a first delivery
// more than one past the previous one means the messages between were removed
// unread. Redeliveries can arrive in any order and are not counted. The first
// message after a start sets the sequence to count from, so a fresh process
// or a promoted standby detects nothing: its tracker starts at zero and the
// startup grace covers it. A first delivery the client dropped before the
// handler (an interruption) also reads as a skip, which only holds the
// tracker longer.
func (s *Scheduler) noteWorkerDelivery(ctx context.Context, md *jetstream.MsgMetadata) {
	skipped := s.workerSeq.next(md.Sequence.Stream)
	if skipped == 0 {
		return
	}
	if s.ingestWorker.gap(md.Timestamp, s.cfg.WorkerTimeout) {
		s.logger.WarnContext(ctx, "scheduler: worker messages expired from the stream unread; liveness held until the consumer reads a heartbeat timeout past them",
			slog.Uint64("skipped", skipped), slog.Time("first_read_after", md.Timestamp),
			slog.Duration("heartbeat_timeout", s.cfg.WorkerTimeout))
	}
}

// recordIngestAck removes an acked message from the tracker it is bound to,
// if any.
func recordIngestAck(msg jetstream.Msg) {
	if m, ok := msg.(*ingestMsg); ok && m.mdErr == nil {
		m.tracker.ack(m.md.Sequence.Stream)
	}
}

// recordIngestNak records a nak'd message, which will be delivered again, in
// the tracker it is bound to, if any, as naming ref.
func recordIngestNak(msg jetstream.Msg, ref ingestRef) {
	if m, ok := msg.(*ingestMsg); ok && m.mdErr == nil {
		m.tracker.nak(m.md.Sequence.Stream, m.md.Timestamp, m.md.NumDelivered, ref)
	}
}

// refreshIngestGauges sets how far each tracked consumer's newest processed
// marker trails now (before its first, how long the scheduler has run) and
// how many of its messages wait to be applied again.
func (s *Scheduler) refreshIngestGauges(context.Context) {
	now := s.now()
	for consumer, t := range map[string]*ingestTracker{"worker": s.ingestWorker, "task": s.ingestTask} {
		s.metrics.SchedulerIngestLagSeconds.WithLabelValues(consumer).Set(t.lag(now, s.startedAt).Seconds())
		s.metrics.SchedulerIngestPending.WithLabelValues(consumer).Set(float64(t.pendingCount(now)))
	}
}
