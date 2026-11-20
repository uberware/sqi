// SPDX-License-Identifier: AGPL-3.0-or-later

package logstreamer

import (
	"context"
	"fmt"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
)

// Transport delivers one encoded log chunk and returns once the log stream has
// stored it, or with the reason it did not.
type Transport interface {
	// PublishAcked publishes data to subj with msgID as its Nats-Msg-Id and
	// waits for the stream's ack, or for ctx to end.
	PublishAcked(ctx context.Context, subj string, data []byte, msgID string) error
}

// jetStreamTransport publishes chunks through JetStream on the worker's own
// connection and waits for SQI_LOGS's ack. Like the status transport it needs
// no permission beyond the worker's grant: the publish goes to
// task.logs.<workerID>.<task> and the ack comes back on the connection's own
// inbox.
//
// It refuses a try while the connection is reconnecting, so the retries of a
// chunk do not pile up in the client's reconnect buffer; the publisher tries
// again once the connection is back. A try written just before the client
// notices a drop is still buffered and sent on reconnect, and the stream drops
// the later retries as duplicates of its message ID.
type jetStreamTransport struct {
	nc *nats.Conn
	js jetstream.JetStream
}

// NewJetStreamTransport returns a [Transport] that publishes on nc and returns
// only once SQI_LOGS has stored the chunk.
func NewJetStreamTransport(nc *nats.Conn) (Transport, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("logstreamer: jetstream context: %w", err)
	}
	return &jetStreamTransport{nc: nc, js: js}, nil
}

func (t *jetStreamTransport) PublishAcked(ctx context.Context, subj string, data []byte, msgID string) error {
	if st := t.nc.Status(); st == nats.RECONNECTING || st == nats.CONNECTING {
		return fmt.Errorf("logstreamer: publish %s: %w", msgID, nats.ErrConnectionReconnecting)
	}
	_, err := t.js.PublishMsg(
		ctx, &nats.Msg{Subject: subj, Data: data},
		jetstream.WithExpectStream(bus.StreamLogs),
		jetstream.WithMsgID(msgID),
	)
	if err != nil {
		return fmt.Errorf("logstreamer: publish %s: %w", msgID, err)
	}
	return nil
}
