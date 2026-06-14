// SPDX-License-Identifier: AGPL-3.0-or-later

// Package natsclient provides the NATS client factory for sqi-worker.
//
// Unlike the server's internal/bus package — which dials a loopback embedded
// broker — the worker connects to a remote NATS server hosted by sqi-server.
// The connection is configured with:
//
//   - Exponential backoff reconnection with jitter so a worker
//     restart storm does not spike load on the server.
//   - Optional TLS, including mutual TLS and InsecureSkipVerify for
//     development environments.
//   - Structured slog logging for all lifecycle events (connect, disconnect,
//     reconnect, close).
//
// Typical usage:
//
//	nc, closedCh, err := natsclient.Connect(ctx, cfg.NATS, logger)
//	if err != nil {
//	    return fmt.Errorf("nats connect: %w", err)
//	}
//	defer natsclient.Drain(nc, gracePeriod, logger)
package natsclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"time"

	nats "github.com/nats-io/nats.go"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
)

const (
	// maxBackoff is the upper bound on the reconnect delay, regardless of how
	// many reconnect attempts have elapsed.
	maxBackoff = 2 * time.Minute

	// jitterFraction controls how much randomness is added to each backoff
	// interval. At 0.2 the actual delay is base ± 20%.
	jitterFraction = 0.2
)

// Connect dials the NATS server described by cfg and returns a connected
// *nats.Conn and a lifecycle channel. The connection is configured for:
//
//   - MaxReconnects from cfg.MaxReconnectAttempts (-1 = unlimited).
//   - Exponential backoff starting at cfg.ReconnectWait, doubling each attempt,
//     capped at [maxBackoff], with ±[jitterFraction] randomness added so
//     simultaneous worker restarts spread their reconnect storms.
//   - TLS when any of cfg.TLSCertFile, cfg.TLSKeyFile, or cfg.TLSCAFile is
//     non-empty, or when cfg.InsecureSkipVerify is true.
//
// A connection failure at dial time is returned as an error. After the initial
// connection is established, reconnect failures are handled internally with
// backoff until the configured max attempts are exhausted, at which point the
// connection transitions to a closed state and the returned closedCh is closed.
//
// Callers should select on closedCh to detect permanent disconnects that occur
// outside of a planned shutdown sequence.
func Connect(ctx context.Context, cfg workerconfig.NATSConfig, logger *slog.Logger) (*nats.Conn, <-chan struct{}, error) {
	// closedCh is closed by the ClosedHandler callback when the NATS connection
	// permanently closes (MaxReconnects exhausted or explicit nc.Close() call).
	closedCh := make(chan struct{})

	opts, err := buildOptions(ctx, cfg, logger, closedCh)
	if err != nil {
		return nil, nil, fmt.Errorf("natsclient: build options: %w", err)
	}

	nc, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("natsclient: connect %q: %w", cfg.URL, err)
	}

	logger.InfoContext(
		ctx, "natsclient: connected",
		slog.String("url", nc.ConnectedUrl()),
		slog.String("server_id", nc.ConnectedServerId()),
	)

	return nc, closedCh, nil
}

// Drain gracefully closes nc by draining in-flight subscriptions and flushing
// any pending publishes before closing the connection. It blocks until the
// drain completes or gracePeriod elapses. If the grace period expires first,
// the connection is force-closed via [nats.Conn.Close].
//
// Drain is idempotent with respect to an already-closed connection: errors
// from draining a closed connection are silently ignored.
func Drain(nc *nats.Conn, gracePeriod time.Duration, logger *slog.Logger) {
	bg := context.Background()
	done := make(chan struct{})

	go func() {
		defer close(done)
		if err := nc.Drain(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) {
			logger.WarnContext(bg, "natsclient: drain error", slog.Any("error", err))
		}
	}()

	select {
	case <-done:
		logger.InfoContext(bg, "natsclient: connection drained")
	case <-time.After(gracePeriod):
		logger.WarnContext(bg, "natsclient: drain timed out — force closing connection")
		nc.Close()
	}
}

// buildOptions assembles the nats.Option slice from WorkerNATSConfig.
// closedCh is closed by the ClosedHandler when the connection permanently
// closes so callers can detect unexpected disconnects.
func buildOptions(ctx context.Context, cfg workerconfig.NATSConfig, logger *slog.Logger, closedCh chan struct{}) ([]nats.Option, error) {
	opts := []nats.Option{
		nats.MaxReconnects(cfg.MaxReconnectAttempts),

		// ── Exponential backoff with jitter ──────────────────────
		//
		// CustomReconnectDelay replaces the fixed ReconnectWait with a
		// function that receives the number of attempts elapsed and returns
		// the delay before the next attempt.
		//
		// Formula: base * 2^attempts, capped at maxBackoff, then ±jitter.
		nats.CustomReconnectDelay(exponentialBackoff(cfg.ReconnectWait)),

		// ── Lifecycle logging ──────────────────────────────────────────────
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				logger.WarnContext(ctx, "natsclient: disconnected", slog.Any("error", err))
			} else {
				logger.InfoContext(ctx, "natsclient: disconnected cleanly")
			}
		}),
		// ReconnectHandler is intentionally omitted here. The worker's
		// registration package installs its own reconnect callback via
		// nc.SetReconnectHandler after the connection is established, which
		// logs the reconnect and re-publishes the registration message in one
		// step. Installing a handler here would be overwritten before any
		// reconnect can occur and would never fire.
		//
		// ClosedHandler fires when the connection transitions to the CLOSED
		// state: either MaxReconnects was exhausted or the connection was
		// explicitly closed. Closing closedCh signals any goroutine that is
		// watching for unexpected permanent disconnects.
		nats.ClosedHandler(func(_ *nats.Conn) {
			logger.InfoContext(ctx, "natsclient: connection closed")
			close(closedCh)
		}),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			logger.ErrorContext(ctx, "natsclient: async error", slog.Any("error", err))
		}),
	}

	// ── TLS ──────────────────────────────────────────────────────
	tlsOpts, err := buildTLSOptions(cfg)
	if err != nil {
		return nil, err
	}
	opts = append(opts, tlsOpts...)

	return opts, nil
}

// exponentialBackoff returns a CustomReconnectDelay function that doubles the
// base wait on each attempt, caps at maxBackoff, and adds ±jitterFraction
// randomness so simultaneous worker restarts spread their reconnect times.
//
// attempts is zero-indexed: attempt 0 is the first reconnect after disconnect.
func exponentialBackoff(base time.Duration) func(int) time.Duration {
	return func(attempts int) time.Duration {
		// 2^attempts growth, capped to avoid overflow on large attempt counts.
		exp := math.Min(float64(attempts), 10) // 2^10 = 1024× base is already ≥ maxBackoff for any sane base
		delay := min(time.Duration(float64(base)*math.Pow(2, exp)), maxBackoff)

		// Add ±jitterFraction randomness. math/rand/v2 is sufficient for
		// backoff jitter — cryptographic randomness is not required here.
		jitter := time.Duration(float64(delay) * jitterFraction * (rand.Float64()*2 - 1)) //nolint:gosec // jitter does not require cryptographic randomness
		delay += jitter
		if delay < 0 {
			delay = 0
		}
		return delay
	}
}

// buildTLSOptions returns the nats.Option(s) needed to configure TLS on the
// connection. It returns an empty slice when no TLS configuration is provided.
//
// TLS is activated when any of the following are set:
//   - cfg.TLSCertFile / cfg.TLSKeyFile  — mutual TLS client certificate
//   - cfg.TLSCAFile                     — custom CA for server cert verification
//   - cfg.InsecureSkipVerify            — disable server cert verification
func buildTLSOptions(cfg workerconfig.NATSConfig) ([]nats.Option, error) {
	wantTLS := cfg.TLSCertFile != "" || cfg.TLSKeyFile != "" ||
		cfg.TLSCAFile != "" || cfg.InsecureSkipVerify

	if !wantTLS {
		return nil, nil
	}

	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // explicitly user-requested
	}

	// Mutual TLS: load client certificate keypair.
	if cfg.TLSCertFile != "" || cfg.TLSKeyFile != "" {
		if cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" {
			return nil, errors.New("natsclient: tls_cert_file and tls_key_file must both be set")
		}
		cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("natsclient: load client cert: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	// Custom CA for verifying the server's certificate.
	if cfg.TLSCAFile != "" {
		pem, err := os.ReadFile(cfg.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("natsclient: read CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("natsclient: no valid certificates found in CA file %s", cfg.TLSCAFile)
		}
		tlsCfg.RootCAs = pool
	}

	return []nats.Option{nats.Secure(tlsCfg)}, nil
}
