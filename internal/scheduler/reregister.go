// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// reregister.go asks a worker the store holds offline, but whose heartbeats
// still arrive, to register again. The heartbeat sweep marks a worker offline
// when its heartbeats stop reaching the store in time, which a store outage or
// a consumer backlog can cause while the worker itself is fine. Its heartbeats
// then resume, but a heartbeat never changes a worker's status: registration
// is the only way back to online, and a worker registers again only when its
// broker connection drops. So the heartbeat handler, on recording a heartbeat
// for such a worker, sends it [protocol.ControlReregister] on its control
// subject, and the worker runs its normal registration.

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/worker/protocol"
)

const (
	// reregisterMaxInFlight bounds the reregister requests waiting for a
	// reply at once. An ask past it is skipped, and the worker's next
	// heartbeat asks again.
	reregisterMaxInFlight = 32
	// reregisterRequestTimeout bounds one reregister request.
	reregisterRequestTimeout = 5 * time.Second
	// reregisterForgetAfter is how many WorkerTimeouts an ask is remembered
	// for before the sweep drops it. Any multiple above one keeps the rate
	// limit; the entry only has to outlive it.
	reregisterForgetAfter = 4
)

// reregisterAsks tracks the reregister requests: when each worker was last
// asked, so one is asked at most once per WorkerTimeout, and how many requests
// are waiting for a reply. The zero value is ready to use.
type reregisterAsks struct {
	// calls runs the requests off the worker consumer's goroutine. Run closes
	// it at shutdown.
	calls backgroundCalls

	mu       sync.Mutex
	asked    map[string]time.Time
	inFlight int
}

// claim reports whether workerID may be asked now: it was not asked within
// every, and fewer than reregisterMaxInFlight requests are waiting. A claim
// records the ask and takes a slot, which done gives back.
func (r *reregisterAsks) claim(workerID string, now time.Time, every time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if last, ok := r.asked[workerID]; ok && now.Sub(last) < every {
		return false
	}
	if r.inFlight >= reregisterMaxInFlight {
		return false
	}
	if r.asked == nil {
		r.asked = make(map[string]time.Time)
	}
	r.asked[workerID] = now
	r.inFlight++
	return true
}

// done gives back the slot a claim took.
func (r *reregisterAsks) done() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inFlight--
}

// forget drops workerID's last ask: it is online again, or is offline on
// purpose.
func (r *reregisterAsks) forget(workerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.asked, workerID)
}

// prune drops every ask made before cutoff, so the map holds only workers
// asked recently, whatever became of the others.
func (r *reregisterAsks) prune(cutoff time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, at := range r.asked {
		if at.Before(cutoff) {
			delete(r.asked, id)
		}
	}
}

// maybeAskReregister asks workerID to register again when out, the outcome
// of recording its heartbeat, shows a heartbeat recorded for a worker the
// store holds offline that is not retiring or asleep (a capacity-group member
// that is, is offline on purpose). It asks at most once per WorkerTimeout,
// and the request runs on a goroutine of its own: the worker consumer handles
// one message at a time, and a worker that does not answer must not hold it
// up.
func (s *Scheduler) maybeAskReregister(ctx context.Context, workerID string, out store.HeartbeatOutcome) {
	if !out.Recorded {
		return
	}
	if out.Status != store.WorkerStatusOffline ||
		out.PowerState == store.PowerRetiring || out.PowerState == store.PowerAsleep {
		s.reregister.forget(workerID)
		return
	}
	if !s.reregister.claim(workerID, time.Now(), s.cfg.WorkerTimeout) {
		return
	}
	if !s.reregister.calls.Go(func() {
		defer s.reregister.done()
		s.askReregister(ctx, workerID)
	}) {
		s.reregister.done()
	}
}

// askReregister sends workerID the reregister request and counts its outcome.
func (s *Scheduler) askReregister(ctx context.Context, workerID string) {
	s.logger.DebugContext(ctx, "scheduler: asked an offline worker to register again",
		slog.String("worker_id", workerID))
	reply, err := requestControl(ctx, s.bus.RequestWorkerControl, workerID, protocol.ControlMsg{
		Version: protocol.ProtocolVersion, Type: protocol.TypeControl, Command: protocol.ControlReregister,
	}, reregisterRequestTimeout)
	switch {
	case err != nil:
		s.countReregister("error")
		s.logger.DebugContext(ctx, "scheduler: an offline worker did not answer the request to register again",
			slog.String("worker_id", workerID), slog.Any("error", err))
	case !reply.Accepted:
		s.countReregister("refused")
		s.logger.DebugContext(ctx, "scheduler: an offline worker refused to register again",
			slog.String("worker_id", workerID), slog.String("reason", reply.Reason))
	default:
		s.countReregister("accepted")
	}
}

// countReregister counts one reregister request by outcome.
func (s *Scheduler) countReregister(outcome string) {
	if s.metrics != nil {
		s.metrics.SchedulerWorkerReregisterRequests.WithLabelValues(outcome).Inc()
	}
}

// pruneReregisterAsks is the sweep's place for dropping old reregister asks.
func (s *Scheduler) pruneReregisterAsks(context.Context) {
	s.reregister.prune(time.Now().Add(-reregisterForgetAfter * s.cfg.WorkerTimeout))
}

// closeReregisterCalls refuses later reregister requests and waits, briefly,
// for those still running. Run calls it after its context is canceled, which
// ends a request still waiting for a reply.
func (s *Scheduler) closeReregisterCalls(ctx context.Context) {
	if !s.reregister.calls.Close(reregisterRequestTimeout) {
		s.logger.WarnContext(ctx, "scheduler: a reregister request was still running at shutdown",
			slog.Duration("waited", reregisterRequestTimeout))
	}
}
