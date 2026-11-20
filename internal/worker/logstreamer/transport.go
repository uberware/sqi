// SPDX-License-Identifier: AGPL-3.0-or-later

package logstreamer

import (
	"context"
	"fmt"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
)

// Transport sends encoded log chunks and reports whether the log stream
// stored each one.
type Transport interface {
	// Publish sends data to subj with msgID as its Nats-Msg-Id without
	// waiting for the stream, and returns the wait for its ack. Chunks sent
	// one after another reach the stream in that order. An error means the
	// chunk was not sent.
	Publish(subj string, data []byte, msgID string) (AckWait, error)
}

// AckWait waits for the stream's ack of one sent chunk: nil once the stream
// stored it, or the reason it did not, or ctx's error when ctx ends first. An
// outcome already in when it is called is returned even if ctx has ended.
type AckWait func(ctx context.Context) error

// jetStreamTransport publishes chunks through JetStream on the worker's own
// connection and waits for SQI_LOGS's ack. Like the status transport it needs
// no permission beyond the worker's grant: the publish goes to
// task.logs.<workerID>.<task> and the ack comes back under the connection's
// own inbox prefix.
//
// It refuses a send while the connection is reconnecting, so the retries of a
// chunk do not pile up in the client's reconnect buffer; the publisher tries
// again once the connection is back. A send written just before the client
// notices a drop is still buffered and sent on reconnect, and the stream drops
// the later retries as duplicates of its message ID.
type jetStreamTransport struct {
	nc *nats.Conn
	js jetstream.JetStream
}

// asyncAckGiveUp is when the client forgets a chunk whose ack never came, so
// a lost ack holds nothing in it. The publisher's own wait decides a chunk's
// outcome; this only has to outlast it, and the client starts the clock at
// the send, while a send's last chunk is waited for only once the chunks
// before it were acked.
const asyncAckGiveUp = 2 * defaultWindow * defaultAckTimeout

// NewJetStreamTransport returns a [Transport] that publishes on nc.
func NewJetStreamTransport(nc *nats.Conn) (Transport, error) {
	js, err := jetstream.New(nc, jetstream.WithPublishAsyncTimeout(asyncAckGiveUp))
	if err != nil {
		return nil, fmt.Errorf("logstreamer: jetstream context: %w", err)
	}
	return &jetStreamTransport{nc: nc, js: js}, nil
}

func (t *jetStreamTransport) Publish(subj string, data []byte, msgID string) (AckWait, error) {
	if bus.Reconnecting(t.nc) {
		return nil, fmt.Errorf("logstreamer: publish %s: %w", msgID, nats.ErrConnectionReconnecting)
	}
	f, err := t.js.PublishMsgAsync(
		&nats.Msg{Subject: subj, Data: data},
		jetstream.WithExpectStream(bus.StreamLogs),
		jetstream.WithMsgID(msgID),
	)
	if err != nil {
		return nil, fmt.Errorf("logstreamer: publish %s: %w", msgID, err)
	}
	return func(ctx context.Context) error {
		// An outcome already in wins over a ctx that has ended too.
		select {
		case <-f.Ok():
			return nil
		case err := <-f.Err():
			return fmt.Errorf("logstreamer: publish %s: %w", msgID, err)
		default:
		}
		select {
		case <-f.Ok():
			return nil
		case err := <-f.Err():
			return fmt.Errorf("logstreamer: publish %s: %w", msgID, err)
		case <-ctx.Done():
			return fmt.Errorf("logstreamer: publish %s: %w", msgID, ctx.Err())
		}
	}, nil
}
