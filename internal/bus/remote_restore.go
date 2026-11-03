// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/redact"
)

// purgeOpTimeout bounds one stream purge or consumer deletion request. A
// purge of a large replicated stream takes longer than a KV write.
const purgeOpTimeout = 30 * time.Second

// DurableConsumer is a durable consumer and the stream it reads.
type DurableConsumer struct {
	Stream string
	Name   string
}

// RestoreTargets returns what [RestoreLock.Purge] empties: every one of sqi's
// streams, and the durable consumers a running sqi-server keeps on them.
func RestoreTargets() (streams []string, consumers []DurableConsumer) {
	for _, def := range streamDefs(1) {
		streams = append(streams, def.Name)
	}
	return streams, []DurableConsumer{
		{Stream: StreamTask, Name: taskStatusConsumerName},
		{Stream: StreamLogs, Name: taskLogsConsumerName},
		{Stream: StreamWorker, Name: workerConsumerName},
	}
}

// RestoreLock is a broker account held by "sqi-server restore": a connection
// on the data credential, and the account's guard. While it is held no
// sqi-server can start on the account, and a server refused names the
// restore as the holder.
type RestoreLock struct {
	r     *Remote
	nc    *nats.Conn
	js    jetstream.JetStream
	guard *Guard
	once  sync.Once
}

// LockForRestore connects with the data credential alone, with no callout
// responder and no system connection, checks the broker's version and that
// the account has JetStream, and takes the account's guard as a restore. It
// returns an error wrapping [ErrGuardHeld], and changes nothing, while an
// sqi-server runs on the account.
//
// It changes nothing on the broker but the guard key: on an account no server
// has used it creates the guard bucket, and an existing bucket keeps its
// configuration. Call it on a Remote that is not started, and call
// [RestoreLock.Release] when done.
func (r *Remote) LockForRestore(ctx context.Context) (*RestoreLock, error) {
	r.mu.Lock()
	started := r.started
	r.mu.Unlock()
	if started {
		return nil, errors.New("bus: a started broker cannot be held for a restore")
	}
	if err := r.cfg.validate(); err != nil {
		return nil, err
	}
	dialOpts, err := r.cfg.dialOptions()
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(r.cfg.ConnectTimeout)
	nc, err := r.dial(ctx, roleRestore, dialOpts, deadline)
	if err != nil {
		return nil, err
	}
	l, err := r.lockOn(ctx, nc, deadline)
	if err != nil {
		nc.Close()
		return nil, err
	}
	return l, nil
}

// lockOn checks the broker nc reached and takes the guard on it.
func (r *Remote) lockOn(ctx context.Context, nc *nats.Conn, deadline time.Time) (*RestoreLock, error) {
	if err := CheckBrokerVersion(brokerVersion(nc)); err != nil {
		return nil, fmt.Errorf("%w (broker at %s)", err, redact.URL(nc.ConnectedUrl()))
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("bus: jetstream context: %w", err)
	}
	if err := r.requireAccountJetStream(ctx, js, deadline); err != nil {
		return nil, err
	}
	guard, err := r.acquireGuard(ctx, js, deadline, GuardPurposeRestore)
	if err != nil {
		return nil, err
	}
	return &RestoreLock{r: r, nc: nc, js: js, guard: guard}, nil
}

// Purge empties every one of sqi's streams and deletes the server's durable
// consumers, so that no message from before the restore replays against the
// restored database. A stream or consumer that does not exist, on an account
// no server finished starting on, is skipped. The streams keep their
// configuration, replica count included. Requests the cluster cannot serve
// yet are retried for up to the connect timeout.
//
// It refuses when the lock no longer holds the guard: another sqi-server may
// be running on the account.
func (l *RestoreLock) Purge(ctx context.Context) error {
	if l.guard != nil {
		if err := l.guard.Healthy(); err != nil {
			return fmt.Errorf("bus: purge for restore: %w", err)
		}
	}
	deadline := time.Now().Add(l.r.cfg.ConnectTimeout)
	streams, consumers := RestoreTargets()
	for _, name := range streams {
		err := retryUntil(ctx, deadline, func() error { return purgeStream(ctx, l.js, name) }, transientJetStreamError)
		if err != nil {
			return fmt.Errorf("bus: purge stream %s: %w", name, err)
		}
	}
	names := make([]string, 0, len(consumers))
	for _, c := range consumers {
		err := retryUntil(ctx, deadline, func() error { return deleteDurable(ctx, l.js, c) }, transientJetStreamError)
		if err != nil {
			return fmt.Errorf("bus: delete consumer %s on stream %s: %w", c.Name, c.Stream, err)
		}
		names = append(names, c.Name)
	}
	l.r.logger.InfoContext(ctx, "bus: purged sqi's jetstream state on the external broker",
		slog.String("account", l.r.cfg.Account),
		slog.String("streams", strings.Join(streams, ",")),
		slog.String("consumers_deleted", strings.Join(names, ",")))
	return nil
}

// Release gives the account's guard back and closes the connection. It is
// safe to call more than once.
func (l *RestoreLock) Release() {
	l.once.Do(func() {
		l.r.releaseGuard(l.guard)
		l.nc.Close()
	})
}

// purgeStream removes every message from the stream name, if it exists.
func purgeStream(ctx context.Context, js jetstream.JetStream, name string) error {
	ctx, cancel := context.WithTimeout(ctx, purgeOpTimeout)
	defer cancel()
	s, err := js.Stream(ctx, name)
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.Purge(ctx)
}

// deleteDurable deletes the consumer c, if it and its stream exist.
func deleteDurable(ctx context.Context, js jetstream.JetStream, c DurableConsumer) error {
	ctx, cancel := context.WithTimeout(ctx, purgeOpTimeout)
	defer cancel()
	err := js.DeleteConsumer(ctx, c.Stream, c.Name)
	if errors.Is(err, jetstream.ErrConsumerNotFound) || errors.Is(err, jetstream.ErrStreamNotFound) {
		return nil
	}
	return err
}
