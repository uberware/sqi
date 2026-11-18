// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"sync"
	"time"

	nats "github.com/nats-io/nats.go"
)

// RequestLease sends a core-NATS work-lease request for queueID on behalf of
// workerID and waits up to timeout for the server's reply. Returns
// nats.ErrNoResponders immediately if no server is subscribed, or
// nats.ErrTimeout if no reply arrives in time.
func (c *Client) RequestLease(ctx context.Context, workerID, queueID string, data []byte, timeout time.Duration) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	msg, err := c.nc.RequestWithContext(reqCtx, WorkLeaseSubject(workerID, queueID), data)
	if err != nil {
		return nil, err
	}
	return msg.Data, nil
}

// LeaseSubscription is the server's subscription to work-lease requests.
type LeaseSubscription struct {
	nc  *nats.Conn
	sub *nats.Subscription
	// inFlight counts the handlers still answering a request.
	inFlight sync.WaitGroup
}

// Unsubscribe removes the subscription at once, without waiting for the
// requests it has taken to be answered. Tests use it for cleanup; a server
// stops with [LeaseSubscription.Stop].
func (l *LeaseSubscription) Unsubscribe() error {
	return l.sub.Unsubscribe()
}

// Stop stops taking requests, then calls wake, then returns once every
// request already taken has been answered, or when ctx ends. wake is called
// exactly once, whatever Stop returns, and is where the caller releases the
// replies it holds back while stopping (see below).
//
// The subscription is drained, not unsubscribed, so the client keeps
// delivering until the broker node it is connected to has stopped routing
// here: until then a request is delivered and answered, and after it one
// sent to that node finds no responder, and the worker retries after its
// brief backoff rather than waiting out its whole request timeout. On a
// cluster, a node that has not yet learnt the interest is gone still
// forwards a request, which is then dropped. The caller narrows that window
// by sending no reply while stopping until wake: a reply then follows the
// interest removal over the same route, so the requester's node has seen it
// go before the requester can ask again. Waiting for the handlers matters
// because the server closes its connection right after the scheduler stops,
// and a reply sent after that is lost too.
func (l *LeaseSubscription) Stop(ctx context.Context, wake func()) error {
	woken := false
	defer func() {
		if !woken {
			wake()
		}
	}()
	if err := l.sub.Drain(); err != nil {
		return err
	}
	tick := time.NewTicker(leaseDrainPoll)
	defer tick.Stop()
	// The client removes a drained subscription only after a flush that
	// follows the unsubscribe, and once every message it delivered has
	// returned from the callback, which is where each one was counted.
	for l.sub.IsValid() {
		if l.nc.IsClosed() {
			return nats.ErrConnectionClosed // nothing taken can be answered now
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
	// A closed connection also invalidates the subscription, with a callback
	// possibly still counting a request; nothing could be answered anyway.
	if l.nc.IsClosed() {
		return nats.ErrConnectionClosed
	}
	woken = true
	wake()
	answered := make(chan struct{})
	go func() {
		l.inFlight.Wait()
		close(answered)
	}()
	select {
	case <-answered:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// leaseDrainPoll is how often [LeaseSubscription.Stop] checks whether the
// drained subscription has been removed.
const leaseDrainPoll = 10 * time.Millisecond

// SubscribeLease subscribes to work-lease requests from all workers for all
// queues (work.lease.>) and replies with the bytes handler returns. The
// requesting worker's ID and the queue it asked about are taken from the
// subject. handler does its own long-poll blocking before returning; it must
// respect no work by returning an empty/again-marker payload of the caller's
// choosing.
func (c *Client) SubscribeLease(handler func(workerID, queueID string, data []byte) []byte) (*LeaseSubscription, error) {
	l := &LeaseSubscription{nc: c.nc}
	// Spawn a goroutine per message so a parked handler (a long-poll lease that
	// blocks up to leaseHoldTimeout) never stalls delivery of other workers'
	// requests — NATS delivers one-at-a-time per subscription callback.
	// WaitGroup.Go counts the goroutine here, on the delivery goroutine, so a
	// drained subscription has counted every request it delivered.
	sub, err := c.nc.Subscribe(SubjectWorkLeasePrefix+".>", func(msg *nats.Msg) {
		l.inFlight.Go(func() {
			workerID, queueID, ok := ParseWorkerSubject(msg.Subject)
			if !ok {
				// A subject shape this server does not speak. Do not reply:
				// the requester times out and retries, which is the same path
				// an unreachable server produces.
				return
			}
			reply := handler(workerID, queueID, msg.Data)
			_ = msg.Respond(reply) //nolint:errcheck // best-effort reply; worker retries on timeout
		})
	})
	if err != nil {
		return nil, err
	}
	// Flush so the interest is registered on the server before we return.
	// Otherwise a RequestLease published immediately afterwards can outrace the
	// SUB and fail with nats.ErrNoResponders (a request/reply race the worker
	// would have to time out and retry through).
	if err := c.nc.Flush(); err != nil {
		_ = sub.Unsubscribe() //nolint:errcheck // best-effort cleanup on a failed setup
		return nil, err
	}
	l.sub = sub
	return l, nil
}
