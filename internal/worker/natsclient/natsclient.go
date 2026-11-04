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
//	nc, closedCh, err := natsclient.Connect(ctx, cfg.NATS, workerID, seed, publicKey, logger)
//	if err != nil {
//	    return fmt.Errorf("nats connect: %w", err)
//	}
//	defer natsclient.Drain(nc, gracePeriod, logger)
package natsclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	nats "github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/redact"
	"github.com/uberware/sqi/internal/tlsutil"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// authRejectGrace is how long the worker keeps retrying a broker that
// refuses its credential before it treats the refusal as final.
//
// The broker's auth callout refuses a revoked or unknown key and a key it
// could not look up (sqi-server or its store unavailable) with the same
// "Authorization Violation", so a refusal alone cannot say which happened.
// The grace lets a worker ride out a database restart, or in external mode a
// sqi-server restart, while a revoked worker still stops. It is not operator
// configuration; tests pass a shorter grace to connect.
const authRejectGrace = 2 * time.Minute

// Connect dials the NATS server described by cfg and returns a connected
// *nats.Conn and a lifecycle channel. The connection is configured for:
//
//   - MaxReconnects from cfg.MaxReconnectAttempts (-1 = unlimited).
//   - Exponential backoff starting at cfg.ReconnectWait, doubling each attempt,
//     capped at [protocol.MaxReconnectBackoff], with
//     ±[protocol.ReconnectJitterFraction] randomness added so simultaneous
//     worker restarts spread their reconnect storms.
//   - TLS when any of cfg.TLSCertFile, cfg.TLSKeyFile, or cfg.TLSCAFile is
//     non-empty, or when cfg.InsecureSkipVerify is true.
//
// A connection failure at dial time is returned as an error; a refused
// credential is first retried for [authRejectGrace] (see dial). After the
// initial connection is established, reconnect failures are handled
// internally with backoff until the configured max attempts are exhausted,
// or a refused credential has been retried for the grace, at which point the
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
//
// workerID scopes this connection's reply inboxes to a per-worker prefix
// (see [brokerauth.InboxPrefix]). It is required whether or not a credential
// is present, so the connect path is identical in both modes, and it must be
// a single NATS subject token.
func Connect(ctx context.Context, cfg workerconfig.NATSConfig, workerID string, seed []byte, publicKey string, logger *slog.Logger) (*nats.Conn, <-chan struct{}, error) {
	return connect(ctx, cfg, workerID, seed, publicKey, logger, authRejectGrace)
}

// connect is [Connect] with grace in place of [authRejectGrace], and extra
// options applied after the ones buildOptions assembles.
func connect(
	ctx context.Context,
	cfg workerconfig.NATSConfig,
	workerID string,
	seed []byte,
	publicKey string,
	logger *slog.Logger,
	grace time.Duration,
	extra ...nats.Option,
) (*nats.Conn, <-chan struct{}, error) {
	lc := newLifecycle(ctx, logger, len(seed) > 0, cfg.ReconnectWait, grace)

	opts, err := buildOptions(cfg, workerID, seed, publicKey, lc)
	if err != nil {
		return nil, nil, fmt.Errorf("natsclient: build options: %w", err)
	}
	opts = append(opts, extra...)

	nc, err := dial(ctx, cfg, opts, lc)
	if err != nil {
		return nil, nil, err
	}
	lc.clearStreakOnConnect(nc)

	logger.InfoContext(
		ctx, "natsclient: connected",
		slog.String("url", redact.URL(nc.ConnectedUrl())),
		slog.String("server_id", nc.ConnectedServerId()),
	)

	return nc, lc.closedCh, nil
}

// dial makes the initial connection.
//
// A refused credential never enters nats.go's own backoff loop, where the
// reason would be buried: dial retries it here, with a WARN per refusal, and
// returns the fatal rejection error once the refusals have lasted the grace.
// Any other failure, notably an unreachable server, is returned at once.
func dial(ctx context.Context, cfg workerconfig.NATSConfig, opts []nats.Option, lc *lifecycle) (*nats.Conn, error) {
	backoff := exponentialBackoff(cfg.ReconnectWait)
	var since time.Time
	for attempt := 0; ; attempt++ {
		nc, err := nats.Connect(cfg.URL, opts...)
		if err == nil {
			return nc, nil
		}
		if !IsAuthRefusal(err) {
			return nil, dialError(ctx, cfg.URL, err)
		}
		now := time.Now()
		if since.IsZero() {
			since = now
		}
		remaining := lc.grace - now.Sub(since)
		if remaining <= 0 {
			return nil, lc.rejection(err)
		}
		lc.logRefusal(err, remaining)

		wait := time.NewTimer(min(backoff(attempt), remaining))
		select {
		case <-ctx.Done():
			wait.Stop()
			return nil, fmt.Errorf("natsclient: connect %q: %w: %w", redact.List(cfg.URL), err, ctx.Err())
		case <-wait.C:
		}
	}
}

// dialError wraps an initial dial failure that is not a credential refusal.
//
// The dial is not ctx-aware, so a cancel (a Windows service Stop) that lands
// during a failing dial surfaces as a plain dial error. ctx's error is
// wrapped too so callers can tell a stop-initiated exit from a failure: the
// service host would otherwise report exit code 1 and restart a service the
// operator just stopped.
func dialError(ctx context.Context, url string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("natsclient: connect %q: %w: %w", redact.List(url), err, ctxErr)
	}
	return fmt.Errorf("natsclient: connect %q: %w", redact.List(url), err)
}

// IsAuthRefusal reports whether err is the broker refusing this connection's
// credential: every auth error nats.go recognizes. With IgnoreAuthErrorAbort
// set, one left out here would be retried silently forever.
func IsAuthRefusal(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, nats.ErrAuthorization) ||
		errors.Is(err, nats.ErrAuthExpired) ||
		errors.Is(err, nats.ErrAuthRevoked) ||
		errors.Is(err, nats.ErrAccountAuthExpired) {
		return true
	}
	// The initial dial returns the server's -ERR text as it came, which
	// errors.Is matches to nats.ErrAuthorization alone ("nats: user
	// authentication expired" is not "nats: authentication expired"), so the
	// rest are recognized by the same prefixes nats.go itself checks.
	msg := strings.TrimPrefix(strings.ToLower(err.Error()), "nats: ")
	for _, prefix := range []string{
		nats.AUTHORIZATION_ERR,
		nats.AUTHENTICATION_EXPIRED_ERR,
		nats.AUTHENTICATION_REVOKED_ERR,
		nats.ACCOUNT_AUTHENTICATION_EXPIRED_ERR,
	} {
		if strings.HasPrefix(msg, prefix) {
			return true
		}
	}
	return false
}

// credentialRejectedError reports whether err is the broker refusing this
// connection's credential (see [IsAuthRefusal]). When it is, the second
// return is true and the first is the single crafted message every
// credential-rejection path uses — the initial dial, a refusal streak that
// outlasted the grace, and a closure observed via [nats.Conn.LastError] all
// call this, so they cannot drift apart.
//
// grace is how long the refusals were retried, and keyed whether the worker
// presented a key at all; each adds the cause only it makes possible.
func credentialRejectedError(err error, grace time.Duration, keyed bool) (error, bool) {
	if err == nil || !IsAuthRefusal(err) {
		return nil, false
	}
	msg := "natsclient: the broker rejected this worker's credential: it may have been revoked (re-enroll with a new join token)"
	if grace > 0 {
		msg += fmt.Sprintf(", or sqi-server or its store was unavailable for longer than %s", grace)
	}
	if keyed {
		msg += "; if the farm runs with broker authentication off, the broker refuses any key, so remove this worker's credential file (nats.credential_file)"
	}
	return fmt.Errorf("%s: %w", msg, err), true
}

// lifecycle owns a connection's lifecycle callbacks and the streak of
// credential refusals behind [authRejectGrace].
//
// A streak starts at the first refusal seen while the connection is down
// and ends when the connection returns to CONNECTED, or when a reconnect
// attempt fails for a reason other than a refusal (the broker is
// unreachable, so the refusals are no longer what is keeping the worker
// out). A refusal inside the grace logs a WARN; the first one past it logs
// the rejection error and closes the connection, which closes closedCh.
//
// The routine user-JWT expiry the broker imposes once per
// nats.auth.user_ttl also arrives as an auth error. The first expiry after a
// CONNECTED period is logged at INFO and is not a refusal; a further expiry
// before the connection is back is one, so a broker that sees every fresh
// JWT as already expired (clock skew) still ends in the rejection error.
type lifecycle struct {
	ctx    context.Context //nolint:containedctx // the callbacks nats.go invokes take no context
	logger *slog.Logger

	// grace is the refusal grace for a worker that presents a key
	// ([authRejectGrace] outside tests), and zero for one that does not: no
	// outage explains refusing an anonymous connection, so there is nothing
	// to wait out.
	grace time.Duration
	keyed bool

	// base is the reconnect backoff's first step, the floor for a reconnect
	// delay capped by the grace.
	base time.Duration

	// closedCh is closed by the ClosedHandler when the connection
	// permanently closes (MaxReconnects exhausted or nc.Close()).
	closedCh chan struct{}

	mu       sync.Mutex
	since    time.Time // start of the current streak; zero when there is none
	refusals int       // refusals in the current streak, excluding a routine expiry
	expired  bool      // an expiry was seen since the connection was last CONNECTED
	expiring bool      // a routine expiry ended the session; its disconnect is expected
	gaveUp   bool      // the streak outlasted the grace and the rejection was logged
}

func newLifecycle(ctx context.Context, logger *slog.Logger, keyed bool, base, grace time.Duration) *lifecycle {
	lc := &lifecycle{ctx: ctx, logger: logger, keyed: keyed, base: base, closedCh: make(chan struct{})}
	if keyed {
		lc.grace = grace
	}
	return lc
}

// rejection is [credentialRejectedError] for this connection.
func (lc *lifecycle) rejection(err error) error {
	wrapped, _ := credentialRejectedError(err, lc.grace, lc.keyed)
	return wrapped
}

func (lc *lifecycle) logRefusal(err error, remaining time.Duration) {
	msg := "natsclient: the broker refused this worker's credential; retrying — it may have been revoked, or sqi-server or its store may be unavailable"
	if lc.keyed {
		msg += ", or the farm runs with broker authentication off and refuses any key"
	}
	lc.logger.WarnContext(
		lc.ctx, msg,
		slog.Any("error", err),
		slog.Duration("gives_up_in", remaining.Round(time.Second)),
	)
}

// authVerdict is what [lifecycle.record] makes of an auth error.
type authVerdict int

const (
	authExpired authVerdict = iota // routine user-JWT expiry
	authRefused                    // a refusal inside the grace
	authGiveUp                     // a refusal past the grace
	authSpent                      // a refusal after giving up, already being closed
)

// record adds an auth error to the streak. connected is evaluated under the
// streak's lock: a refusal reported after the reconnect it belongs to has
// already succeeded must not start a streak that nothing will clear.
func (lc *lifecycle) record(connected func() bool, expiry bool) (authVerdict, time.Duration) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	if lc.gaveUp {
		return authSpent, 0
	}
	up := connected()
	now := time.Now()
	if !up && lc.since.IsZero() {
		lc.since = now
	}
	if expiry && lc.refusals == 0 && !lc.expired {
		lc.expired = true
		lc.expiring = true
		return authExpired, 0
	}
	if up {
		return authRefused, lc.grace
	}
	lc.refusals++
	age := now.Sub(lc.since)
	if age >= lc.grace {
		lc.gaveUp = true
		return authGiveUp, 0
	}
	return authRefused, lc.grace - age
}

// clear ends the streak: the connection is back.
func (lc *lifecycle) clear() {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	lc.endStreakLocked()
	lc.expired = false
	lc.expiring = false
}

// endStreakLocked drops the streak's start and its refusals. lc.mu is held.
func (lc *lifecycle) endStreakLocked() {
	lc.since = time.Time{}
	lc.refusals = 0
}

// onReconnectErr is the connection's ReconnectErrHandler. nats.go calls it
// when a reconnect attempt cannot even reach the broker; a refusal is
// reported through the ErrorHandler instead. An unreachable broker ends the
// streak, so the next refusal starts a fresh one rather than being judged
// against a start that predates the outage.
func (lc *lifecycle) onReconnectErr(_ *nats.Conn, err error) {
	if IsAuthRefusal(err) {
		return
	}
	lc.mu.Lock()
	defer lc.mu.Unlock()
	lc.endStreakLocked()
}

// clearStreakOnConnect ends the streak each time nc returns to CONNECTED,
// until nc closes. It observes the status rather than installing a
// reconnect handler, which the registration package owns.
func (lc *lifecycle) clearStreakOnConnect(nc *nats.Conn) {
	ch := nc.StatusChanged(nats.CONNECTED)
	go func() {
		defer nc.RemoveStatusListener(ch)
		for {
			select {
			case <-ch:
				lc.clear()
			case <-lc.closedCh:
				return
			}
		}
	}()
}

// capDelay bounds a reconnect delay by what is left of the grace, so a
// refused worker gives up close to the grace rather than up to one full
// backoff interval after it. The cap never goes below lc.base: a spent grace
// would otherwise make every delay zero, and if the broker then became
// unreachable nats.go would redial it in a tight loop.
func (lc *lifecycle) capDelay(next func(int) time.Duration) func(int) time.Duration {
	return func(attempts int) time.Duration {
		d := next(attempts)
		lc.mu.Lock()
		defer lc.mu.Unlock()
		if lc.refusals == 0 {
			return d
		}
		return min(d, max(lc.grace-time.Since(lc.since), lc.base))
	}
}

// onError is the connection's ErrorHandler.
func (lc *lifecycle) onError(nc *nats.Conn, _ *nats.Subscription, err error) {
	if !IsAuthRefusal(err) {
		lc.logger.ErrorContext(lc.ctx, "natsclient: async error", slog.Any("error", err))
		return
	}
	verdict, remaining := lc.record(nc.IsConnected, errors.Is(err, nats.ErrAuthExpired))
	switch verdict {
	case authExpired:
		lc.logger.InfoContext(lc.ctx, "natsclient: broker session expired; reconnecting to be authorized afresh", slog.Any("error", err))
	case authRefused:
		lc.logRefusal(err, remaining)
	case authGiveUp:
		lc.logger.ErrorContext(lc.ctx, "natsclient: giving up on the broker", slog.Any("error", lc.rejection(err)))
		nc.Close()
	case authSpent:
	}
}

// onDisconnect is the connection's DisconnectErrHandler.
func (lc *lifecycle) onDisconnect(nc *nats.Conn, err error) {
	lc.mu.Lock()
	if IsAuthRefusal(err) && lc.since.IsZero() && !nc.IsConnected() {
		lc.since = time.Now()
	}
	expected := lc.expiring
	lc.expiring = false
	lc.mu.Unlock()

	switch {
	case err == nil:
		lc.logger.InfoContext(lc.ctx, "natsclient: disconnected cleanly")
	case expected:
		lc.logger.InfoContext(lc.ctx, "natsclient: disconnected at broker session expiry", slog.Any("error", err))
	default:
		lc.logger.WarnContext(lc.ctx, "natsclient: disconnected", slog.Any("error", err))
	}
}

// onClosed is the connection's ClosedHandler.
//
// A closure the refusal streak caused has already logged the rejection.
// Otherwise nc.LastError, which nats.go documents as reliable inside the
// ClosedHandler, still names a refusal that ended the connection some other
// way (MaxReconnects exhausted while refused), rather than leaving it a
// generic closure.
func (lc *lifecycle) onClosed(nc *nats.Conn) {
	lc.mu.Lock()
	gaveUp := lc.gaveUp
	lc.mu.Unlock()

	if wrapped, ok := credentialRejectedError(nc.LastError(), lc.grace, lc.keyed); ok && !gaveUp {
		lc.logger.ErrorContext(lc.ctx, "natsclient: connection closed", slog.Any("error", wrapped))
	} else {
		lc.logger.InfoContext(lc.ctx, "natsclient: connection closed")
	}
	close(lc.closedCh)
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
// lc supplies the lifecycle callbacks, including the one that closes its
// closedCh when the connection permanently closes so callers can detect
// unexpected disconnects. seed and publicKey, when non-empty, add an nkey
// signing option so the connection authenticates as this worker's broker
// credential. workerID scopes the connection's reply inboxes to this
// worker's own prefix and is required.
func buildOptions(
	cfg workerconfig.NATSConfig,
	workerID string,
	seed []byte,
	publicKey string,
	lc *lifecycle,
) ([]nats.Option, error) {
	opts := []nats.Option{
		nats.MaxReconnects(cfg.MaxReconnectAttempts),

		// A refused credential is retried, not abandoned on the second
		// identical refusal as nats.go would by default: lc decides when a
		// refusal streak has lasted long enough to be final.
		nats.IgnoreAuthErrorAbort(),

		// ── Exponential backoff with jitter ──────────────────────
		//
		// CustomReconnectDelay replaces the fixed ReconnectWait with a
		// function that receives the number of attempts elapsed and returns
		// the delay before the next attempt.
		//
		// Formula: base * 2^attempts, capped at protocol.MaxReconnectBackoff,
		// then ±jitter, and never past the end of a refusal streak's grace.
		nats.CustomReconnectDelay(lc.capDelay(exponentialBackoff(cfg.ReconnectWait))),
		nats.ReconnectErrHandler(lc.onReconnectErr),

		// ── Lifecycle logging ──────────────────────────────────────────────
		nats.DisconnectErrHandler(lc.onDisconnect),
		// ReconnectHandler is intentionally omitted here. The worker's
		// registration package installs its own reconnect callback via
		// nc.SetReconnectHandler after the connection is established, which
		// logs the reconnect and re-publishes the registration message in one
		// step. Installing a handler here would be overwritten before any
		// reconnect can occur and would never fire.
		//
		// ClosedHandler fires when the connection transitions to the CLOSED
		// state: either MaxReconnects was exhausted or the connection was
		// explicitly closed, including by lc giving up on a refused
		// credential. Closing closedCh signals any goroutine that is
		// watching for unexpected permanent disconnects.
		//
		// A live credential revocation (internal/bus.Broker.Revoke
		// disconnects the client and the auth callout refuses its
		// reconnect) is seen here and by the ErrorHandler, not at the
		// initial dial — the connection was already established when the
		// credential stopped being valid.
		nats.ClosedHandler(lc.onClosed),
		nats.ErrorHandler(lc.onError),
	}

	// ── Per-worker reply inbox ───────────────────────────────────
	//
	// Without this the connection takes nats.go's process-global "_INBOX"
	// prefix, and the only permission that could then cover a lease reply
	// is "_INBOX.>" — which covers every OTHER client's reply inbox on the
	// same broker too, handing any enrolled worker the assignment batches
	// of work it never leased. See [brokerauth.InboxPrefix].
	//
	// Applied whether or not a credential is present, so the connect path
	// does not diverge between the auth-on and auth-off modes. An absent
	// worker ID, or one that is not a single subject token, would leave the
	// connection on the shared prefix or silently widen the subtree the
	// matching grant covers — so it is rejected rather than trusted:
	// LoadOrCreateWorkerID writes a UUID, but the file it writes is one an
	// operator can edit.
	if !brokerauth.ValidWorkerIDToken(workerID) {
		return nil, fmt.Errorf(
			"natsclient: worker id %q is not a valid NATS subject token — it must be non-empty and must not contain '.', whitespace, '*' or '>'",
			workerID,
		)
	}
	opts = append(opts, nats.CustomInboxPrefix(brokerauth.InboxPrefix(workerID)))

	// ── TLS ──────────────────────────────────────────────────────
	tlsOpts, err := TLSOptions(cfg)
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
	// broker does require authentication, connecting with no credential
	// fails the initial dial with nats.ErrAuthorization at once, and a
	// refused key fails it once the refusals have lasted authRejectGrace.
	if len(seed) > 0 {
		opts = append(opts, brokerauth.NkeyOption(publicKey, seed))
	}

	return opts, nil
}

// exponentialBackoff returns a CustomReconnectDelay function that doubles the
// base wait on each attempt, caps at protocol.MaxReconnectBackoff, and adds
// ±protocol.ReconnectJitterFraction randomness so simultaneous worker restarts
// spread their reconnect times.
//
// attempts is zero-indexed: attempt 0 is the first reconnect after disconnect.
func exponentialBackoff(base time.Duration) func(int) time.Duration {
	return func(attempts int) time.Duration {
		// 2^attempts growth, capped to avoid overflow on large attempt counts.
		exp := math.Min(float64(attempts), 10) // 2^10 = 1024× base is already ≥ protocol.MaxReconnectBackoff for any sane base
		delay := min(time.Duration(float64(base)*math.Pow(2, exp)), protocol.MaxReconnectBackoff)

		// Add ±ReconnectJitterFraction randomness. math/rand/v2 is sufficient
		// for backoff jitter: cryptographic randomness is not required here.
		jitter := time.Duration(float64(delay) * protocol.ReconnectJitterFraction * (rand.Float64()*2 - 1)) //nolint:gosec // jitter does not require cryptographic randomness
		delay += jitter
		if delay < 0 {
			delay = 0
		}
		return delay
	}
}

// TLSOptions returns the nats.Option(s) needed to configure TLS on the
// connection. It returns an empty slice when no TLS configuration is provided.
//
// TLS is activated when any of the following are set:
//   - cfg.TLSCertFile / cfg.TLSKeyFile  — mutual TLS client certificate
//   - cfg.TLSCAFile                     — custom CA for server cert verification
//   - cfg.InsecureSkipVerify            — disable server cert verification
func TLSOptions(cfg workerconfig.NATSConfig) ([]nats.Option, error) {
	// The three-valued nats.tls_enabled decides this; see NATSConfig.UseTLS.
	// Any mDNS signal has already been folded into cfg by the caller (see
	// discovery.ApplyTLS), so false here means "nothing, including mDNS, gave a
	// reason to use TLS".
	wantTLS := cfg.UseTLS(false)

	if !wantTLS {
		return nil, nil
	}

	tlsCfg, err := tlsutil.ClientConfig(cfg.TLSCAFile, cfg.TLSCertFile, cfg.TLSKeyFile)
	switch {
	case errors.Is(err, tlsutil.ErrIncompleteClientPair):
		return nil, errors.New("natsclient: tls_cert_file and tls_key_file must both be set")
	case err != nil:
		// The error names the file it could not load.
		return nil, fmt.Errorf("natsclient: tls: %w", err)
	}
	tlsCfg.InsecureSkipVerify = cfg.InsecureSkipVerify // explicitly user-requested

	return []nats.Option{nats.Secure(tlsCfg)}, nil
}
