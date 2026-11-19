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
type jetStreamTransport struct {
	js jetstream.JetStream
}

// NewJetStreamTransport returns a [Transport] that publishes on nc and returns
// only once SQI_TASK has stored the report.
func NewJetStreamTransport(nc *nats.Conn) (Transport, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("status: jetstream context: %w", err)
	}
	return &jetStreamTransport{js: js}, nil
}

// PublishAcked publishes data to subj with msgID as its Nats-Msg-Id and waits
// for the stream's ack, or for ctx to end.
func (t *jetStreamTransport) PublishAcked(ctx context.Context, subj string, data []byte, msgID string) error {
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
