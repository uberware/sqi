// SPDX-License-Identifier: AGPL-3.0-or-later

package status

import (
	"context"
	"fmt"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
)

// jetStreamTransport publishes reports through JetStream on the worker's own
// connection and waits for SQI_TASK's ack.
//
// It needs no permission beyond the worker's broker grant: the publish goes
// to task.status.<workerID>.<job>, and the ack comes back on the connection's
// reply inbox, which carries the worker's own inbox prefix. It never calls
// the JetStream API ($JS.API.>): the expected stream travels as a header.
//
// It refuses a try while the connection is reconnecting rather than let the
// client hold it in its reconnect buffer. A buffered try is sent when the
// connection comes back, carrying the ended_ago_ms it was built with, and the
// stream stores it; the publisher's later tries, built with the right age,
// are then dropped as duplicates of its message ID, so the server would count
// the whole outage as run time.
//
// The refusal narrows that window but does not close it: the status check and
// the publish are not atomic, so a try written just before the client notices
// the connection has dropped is still buffered and sent, with its age, once
// the connection comes back.
type jetStreamTransport struct {
	nc *nats.Conn
	js jetstream.JetStream
}

// NewJetStreamTransport returns a [Transport] that publishes on nc and returns
// only once SQI_TASK has stored the report.
func NewJetStreamTransport(nc *nats.Conn) (Transport, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("status: jetstream context: %w", err)
	}
	return &jetStreamTransport{nc: nc, js: js}, nil
}

// PublishAcked publishes data to subj with msgID as its Nats-Msg-Id and waits
// for the stream's ack, or for ctx to end. While the connection is
// reconnecting it publishes nothing and returns [nats.ErrConnectionReconnecting],
// which the publisher retries.
func (t *jetStreamTransport) PublishAcked(ctx context.Context, subj string, data []byte, msgID string) error {
	if bus.Reconnecting(t.nc) {
		return fmt.Errorf("status: publish %s: %w", msgID, nats.ErrConnectionReconnecting)
	}
	_, err := t.js.PublishMsg(
		ctx, &nats.Msg{Subject: subj, Data: data},
		jetstream.WithExpectStream(bus.StreamTask),
		jetstream.WithMsgID(msgID),
	)
	if err != nil {
		return fmt.Errorf("status: publish %s: %w", msgID, err)
	}
	return nil
}
