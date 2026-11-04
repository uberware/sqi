// SPDX-License-Identifier: AGPL-3.0-or-later

package observe

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/loadtest/measure"
	"github.com/uberware/sqi/internal/loadtest/simworker"
	"github.com/uberware/sqi/internal/worker/natsclient"
)

// jsStreams are the streams the server provisions.
var jsStreams = bus.StreamNames()

// jsConnectTimeout bounds one dial of the broker.
const jsConnectTimeout = 3 * time.Second

// errBrokerAuth is the permanent form of a refused connection: the broker
// wants a credential the observer does not hold.
var errBrokerAuth = errors.New("the broker requires authentication and the observer has no credential")

// jsReader reads stream and consumer state over a JetStream API connection of
// its own. It dials lazily and keeps the connection until close.
type jsReader struct {
	url   string
	files simworker.TLSFiles

	nc *nats.Conn
	js jetstream.JetStream
	// refused is the broker's authentication refusal. It is permanent for the
	// run, so the observer stops knocking on the broker's auth callout.
	refused error
}

// connect dials the broker if it is not connected.
func (r *jsReader) connect() error {
	if r.refused != nil {
		return r.refused
	}
	// A connection that dropped reconnects on its own; only a closed one is replaced.
	if r.nc != nil && !r.nc.IsClosed() {
		return nil
	}
	// TLS follows sqi-worker's rule, so the observer reaches the broker the
	// way the simulated workers do.
	tlsOpts, err := natsclient.TLSOptions(r.files.NATSConfig())
	if err != nil {
		return err
	}
	opts := append([]nats.Option{nats.Name("sqi-load-observe"), nats.Timeout(jsConnectTimeout), nats.MaxReconnects(-1)}, tlsOpts...)
	nc, err := nats.Connect(r.url, opts...)
	if err != nil {
		if natsclient.IsAuthRefusal(err) {
			r.refused = fmt.Errorf("%w: %w", errBrokerAuth, err)
			return r.refused
		}
		return fmt.Errorf("connect to %s: %w", r.url, err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return fmt.Errorf("JetStream API: %w", err)
	}
	r.nc, r.js = nc, js
	return nil
}

func (r *jsReader) close() {
	if r.nc != nil {
		r.nc.Close()
		r.nc, r.js = nil, nil
	}
}

// sample records every stream's bytes and messages and every consumer's
// pending count. A stream that cannot be read is reported by name in failures,
// and does not stop the others.
func (r *jsReader) sample(ctx context.Context, rec *measure.Recorder, t time.Time) (failures map[string]error) {
	failures = map[string]error{}
	for _, name := range jsStreams {
		if err := r.sampleStream(ctx, rec, t, name); err != nil {
			failures[name] = err
		}
	}
	return failures
}

func (r *jsReader) sampleStream(ctx context.Context, rec *measure.Recorder, t time.Time, name string) error {
	stream, err := r.js.Stream(ctx, name)
	if err != nil {
		return fmt.Errorf("stream %s: %w", name, err)
	}
	// Stream already fetched the stream's info; a second Info would repeat
	// the request.
	info := stream.CachedInfo()
	rec.Add(labeled("js.bytes", name), "bytes", t, float64(info.State.Bytes))
	rec.Add(labeled("js.msgs", name), "msgs", t, float64(info.State.Msgs))

	lister := stream.ListConsumers(ctx)
	for ci := range lister.Info() {
		rec.Add(labeled("js.pending", name+"/"+ci.Name), "msgs", t, float64(ci.NumPending))
	}
	if err := lister.Err(); err != nil {
		return fmt.Errorf("stream %s consumers: %w", name, err)
	}
	return nil
}
