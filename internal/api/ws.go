// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// WebSocket upgrade handler.
//
// This file implements the GET /api/v1/ws endpoint and the full WebSocket
// connection lifecycle including subscriptions, hub fan-out, and connection
// health management.
//
// # Lifecycle
//
//  1. The client sends a standard WebSocket upgrade request.
//  2. h.authn.Authenticate(r) runs; a nil authn defaults to auth.Anonymous().
//  3. websocket.Accept completes the handshake (101 Switching Protocols).
//  4. readLoop starts and blocks for the lifetime of the connection:
// a. Registers the client with the hub and starts a send goroutine.
//       b. Drives the server-side ping ticker (keep-alive + idle detection).
//       c. Decodes inbound frames and dispatches to subscribe/unsubscribe/ping.
//  5. On disconnect or context cancellation readLoop runs cleanup defers:
//       (i)  close(pingDone)          — signals the ping goroutine to stop
//       (ii) hub.Deregister(clientID) — closes the send channel, unblocking the
//            send goroutine's range loop so it can exit
//       (iii) sendWg.Wait()           — waits for the send goroutine to drain and exit
//       (iv)  pingWg.Wait()           — waits for the ping goroutine to exit
//
// # Subscription dispatch
//
// TypeSubscribe: registers the connection with the hub for the requested
// subject.  The hub validates the subject, replays any ring-buffered events
// since the provided SinceSeq, and begins live fan-out.
//
// TypeUnsubscribe: removes the subscription from the hub for that subject.
//
// # Ping/pong and idle disconnect
//
// The server sends WebSocket ping frames every wsPingInterval.  The ping
// goroutine also checks whether any frame has been received from the client
// within wsIdleTimeout.  If not, the connection is closed with status 1001
// (Going Away).
//
// # Sequence numbers and resumable subscriptions
//
// Hub assigns a per-subject monotonically increasing sequence number (global
// across connections) to every TypePush envelope.  Clients track the Seq of
// the last received push and pass it as SinceSeq when reconnecting to receive
// any missed events from the ring buffer.
//
// # Concurrency
//
// coder/websocket does not allow concurrent raw Conn.Write calls.  wsConn.mu
// serializes all writes from the ping goroutine, the ack/error senders, and
// the hub send goroutine so no two goroutines write to the wire concurrently.
//
// # Goroutine lifecycle
//
// readLoop starts exactly two goroutines: the ping goroutine and (when a hub
// is wired) the send goroutine.  The cleanup defers are registered in a
// specific order so they execute in the correct sequence: close(pingDone) →
// hub.Deregister (closes sendCh) → sendWg.Wait → pingWg.Wait.  This ordering
// ensures neither goroutine is left running after readLoop returns.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/middleware"
	internalws "github.com/uberware/sqi/internal/ws"
)

// wsReadLimit is the maximum size of a single inbound WebSocket frame.
// Client subscription messages are small; 64 KiB is generous.
const wsReadLimit = 64 << 10 // 64 KiB

// wsPingInterval is how often the server sends a WebSocket ping to idle
// connections so intermediaries (load balancers, proxies) do not close them.
const wsPingInterval = 30 * time.Second

// wsWriteTimeout is the deadline applied to each individual write operation.
const wsWriteTimeout = 10 * time.Second

// wsIdleTimeout is the maximum time the server waits for any client message
// (subscribe, unsubscribe, ping) before closing the connection.  The check
// fires on the wsPingInterval tick, so effective granularity is wsPingInterval.
// Must be greater than wsPingInterval to avoid false positives.
const wsIdleTimeout = 5 * time.Minute

// wsOriginConfig controls WebSocket Origin enforcement (CSWSH hardening).
//
// When Enabled is false (auth off), origin checking is disabled entirely
// (InsecureSkipVerify: true) — byte-for-byte the pre-A1 behavior, since with
// no session cookie there is no ambient credential for a hostile page to
// ride.  When true (auth on), AllowedOrigins (converted to coder/websocket's
// host-pattern form by [originPatterns]) is passed as OriginPatterns; the
// request's own host is always implicitly authorized by the library, so an
// empty AllowedOrigins still permits same-origin connections (the embedded
// UI) while rejecting everything else.
type wsOriginConfig struct {
	Enabled        bool
	AllowedOrigins []string
}

// wsHandler handles GET /api/v1/ws WebSocket upgrade requests.
type wsHandler struct {
	logger *slog.Logger
	hub    *internalws.Hub // nil when no hub is wired (subscriptions are accepted but produce no pushes)
	authn  auth.Authenticator
	origin wsOriginConfig
}

func newWSHandler(logger *slog.Logger, hub *internalws.Hub, authn auth.Authenticator, origin wsOriginConfig) *wsHandler {
	if authn == nil {
		authn = auth.Anonymous()
	}
	return &wsHandler{logger: logger, hub: hub, authn: authn, origin: origin}
}

// ServeHTTP upgrades the connection and runs the per-connection message loop.
func (h *wsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqID := middleware.RequestIDFromContext(r.Context())
	log := h.logger.With(slog.String("request_id", reqID), slog.String("remote", r.RemoteAddr))

	principal, err := h.authn.Authenticate(r)
	if err != nil {
		log.WarnContext(r.Context(), "ws: authentication failed", slog.String("err", err.Error()))
		// Same RFC-7807 body the REST 401 returns (middleware.Auth): the
		// upgrade is rejected before websocket.Accept, so this is a plain HTTP
		// response and a client should not have to parse two error formats
		// depending on which surface refused it. The authenticator's reason
		// stays in the log, not the response.
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication required")
		return
	}

	acceptOpts := &websocket.AcceptOptions{
		// Disable per-message compression: the Go implementation allocates a
		// zlib.Writer per message without pooling; revisit when profiling warrants.
		CompressionMode: websocket.CompressionDisabled,
	}
	if h.origin.Enabled {
		// Auth on: a session cookie is in play, so this upgrade is an ambient
		// credential a hostile page could ride (CSWSH). OriginPatterns
		// restricts the handshake to the configured origins; the request's
		// own host is always implicitly allowed by the library, so the
		// same-origin embedded UI works even with an empty allow-list.
		acceptOpts.OriginPatterns = originPatterns(h.origin.AllowedOrigins)
	} else {
		// Auth off: no cookie, no ambient-credential surface to protect —
		// byte-for-byte the pre-A1 behavior (InsecureSkipVerify skips the
		// Origin header check; it does not affect TLS verification).
		acceptOpts.InsecureSkipVerify = true
	}
	conn, acceptErr := websocket.Accept(w, r, acceptOpts)
	if acceptErr != nil {
		log.WarnContext(r.Context(), "ws: upgrade failed", slog.String("err", acceptErr.Error()))
		return
	}

	conn.SetReadLimit(wsReadLimit)

	wc := &wsConn{
		id:     uuid.NewString(),
		conn:   conn,
		logger: log,
		hub:    h.hub,
	}

	ctx := auth.NewContext(r.Context(), principal)
	log.InfoContext(ctx, "ws: connection established", slog.String("client_id", wc.id))
	wc.readLoop(ctx)
	log.InfoContext(ctx, "ws: connection closed", slog.String("client_id", wc.id))
}

// wsConn wraps a [websocket.Conn] with serialized write access and hub
// registration for the per-connection WebSocket lifecycle.
//
// mu serializes all calls to conn.Write and conn.Ping.  Hub-assigned TypePush
// Seq values must be set before the envelope is placed in the send channel; mu
// ensures no two goroutines write to the wire concurrently.
type wsConn struct {
	id     string // stable UUID for this connection; used as the hub client ID
	conn   *websocket.Conn
	logger *slog.Logger
	hub    *internalws.Hub // may be nil when no hub is wired

	mu sync.Mutex // serializes all conn.Write and conn.Ping calls
}

// Send encodes env as a JSON text frame and writes it to the WebSocket
// connection.  The envelope is written as-is; callers set Seq before calling.
//
// Send is safe for concurrent use from multiple goroutines.
func (wc *wsConn) Send(ctx context.Context, env internalws.Envelope) error {
	writeCtx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()

	wc.mu.Lock()
	defer wc.mu.Unlock()

	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("ws: marshal envelope: %w", err)
	}
	return wc.conn.Write(writeCtx, websocket.MessageText, data)
}

// sendAck sends a TypeAck echoing clientSeq.
// errMsg is non-empty when the operation failed.
func (wc *wsConn) sendAck(ctx context.Context, clientSeq uint64, errMsg string) {
	payload, err := json.Marshal(internalws.AckPayload{
		ClientSeq: clientSeq,
		Error:     errMsg,
	})
	if err != nil {
		wc.logger.DebugContext(ctx, "ws: marshal ack payload", slog.Any("error", err))
		return
	}
	if err := wc.Send(ctx, internalws.Envelope{
		Type:    internalws.TypeAck,
		Payload: payload,
	}); err != nil {
		wc.logger.DebugContext(ctx, "ws: send ack", slog.Any("error", err))
	}
}

// sendError sends a TypeError to the client.
func (wc *wsConn) sendError(ctx context.Context, code, message string) {
	payload, err := json.Marshal(internalws.ErrorPayload{
		Code:    code,
		Message: message,
	})
	if err != nil {
		wc.logger.DebugContext(ctx, "ws: marshal error payload", slog.Any("error", err))
		return
	}
	if err := wc.Send(ctx, internalws.Envelope{
		Type:    internalws.TypeError,
		Payload: payload,
	}); err != nil {
		wc.logger.DebugContext(
			ctx, "ws: send error",
			slog.String("code", code),
			slog.Any("error", err),
		)
	}
}

// pingLoop runs until pingDone is closed or a ping fails.  It sends a
// WebSocket ping every wsPingInterval and closes the connection if no frame
// has been received from the client within wsIdleTimeout.
func (wc *wsConn) pingLoop(ctx context.Context, ticker *time.Ticker, lastActivity *atomic.Int64, pingDone <-chan struct{}) {
	for {
		select {
		case <-pingDone:
			return
		case <-ticker.C:
			since := time.Since(time.Unix(0, lastActivity.Load()))
			if since > wsIdleTimeout {
				wc.logger.InfoContext(
					ctx, "ws: idle timeout — closing connection",
					slog.Duration("idle", since),
				)
				_ = wc.conn.Close(websocket.StatusGoingAway, "idle timeout")
				return
			}
			pingCtx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
			wc.mu.Lock()
			err := wc.conn.Ping(pingCtx)
			wc.mu.Unlock()
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// sendLoop drains sendCh and writes each envelope to the WebSocket connection.
// It returns when sendCh is closed (i.e. after hub.Deregister).
func (wc *wsConn) sendLoop(ctx context.Context, sendCh <-chan internalws.Envelope) {
	for env := range sendCh {
		if err := wc.Send(ctx, env); err != nil {
			if !errors.Is(err, context.Canceled) && !isConnClosed(err) {
				wc.logger.DebugContext(ctx, "ws: hub send failed", slog.Any("error", err))
			}
			// Don't return on a single write error — drain until sendCh closes.
		}
	}
}

// readLoop runs until the connection closes or ctx is canceled.
//
// Goroutine lifecycle:
//
//	The ping goroutine (always) and send goroutine (when hub != nil) are
//	started here.  Cleanup defers are registered in reverse execution order
//	(LIFO) so the sequence is:
//	  1. close(pingDone)          — signals ping goroutine to stop
//	  2. hub.Deregister(id)       — closes sendCh; send goroutine exits its range loop
//	  3. sendWg.Wait()            — waits for send goroutine to exit
//	  4. pingWg.Wait()            — waits for ping goroutine to exit
//	  5. pingTicker.Stop()        — stops the ticker (first deferred = last executed)
func (wc *wsConn) readLoop(ctx context.Context) {
	pingTicker := time.NewTicker(wsPingInterval)
	defer pingTicker.Stop() // executes last (deferred first)

	// lastActivity records when the most recent frame arrived from the client,
	// used by the ping goroutine to enforce wsIdleTimeout.
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())

	var pingWg, sendWg sync.WaitGroup
	pingDone := make(chan struct{})

	// ── Hub send goroutine ──────────────────────────────────────────
	// Started before cleanup defers so the send goroutine is running before
	// hub.Deregister is registered as a cleanup step.
	if wc.hub != nil {
		sendCh := wc.hub.Register(wc.id)
		sendWg.Go(func() { wc.sendLoop(ctx, sendCh) })
	}

	// ── Cleanup defers — registered in reverse execution order ───────────────
	// Last deferred = first executed (LIFO). See file-header comment for the
	// rationale behind this ordering.
	defer pingWg.Wait() // 4th: wait for ping goroutine
	defer sendWg.Wait() // 3rd: wait for send goroutine
	if wc.hub != nil {
		defer wc.hub.Deregister(wc.id) // 2nd: close sendCh → unblock send goroutine
	}
	defer close(pingDone) // 1st: signal ping goroutine to stop

	// ── Ping goroutine ────────────────────────────────────────────────────────
	pingWg.Go(func() { wc.pingLoop(ctx, pingTicker, &lastActivity, pingDone) })

	// ── Read loop ─────────────────────────────────────────────────────────────
	for {
		msgType, data, err := wc.conn.Read(ctx)
		if err != nil {
			wc.logReadError(ctx, err)
			return
		}

		// Record activity for idle-disconnect tracking.
		lastActivity.Store(time.Now().UnixNano())

		if msgType != websocket.MessageText {
			// RFC 6455 §7: a protocol error should close the connection.
			_ = wc.conn.Close(websocket.StatusUnsupportedData, "only text frames are accepted")
			return
		}

		var env internalws.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			wc.sendError(ctx, "invalid_envelope", "message is not a valid JSON envelope")
			continue
		}

		wc.dispatch(ctx, env)
	}
}

// logReadError logs a WebSocket read error at the appropriate level.
func (wc *wsConn) logReadError(ctx context.Context, err error) {
	var closeErr websocket.CloseError
	if errors.As(err, &closeErr) {
		wc.logger.DebugContext(
			ctx, "ws: client closed connection",
			slog.Int("code", int(closeErr.Code)),
			slog.String("reason", closeErr.Reason),
		)
		return
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		wc.logger.WarnContext(ctx, "ws: read error", slog.Any("error", err))
	}
}

// dispatch routes a decoded client Envelope to the appropriate handler.
func (wc *wsConn) dispatch(ctx context.Context, env internalws.Envelope) {
	switch env.Type {
	case internalws.TypePing:
		if err := wc.Send(ctx, internalws.Envelope{Type: internalws.TypePong}); err != nil {
			wc.logger.DebugContext(ctx, "ws: send pong", slog.Any("error", err))
		}

	case internalws.TypeSubscribe:
		wc.handleSubscribe(ctx, env)

	case internalws.TypeUnsubscribe:
		wc.handleUnsubscribe(ctx, env)

	default:
		wc.sendError(ctx, "unknown_type",
			fmt.Sprintf("unknown message type %q", env.Type))
	}
}

// handleSubscribe processes a TypeSubscribe client message.
//
// Validates the subject and sends an ack, then registers the subscription with
// the hub (which may immediately replay buffered events into the send channel).
// The ack is sent first so clients always receive it before any replayed pushes.
func (wc *wsConn) handleSubscribe(ctx context.Context, env internalws.Envelope) {
	if env.Subject == "" {
		wc.sendAck(ctx, env.Seq, "subject is required")
		return
	}

	var payload internalws.SubscribePayload
	if len(env.Payload) > 0 {
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			wc.sendAck(ctx, env.Seq, "invalid subscribe payload")
			return
		}
	}

	if wc.hub == nil {
		// No hub wired — accept the subscription silently without delivering pushes.
		wc.sendAck(ctx, env.Seq, "")
		return
	}

	// First call: validate subject + register subscription with no replay.
	// Pass MaxUint64 so ring.since(MaxUint64) returns nothing — the ack must
	// arrive before any replayed pushes (which are queued by the second call).
	const noReplaySeq = ^uint64(0)
	if err := wc.hub.Subscribe(wc.id, env.Subject, noReplaySeq); err != nil {
		wc.sendAck(ctx, env.Seq, err.Error())
		return
	}

	wc.sendAck(ctx, env.Seq, "")

	// Second call: trigger replay after the ack has been written.  Idempotent
	// for subscription registration; only enqueues buffered ring entries into
	// the send channel.  since_seq==0 replays all buffered messages; >0 replays
	// from that cursor.  Subject validity confirmed above; errors are unexpected.
	if err := wc.hub.Subscribe(wc.id, env.Subject, payload.SinceSeq); err != nil {
		wc.logger.WarnContext(ctx, "ws: replay re-subscribe failed", slog.Any("error", err))
	}

	wc.logger.DebugContext(
		ctx, "ws: subscribed",
		slog.String("subject", env.Subject),
		slog.Uint64("since_seq", payload.SinceSeq),
	)
}

// handleUnsubscribe processes a TypeUnsubscribe client message.
func (wc *wsConn) handleUnsubscribe(ctx context.Context, env internalws.Envelope) {
	if env.Subject == "" {
		wc.sendAck(ctx, env.Seq, "subject is required")
		return
	}

	if wc.hub != nil {
		wc.hub.Unsubscribe(wc.id, env.Subject)
	}

	wc.logger.DebugContext(ctx, "ws: unsubscribed", slog.String("subject", env.Subject))
	wc.sendAck(ctx, env.Seq, "")
}

// originPatterns converts configured origins (e.g. "https://farm.example",
// full URLs as stored in cfg.CORSOrigins) into the host[:port] pattern form
// coder/websocket's OriginPatterns expects. Entries that fail to parse as a
// URL with a host, plus the empty string and the literal wildcard "*", are
// passed through only when non-empty and not "*" — "*" is deliberately never
// forwarded (see the wildcard-drop discussion on cfg.AuthEnabled in
// router.go); use InsecureSkipVerify to intentionally allow every origin.
func originPatterns(origins []string) []string {
	pats := make([]string, 0, len(origins))
	for _, o := range origins {
		if o == "" || o == "*" {
			continue
		}
		if u, err := url.Parse(o); err == nil && u.Host != "" {
			pats = append(pats, u.Host)
		} else {
			pats = append(pats, o)
		}
	}
	return pats
}

// isConnClosed reports whether err indicates a closed WebSocket connection,
// to suppress noisy "write on closed pipe" log entries.
func isConnClosed(err error) bool {
	if err == nil {
		return false
	}
	var closeErr websocket.CloseError
	return errors.As(err, &closeErr)
}
