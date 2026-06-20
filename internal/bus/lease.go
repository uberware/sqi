// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"strings"
	"time"

	nats "github.com/nats-io/nats.go"
)

// RequestLease sends a core-NATS work-lease request for queueID and waits up to
// timeout for the server's reply. Returns nats.ErrNoResponders immediately if
// no server is subscribed, or nats.ErrTimeout if no reply arrives in time.
func (c *Client) RequestLease(ctx context.Context, queueID string, data []byte, timeout time.Duration) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	msg, err := c.nc.RequestWithContext(reqCtx, WorkLeaseSubject(queueID), data)
	if err != nil {
		return nil, err
	}
	return msg.Data, nil
}

// SubscribeLease subscribes to work-lease requests for all queues
// (work.lease.>) and replies with the bytes handler returns. handler does its
// own long-poll blocking before returning; it must respect no work by returning
// an empty/again-marker payload of the caller's choosing.
func (c *Client) SubscribeLease(handler func(queueID string, data []byte) []byte) (*nats.Subscription, error) {
	// Spawn a goroutine per message so a parked handler (a long-poll lease that
	// blocks up to leaseHoldTimeout) never stalls delivery of other workers'
	// requests — NATS delivers one-at-a-time per subscription callback.
	sub, err := c.nc.Subscribe(SubjectWorkLeasePrefix+".>", func(msg *nats.Msg) {
		go func() {
			queueID := strings.TrimPrefix(msg.Subject, SubjectWorkLeasePrefix+".")
			reply := handler(queueID, msg.Data)
			_ = msg.Respond(reply) //nolint:errcheck // best-effort reply; worker retries on timeout
		}()
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
	return sub, nil
}
