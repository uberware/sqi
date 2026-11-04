// SPDX-License-Identifier: AGPL-3.0-or-later

package observe

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/loadtest/measure"
	"github.com/uberware/sqi/internal/loadtest/simworker"
	"github.com/uberware/sqi/internal/tlsutil"
)

// jsStreams are the streams the server provisions.
var jsStreams = []string{bus.StreamTask, bus.StreamLogs, bus.StreamWorker, bus.StreamCancel}

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
	opts := []nats.Option{nats.Name("sqi-load-observe"), nats.Timeout(jsConnectTimeout), nats.MaxReconnects(-1)}
	cfg, err := r.tlsConfig()
	if err != nil {
		return err
	}
	if cfg != nil {
		opts = append(opts, nats.Secure(cfg))
	}
	nc, err := nats.Connect(r.url, opts...)
	if err != nil {
		if errors.Is(err, nats.ErrAuthorization) || strings.Contains(strings.ToLower(err.Error()), "authorization") {
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

func (r *jsReader) tlsConfig() (*tls.Config, error) {
	f := r.files
	if f.CAFile == "" && f.CertFile == "" && f.Enabled != "true" && !f.InsecureSkipVerify {
		return nil, nil
	}
	cfg, err := tlsutil.ClientConfig(f.CAFile, f.CertFile, f.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("broker TLS: %w", err)
	}
	cfg.InsecureSkipVerify = f.InsecureSkipVerify
	return cfg, nil
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
	info, err := stream.Info(ctx)
	if err != nil {
		return fmt.Errorf("stream %s info: %w", name, err)
	}
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
