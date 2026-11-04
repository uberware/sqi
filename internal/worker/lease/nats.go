// SPDX-License-Identifier: AGPL-3.0-or-later

package lease

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/bus"
)

// NATSTransport adapts a raw *nats.Conn to [Transport], issuing core-NATS
// request/reply work-lease requests on the work.lease.<worker>.<queue>
// subject. It provides the same RequestLease behavior as
// [bus.Client.RequestLease] without a second client connection.
type NATSTransport struct {
	NC *nats.Conn
}

// RequestLease sends a work-lease request for queueID on behalf of workerID and
// waits up to timeout for the server's reply. It returns the raw reply bytes (a
// marshaled leaseReply) for the lease loop to decode.
func (t NATSTransport) RequestLease(ctx context.Context, workerID, queueID string, data []byte, timeout time.Duration) ([]byte, error) {
	reqCtx, cancelReq := context.WithTimeout(ctx, timeout)
	defer cancelReq()
	msg, err := t.NC.RequestWithContext(reqCtx, bus.WorkLeaseSubject(workerID, queueID), data)
	if err != nil {
		return nil, fmt.Errorf("lease: request lease for queue %q: %w", queueID, err)
	}
	return msg.Data, nil
}

// QueueIDs maps a worker's configured queue list to the queues it requests
// leases on. A worker with no configured queues serves any queue; it must still
// request on a valid subject, so it uses [bus.WildcardQueueToken]
// (work.lease.<worker>._any) rather than an empty queue token, which routes to
// no responder. The server selects tasks farm-wide and gates by eligibility, so
// a queue-unaffiliated worker is matched to any queue's ready work.
func QueueIDs(configured []string) []string {
	if len(configured) == 0 {
		return []string{bus.WildcardQueueToken}
	}
	return configured
}
