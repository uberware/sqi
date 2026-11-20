// SPDX-License-Identifier: AGPL-3.0-or-later

package logstreamer

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/bus"
)

// Delivery defaults. All but warnEvery back a [Config] field tests can
// shorten; none is operator configuration.
const (
	// defaultAckTimeout bounds the wait for one chunk's ack. It matches the
	// status publisher's DefaultAckTimeout.
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
	// defaultWindow is how many chunks one send puts on the wire before
	// waiting for their acks.
	defaultWindow = 32
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
	// sending is set while the chunk is on the wire, guarded by outbox.mu.
	sending bool
}

// outbox holds encoded chunks in the order they were published and delivers
// them on a sender goroutine, which runs only while the outbox holds
// something. Each send puts up to [Config.window] chunks from the head on the
// wire in order, then waits for their acks, so a worker far from the broker
// is not held to one chunk per round trip. A chunk stays queued until the
// stream stores it or it fails in a way no retry can cure.
//
// Chunks reach the stream in the order they were published, with one
// exception: when one chunk of a send fails and a later one is stored, the
// failed one is stored when it is retried, after it.
type outbox struct {
	t      Transport
	logger *slog.Logger
	cfg    Config

	mu         sync.Mutex
	queue      []*pendingChunk
	bytes      int
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
	failures     int // consecutive sends with a chunk that failed
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
	if evicted > 0 {
		o.signalLocked()
	}
	if !o.running {
		o.running = true
		o.idle = make(chan struct{})
		go o.run()
	}
	o.mu.Unlock()
	o.noteDropped(evicted)
}

// evictLocked drops the oldest chunks until the held bytes fit the bound. It
// never drops the newest chunk, nor one on the wire, whose outcome is not
// known yet.
func (o *outbox) evictLocked() (evicted int) {
	for o.bytes > o.cfg.maxPendingBytes {
		i := slices.IndexFunc(o.queue, func(c *pendingChunk) bool { return !c.sending })
		if i < 0 || i >= len(o.queue)-1 {
			break
		}
		o.removeLocked(i)
		evicted++
	}
	return evicted
}

// removeLocked takes the chunk at index i out of the queue. Chunks leave
// from within a window of the head, so the chunks in front of it move up one
// rather than every chunk behind it moving down: a backlog of many thousands
// drains in time proportional to its length. Callers then call signalLocked.
func (o *outbox) removeLocked(i int) {
	c := o.queue[i]
	copy(o.queue[1:i+1], o.queue[:i])
	o.queue[0] = nil
	o.queue = o.queue[1:]
	o.bytes -= len(c.data)
	o.perAttempt[c.attemptID]--
	if o.perAttempt[c.attemptID] <= 0 {
		delete(o.perAttempt, c.attemptID)
	}
}

// signalLocked wakes everything waiting for a chunk to leave the queue.
func (o *outbox) signalLocked() {
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

// run is the sender: it sends the head of the queue until the stream stores
// every chunk, and exits once the queue is empty or the outbox is closed.
func (o *outbox) run() {
	for {
		o.mu.Lock()
		if o.closed || len(o.queue) == 0 {
			o.running = false
			close(o.idle)
			o.mu.Unlock()
			return
		}
		batch := slices.Clone(o.queue[:min(len(o.queue), o.cfg.window)])
		for _, c := range batch {
			c.sending = true
		}
		o.mu.Unlock()

		errs := o.send(batch)

		o.mu.Lock()
		removed := false
		for i, c := range batch {
			c.sending = false
			if errs[i] == nil || bus.PermanentPublishError(errs[i]) {
				if j := slices.Index(o.queue, c); j >= 0 {
					o.removeLocked(j)
					removed = true
				}
			}
		}
		if removed {
			o.signalLocked()
		}
		o.mu.Unlock()
		o.settle(batch, errs)
	}
}

// send puts batch on the wire in order and waits for each chunk's ack. Each
// chunk is given the ack timeout from when the chunk before it was acked, or
// from the send for the first: the stream acks a send's chunks one after
// another, and on a slow link the last of them is still on the wire long
// after the first was stored. It returns each chunk's outcome. A chunk the
// transport refuses for a reason a retry can cure, as while reconnecting,
// ends the send there, and the chunks after it fail with the same error, so
// none is stored ahead of it. A chunk no retry can send is dropped alone.
func (o *outbox) send(batch []*pendingChunk) []error {
	errs := make([]error, len(batch))
	waits := make([]AckWait, len(batch))
	for i, c := range batch {
		if i > 0 && errs[i-1] != nil && !bus.PermanentPublishError(errs[i-1]) {
			errs[i] = errs[i-1]
			continue
		}
		waits[i], errs[i] = o.t.Publish(c.subj, c.data, c.msgID)
	}
	// Once a chunk's ack is overdue the link is presumed silent: the chunks
	// after it count only if their acks are already in, so a send fails
	// after one ack timeout rather than one per chunk.
	overdue, stop := context.WithCancel(o.stopCtx)
	stop()
	timedOut := false
	for i, wait := range waits {
		if errs[i] != nil {
			continue
		}
		if timedOut {
			errs[i] = wait(overdue)
			continue
		}
		ctx, cancel := context.WithTimeout(o.stopCtx, o.cfg.ackTimeout)
		errs[i] = wait(ctx)
		cancel()
		timedOut = errors.Is(errs[i], context.DeadlineExceeded)
	}
	return errs
}

// settle logs a send's outcome and, when a chunk of it can be retried, waits
// out the backoff before the next send.
func (o *outbox) settle(batch []*pendingChunk, errs []error) {
	var retry, lost, stored *pendingChunk
	var retryErr, lostErr error
	lostCount := 0
	for i, err := range errs {
		switch {
		case err == nil:
			stored = batch[i]
		case bus.PermanentPublishError(err):
			if lost == nil {
				lost, lostErr = batch[i], err
			}
			lostCount++
		case retry == nil:
			retry, retryErr = batch[i], err
		}
	}
	if lost != nil {
		o.logger.ErrorContext(context.Background(), "logstreamer: log chunk cannot be sent; dropped",
			slog.String("subject", lost.subj), slog.String("msg_id", lost.msgID),
			slog.Int("chunks", lostCount), slog.Any("error", lostErr))
	}
	switch {
	case o.stopCtx.Err() != nil:
		// Closed; the next pass exits.
	case retry != nil:
		o.retryWait(retry, retryErr)
	case stored != nil:
		o.stored(stored)
	}
}

// stored logs that a chunk was stored after failed sends, and resets the
// failure count.
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
