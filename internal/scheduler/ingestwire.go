// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/worker/protocol"
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

// ingestLoopState is what the marker loop keeps between ticks. Only the
// marker loop goroutine touches it.
type ingestLoopState struct {
	// prevTick is the time of the previous tick, zero before the first.
	prevTick time.Time
	// publishFailing is set while marker publishes fail, so an outage is
	// logged once.
	publishFailing bool
	// lastInterrupt is the newest bus interruption already held for.
	lastInterrupt time.Time
}

// runIngestMarkers publishes an ingest marker into SQI_WORKER and SQI_TASK
// every [ingestMarkerInterval] until ctx is canceled, the first at once. The
// consumers advance their trackers when the markers come back to them (see
// [Scheduler.handleIngestMarker]); publishing advances nothing.
func (s *Scheduler) runIngestMarkers(ctx context.Context) {
	ticker := time.NewTicker(ingestMarkerInterval)
	defer ticker.Stop()

	s.ingestMarkerTick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.ingestMarkerTick(ctx)
		}
	}
}

// ingestMarkerTick is one tick of [Scheduler.runIngestMarkers]: it holds the
// trackers after a process pause or a bus interruption, then publishes the
// markers.
func (s *Scheduler) ingestMarkerTick(ctx context.Context) {
	now := s.now()
	prev := s.ingestLoop.prevTick
	// The gap runs from the end of the previous tick's work, so a slow
	// publish is not mistaken for a pause.
	defer func() { s.ingestLoop.prevTick = s.now() }()
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

	// Bounded, so a stalled broker cannot hold up the next tick's checks.
	pubCtx, cancel := context.WithTimeout(ctx, ingestMarkerInterval)
	defer cancel()
	if err := s.bus.PublishIngestMarkers(pubCtx); err != nil {
		if !s.ingestLoop.publishFailing && ctx.Err() == nil {
			s.logger.DebugContext(ctx, "scheduler: publish ingest markers failed", slog.Any("error", err))
		}
		s.ingestLoop.publishFailing = true
		return
	}
	s.ingestLoop.publishFailing = false
}

// checkBusInterruption holds each tracker for its consumer's AckWait from the
// latest bus interruption, once per interruption: a message the broker sent
// while a consumer's pull was lost is redelivered only after that long.
func (s *Scheduler) checkBusInterruption(now time.Time) {
	if s.cfg.BusInterruptedAt == nil {
		return
	}
	at := s.cfg.BusInterruptedAt()
	if at.IsZero() || !at.After(s.ingestLoop.lastInterrupt) {
		return
	}
	s.ingestLoop.lastInterrupt = at
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

// noteWorkerDelivery holds the worker tracker when msg shows that SQI_WORKER
// removed messages before the worker consumer read them. Its MaxAge is two
// minutes, so heartbeats expire unread whenever the consumer falls that far
// behind: during a store stall that blocks the handler, or under a load it
// cannot keep up with. A marker read after them would otherwise move the
// tracker past heartbeats that never reached the store, and the sweep would
// take live workers offline.
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
func (s *Scheduler) noteWorkerDelivery(ctx context.Context, msg jetstream.Msg) {
	md, err := msg.Metadata()
	if err != nil || md.NumDelivered != 1 {
		return
	}
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

// trackerFor returns the tracker of the consumer that receives subject, or
// nil for a subject neither tracked consumer receives.
func (s *Scheduler) trackerFor(subject string) *ingestTracker {
	switch {
	case subject == bus.SubjectMarkerWorker || strings.HasPrefix(subject, "worker."):
		return s.ingestWorker
	case subject == bus.SubjectMarkerTask || strings.HasPrefix(subject, bus.SubjectTaskStatusPrefix+"."):
		return s.ingestTask
	default:
		return nil
	}
}

// handleIngestMarker records that the consumer receiving msg, a marker, has
// handled everything stored before it, and acks it.
func (s *Scheduler) handleIngestMarker(ctx context.Context, msg jetstream.Msg) {
	if t := s.trackerFor(msg.Subject()); t != nil {
		if md, err := msg.Metadata(); err == nil {
			t.marker(md.Timestamp)
		}
	}
	s.ackMsg(ctx, msg)
}

// recordIngestAck removes an acked message from its consumer's tracker.
func (s *Scheduler) recordIngestAck(msg jetstream.Msg) {
	t := s.trackerFor(msg.Subject())
	if t == nil {
		return
	}
	if md, err := msg.Metadata(); err == nil {
		t.ack(md.Sequence.Stream)
	}
}

// recordIngestNak records a nak'd message, which will be delivered again, in
// its consumer's tracker.
func (s *Scheduler) recordIngestNak(msg jetstream.Msg, md *jetstream.MsgMetadata) {
	t := s.trackerFor(msg.Subject())
	if t == nil || md == nil {
		return
	}
	t.nak(md.Sequence.Stream, md.Timestamp, md.NumDelivered, ingestKeys(msg))
}

// ingestKeys returns the keys of what msg would change once applied: its
// worker for a worker message, and for a task status also its task and
// attempt (each left out when the body does not name it).
func ingestKeys(msg jetstream.Msg) []string {
	subject := msg.Subject()
	workerID, _, ok := bus.ParseWorkerSubject(subject)
	if !ok {
		return nil
	}
	switch {
	case strings.HasPrefix(subject, "worker."):
		return []string{workerKey(workerID)}
	case strings.HasPrefix(subject, bus.SubjectTaskStatusPrefix+"."):
		keys := []string{workerKey(workerID)}
		var m protocol.TaskStatusMsg
		if err := json.Unmarshal(msg.Data(), &m); err != nil {
			return keys
		}
		if m.TaskID != "" {
			keys = append(keys, taskKey(m.TaskID))
		}
		if m.AttemptID != "" {
			keys = append(keys, attemptKey(m.AttemptID))
		}
		return keys
	default:
		return nil
	}
}

// refreshIngestGauges sets how far each tracked consumer's newest processed
// marker trails now and how many of its messages wait to be applied again.
func (s *Scheduler) refreshIngestGauges(context.Context) {
	now := time.Now()
	for consumer, t := range map[string]*ingestTracker{"worker": s.ingestWorker, "task": s.ingestTask} {
		s.metrics.SchedulerIngestLagSeconds.WithLabelValues(consumer).Set(t.lag(now).Seconds())
		s.metrics.SchedulerIngestPending.WithLabelValues(consumer).Set(float64(t.pendingCount(now)))
	}
}
