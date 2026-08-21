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
//	nc, closedCh, err := natsclient.Connect(ctx, cfg.NATS, seed, publicKey, logger)
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
	"github.com/nats-io/nkeys"

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
//
// seed and publicKey are the worker's nkey broker credential, obtained
// separately (see internal/worker/enroll). They are taken as parameters
// rather than folded into cfg so that a seed never sits in a config struct
// that might get logged elsewhere. An empty seed connects with no
// credential, which is correct on a farm that does not require worker
// authentication; see buildOptions for how that case is distinguished from a
// broker that actively rejects the connection.
func Connect(ctx context.Context, cfg workerconfig.NATSConfig, seed []byte, publicKey string, logger *slog.Logger) (*nats.Conn, <-chan struct{}, error) {
	// closedCh is closed by the ClosedHandler callback when the NATS connection
	// permanently closes (MaxReconnects exhausted or explicit nc.Close() call).
	closedCh := make(chan struct{})

	opts, err := buildOptions(ctx, cfg, seed, publicKey, logger, closedCh)
	if err != nil {
		return nil, nil, fmt.Errorf("natsclient: build options: %w", err)
	}

	nc, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		// An authorization failure is FATAL and must never enter the
		// reconnect-backoff loop: retrying a rejected credential produces a
		// worker that never appears, with the reason buried in backoff. An
		// unreachable server is the opposite — that is what backoff is for.
		if wrapped, ok := credentialRejectedError(err); ok {
			return nil, nil, wrapped
		}
		return nil, nil, fmt.Errorf("natsclient: connect %q: %w", cfg.URL, err)
	}

	logger.InfoContext(
		ctx, "natsclient: connected",
		slog.String("url", nc.ConnectedUrl()),
		slog.String("server_id", nc.ConnectedServerId()),
	)

	return nc, closedCh, nil
}

// credentialRejectedError reports whether err is the broker rejecting this
// connection's nkey credential — nats.ErrAuthorization (unknown or wrong
// key) or nats.ErrAuthExpired (a key the broker no longer accepts, notably
// after a live revocation: internal/bus.Broker.ReloadCredentials drops the
// key and synchronously disconnects any client using it). When it is, the
// second return is true and the first is the single crafted message every
// credential-rejection path uses — the initial dial in [Connect] and a later
// live revocation observed via [nats.Conn.LastError] in the ClosedHandler
// below both call this, so the two paths cannot drift apart.
func credentialRejectedError(err error) (error, bool) {
	if err == nil {
		return nil, false
	}
	if !errors.Is(err, nats.ErrAuthorization) && !errors.Is(err, nats.ErrAuthExpired) {
		return nil, false
	}
	return fmt.Errorf(
		"natsclient: the broker rejected this worker's credential — it may have been revoked; re-enroll with a new join token: %w", err,
	), true
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
// closes so callers can detect unexpected disconnects. seed and publicKey,
// when non-empty, add an nkey signing option so the connection authenticates
// as this worker's broker credential.
func buildOptions(
	ctx context.Context,
	cfg workerconfig.NATSConfig,
	seed []byte,
	publicKey string,
	logger *slog.Logger,
	closedCh chan struct{},
) ([]nats.Option, error) {
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
		//
		// A live credential revocation (internal/bus.Broker.ReloadCredentials
		// disconnects the client synchronously via authViolation) lands here,
		// not at the initial dial in [Connect] — the connection was already
		// established when the credential stopped being valid. nats.Conn's
		// own doc for LastError says it "can be used reliably within
		// ClosedCB in order to find out reason why connection was closed",
		// so this is the one place that case can be classified and named,
		// rather than surfacing only as a generic closure to the operator.
		nats.ClosedHandler(func(nc *nats.Conn) {
			if wrapped, ok := credentialRejectedError(nc.LastError()); ok {
				logger.ErrorContext(ctx, "natsclient: connection closed", slog.Any("error", wrapped))
			} else {
				logger.InfoContext(ctx, "natsclient: connection closed")
			}
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

	// ── Broker credential ────────────────────────────────────────
	//
	// A non-empty seed authenticates this connection as the worker's
	// enrolled nkey. An empty seed adds no option at all, which is correct
	// on a farm that does not require worker authentication — the broker
	// accepts the anonymous connection exactly as it does today. When the
	// broker DOES require authentication, connecting with no credential (or
	// a rejected one) fails at nats.Connect above with nats.ErrAuthorization
	// or nats.ErrAuthExpired, which is classified as fatal rather than
	// retried.
	if len(seed) > 0 {
		opts = append(opts, nats.Nkey(publicKey, func(nonce []byte) ([]byte, error) {
			kp, err := nkeys.FromSeed(seed)
			if err != nil {
				return nil, err
			}
			return kp.Sign(nonce)
		}))
	}

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
