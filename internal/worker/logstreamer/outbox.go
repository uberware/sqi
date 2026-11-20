// SPDX-License-Identifier: AGPL-3.0-or-later

package logstreamer

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// Delivery defaults. Each is a [Config] field tests can shorten; none is
// operator configuration.
const (
	// defaultAckTimeout bounds one try, the wait for the stream's ack
	// included. It matches the status publisher's.
	defaultAckTimeout = 2 * time.Second
	// defaultRetryBase is the wait before the first retry of a chunk; each
	// later wait doubles, up to defaultRetryMax.
	defaultRetryBase = 250 * time.Millisecond
	defaultRetryMax  = 5 * time.Second
	// defaultFlushWait bounds how long FlushLogs waits for its attempt's
	// chunks to be stored before the terminal status goes out.
	defaultFlushWait = 5 * time.Second
	// defaultMaxPendingBytes bounds the encoded chunks held for delivery.
	defaultMaxPendingBytes = 32 << 20
	// warnEvery rate-limits the retry and drop warnings.
	warnEvery = time.Minute
)

// msgDropped is logged when the bound drops chunks the broker has not stored.
const msgDropped = "logstreamer: dropped log chunks the broker has not stored"

// pendingChunk is one encoded chunk waiting for the stream's ack.
type pendingChunk struct {
	subj      string
	msgID     string
	attemptID string
	data      []byte
}

// outbox holds encoded chunks in the order they were published and delivers
// them one at a time on a sender goroutine, which runs only while the outbox
// holds something. A chunk stays at the head until the stream stores it or it
// fails in a way no retry can cure.
type outbox struct {
	t      Transport
	logger *slog.Logger
	cfg    Config

	mu         sync.Mutex
	queue      []*pendingChunk
	bytes      int
	inflight   *pendingChunk
	perAttempt map[string]int
	// changed is closed and replaced whenever a chunk leaves the queue.
	changed chan struct{}
	// idle is closed while no sender runs.
	idle    chan struct{}
	running bool
	// closed is set by Flush and Abandon: nothing more is sent.
	closed  bool
	stopCtx context.Context
	stop    context.CancelFunc

	dropped      int // dropped by the bound since the last warning
	lastDropWarn time.Time
	failures     int // consecutive failed tries of the head
	lastWarn     time.Time
}

func newOutbox(t Transport, cfg Config, logger *slog.Logger) *outbox {
	idle := make(chan struct{})
	close(idle)
	ctx, stop := context.WithCancel(context.Background())
	return &outbox{
		t: t, logger: logger, cfg: cfg,
		perAttempt: map[string]int{},
		changed:    make(chan struct{}),
		idle:       idle,
		stopCtx:    ctx,
		stop:       stop,
	}
}

// enqueue adds c behind every chunk already held, dropping the oldest held
// chunks the bound no longer fits, and starts the sender if none runs. It
// never waits on the broker.
func (o *outbox) enqueue(c *pendingChunk) {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		o.noteDropped(1)
		return
	}
	o.queue = append(o.queue, c)
	o.bytes += len(c.data)
	o.perAttempt[c.attemptID]++
	evicted := o.evictLocked()
	if !o.running {
		o.running = true
		o.idle = make(chan struct{})
		go o.run()
	}
	o.mu.Unlock()
	o.noteDropped(evicted)
}

// evictLocked drops the oldest chunks until the held bytes fit the bound. It
// never drops the newest chunk, nor the one being tried, whose outcome is not
// known yet.
func (o *outbox) evictLocked() (evicted int) {
	for o.bytes > o.cfg.maxPendingBytes && len(o.queue) > 1 {
		i := 0
		if o.queue[0] == o.inflight {
			i = 1
		}
		if i >= len(o.queue)-1 {
			break
		}
		o.removeLocked(i)
		evicted++
	}
	return evicted
}

// removeLocked takes the chunk at index i out of the queue.
func (o *outbox) removeLocked(i int) {
	c := o.queue[i]
	o.queue = slices.Delete(o.queue, i, i+1)
	o.bytes -= len(c.data)
	o.perAttempt[c.attemptID]--
	if o.perAttempt[c.attemptID] <= 0 {
		delete(o.perAttempt, c.attemptID)
	}
	close(o.changed)
	o.changed = make(chan struct{})
}

// noteDropped counts n chunks the bound dropped and warns at most once a
// minute.
func (o *outbox) noteDropped(n int) {
	if n == 0 {
		return
	}
	o.mu.Lock()
	o.dropped += n
	now := time.Now()
	if now.Sub(o.lastDropWarn) < warnEvery {
		o.mu.Unlock()
		return
	}
	dropped := o.dropped
	o.dropped, o.lastDropWarn = 0, now
	o.mu.Unlock()
	o.logger.WarnContext(context.Background(), msgDropped,
		slog.Int("dropped", dropped), slog.Int("max_pending_bytes", o.cfg.maxPendingBytes))
}

// run is the sender: it tries the head chunk until the stream stores it, then
// the next, and exits once the queue is empty or the outbox is closed.
func (o *outbox) run() {
	for {
		o.mu.Lock()
		if o.closed || len(o.queue) == 0 {
			o.running = false
			close(o.idle)
			o.mu.Unlock()
			return
		}
		c := o.queue[0]
		o.inflight = c
		o.mu.Unlock()

		ctx, cancel := context.WithTimeout(o.stopCtx, o.cfg.ackTimeout)
		err := o.t.PublishAcked(ctx, c.subj, c.data, c.msgID)
		cancel()

		o.mu.Lock()
		o.inflight = nil
		done := err == nil || permanent(err)
		if done && len(o.queue) > 0 && o.queue[0] == c {
			o.removeLocked(0)
		}
		o.mu.Unlock()

		switch {
		case err == nil:
			o.stored(c)
		case o.stopCtx.Err() != nil:
			// Closed; the next pass exits.
		case permanent(err):
			o.logger.ErrorContext(context.Background(), "logstreamer: log chunk cannot be sent; dropped",
				slog.String("subject", c.subj), slog.String("msg_id", c.msgID), slog.Any("error", err))
		default:
			o.retryWait(c, err)
		}
	}
}

// stored logs that a chunk got through after failed tries.
func (o *outbox) stored(c *pendingChunk) {
	if o.failures > 0 {
		o.logger.InfoContext(context.Background(), "logstreamer: log chunk stored after retrying",
			slog.String("msg_id", c.msgID), slog.Int("failures", o.failures))
	}
	o.failures = 0
}

// retryWait warns about a failed try, at most once a minute, and waits out the
// backoff before the next one.
func (o *outbox) retryWait(c *pendingChunk, err error) {
	o.failures++
	if now := time.Now(); o.failures == 1 || now.Sub(o.lastWarn) >= warnEvery {
		o.lastWarn = now
		o.logger.WarnContext(context.Background(), "logstreamer: log chunk not acknowledged; retrying",
			slog.String("msg_id", c.msgID), slog.Int("failures", o.failures), slog.Any("error", err))
	}
	delay := o.cfg.retryBase
	for range min(o.failures-1, 16) {
		delay = min(delay*2, o.cfg.retryMax)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-o.stopCtx.Done():
	case <-timer.C:
	}
}

// waitAttempt waits until none of attemptID's chunks is held, the outbox is
// closed, or d passes.
func (o *outbox) waitAttempt(attemptID string, d time.Duration) {
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	for {
		o.mu.Lock()
		if o.perAttempt[attemptID] == 0 || o.closed {
			o.mu.Unlock()
			return
		}
		ch := o.changed
		o.mu.Unlock()
		select {
		case <-ch:
		case <-deadline.C:
			return
		}
	}
}

// flush waits until the outbox is empty or ctx ends, then closes it and
// returns how many chunks it dropped.
func (o *outbox) flush(ctx context.Context) (dropped int) {
	for {
		o.mu.Lock()
		if len(o.queue) == 0 {
			o.mu.Unlock()
			return o.close()
		}
		ch := o.changed
		o.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return o.close()
		}
	}
}

// close stops delivery, drops every held chunk and waits for the sender to
// exit. It returns how many chunks it dropped.
func (o *outbox) close() (dropped int) {
	o.mu.Lock()
	o.closed = true
	dropped = len(o.queue)
	o.queue, o.bytes = nil, 0
	clear(o.perAttempt)
	close(o.changed)
	o.changed = make(chan struct{})
	o.stop()
	idle := o.idle
	o.mu.Unlock()
	<-idle
	return dropped
}

// permanent reports whether err cannot be cured by sending the same chunk
// again. A closed connection never reopens.
func permanent(err error) bool {
	return errors.Is(err, nats.ErrMaxPayload) || errors.Is(err, nats.ErrConnectionClosed)
}
