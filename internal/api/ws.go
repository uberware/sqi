// SPDX-License-Identifier: AGPL-3.0-only

package api

// WebSocket upgrade handler — task 87.
//
// This file implements the GET /api/v1/ws endpoint that upgrades an HTTP/1.1
// connection to a WebSocket connection using github.com/coder/websocket.
//
// # Lifecycle
//
//  1. The client sends a standard WebSocket upgrade request to GET /api/v1/ws.
//  2. The server authenticates the connection (Phase 1: open — no token required
//     until auth lands in Phase 3; the hook is present but always passes).
//  3. The server calls websocket.Accept to complete the HTTP → WebSocket
//     handshake and returns 101 Switching Protocols.
//  4. readLoop starts, owns the connection for reading, and drives the ping
//     ticker until the connection closes or ctx is canceled.
//  5. Callers send push messages via [wsConn.Send] (used by the fanout layer in
//     tasks 89–91) from any goroutine.
//  6. On disconnect readLoop waits for the ping goroutine to exit (via pingWg)
//     then returns cleanly — no goroutine leaks.
//
// # Authentication stub
//
// Phase 1 has no authentication subsystem (§9 of sqi.md defers auth to a later
// phase). authenticateWS is a named function so it can be replaced with a real
// implementation — token validation, mTLS, or API-key lookup — without touching
// the upgrade path.
//
// # Concurrency
//
// coder/websocket does not allow concurrent raw Conn.Write calls. wsConn.Send
// acquires mu before writing, and the seq counter is incremented inside that
// same critical section to guarantee that the sequence number assigned to a
// frame matches the order in which that frame reaches the wire.  Using mu for
// both seq increment and Write prevents the ordering inversion that would occur
// if seq.Add were called atomically outside the lock (goroutine A increments to
// 3, goroutine B increments to 4, B acquires the lock first → client sees 4
// then 3).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/uberware/sqi/internal/middleware"
	internalws "github.com/uberware/sqi/internal/ws"
)

// wsReadLimit is the maximum size of a single inbound WebSocket frame in bytes.
// Client subscription messages are small; 64 KiB is generous.
const wsReadLimit = 64 << 10 // 64 KiB

// wsPingInterval is how often the server sends a WebSocket ping to idle
// connections so intermediaries (load balancers, proxies) do not close them.
// TODO: make configurable via Config when profiling warrants it.
const wsPingInterval = 30 * time.Second

// wsWriteTimeout is the deadline applied to each individual write operation.
// The connection is closed if a write takes longer than this.
// TODO: make configurable via Config when profiling warrants it.
const wsWriteTimeout = 10 * time.Second

// wsHandler handles GET /api/v1/ws WebSocket upgrade requests.
type wsHandler struct {
	logger *slog.Logger
}

func newWSHandler(logger *slog.Logger) *wsHandler {
	return &wsHandler{logger: logger}
}

// ServeHTTP implements http.Handler. It upgrades the connection and runs the
// per-connection message loop.
//
// Note: r.Context() is used for the read loop. net/http cancels the request
// context when ServeHTTP returns — but ServeHTTP blocks on readLoop, so
// cancellation only arrives from the server's base context (shutdown). A future
// http.TimeoutHandler wrapping this handler would cancel the WebSocket
// connection prematurely; do not wrap this handler with a timeout middleware.
func (h *wsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqID := middleware.RequestIDFromContext(r.Context())
	log := h.logger.With(slog.String("request_id", reqID), slog.String("remote", r.RemoteAddr))

	// Phase 1 authentication hook — always passes.  Replace with real token /
	// session validation in Phase 3.
	if err := authenticateWS(r); err != nil {
		log.WarnContext(r.Context(), "ws: authentication failed", slog.String("err", err.Error()))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// InsecureSkipVerify skips the Origin header check (not TLS verification —
		// the field name in coder/websocket is misleading). Acceptable in Phase 1
		// because there is no session or CSRF surface to protect.
		// TODO(Phase 3): replace with OriginPatterns: []string{cfg.AllowedOrigin}
		// once authentication is introduced. Track as follow-up to task 87.
		InsecureSkipVerify: true, // origin check deferred to Phase 3 — see TODO above

		// Disable per-message compression (permessage-deflate). Enabling it
		// requires care because the Go implementation allocates a zlib.Writer
		// per message without pooling; revisit when profiling shows it matters.
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		// websocket.Accept already wrote the HTTP error response.
		log.WarnContext(r.Context(), "ws: upgrade failed", slog.String("err", err.Error()))
		return
	}

	conn.SetReadLimit(wsReadLimit)

	wc := &wsConn{
		conn:   conn,
		logger: log,
	}

	log.InfoContext(r.Context(), "ws: connection established")

	wc.readLoop(r.Context())

	log.InfoContext(r.Context(), "ws: connection closed")
}

// authenticateWS validates the WebSocket upgrade request.
//
// Phase 1 — always returns nil (open access). Phase 3 will inspect
// r.Header.Get("Authorization") or a session cookie and return a non-nil error
// for invalid credentials.
func authenticateWS(_ *http.Request) error {
	return nil
}

// wsConn wraps a [websocket.Conn] with serialized write access and a
// per-connection sequence counter for server push messages.
//
// mu guards both conn.Write and seq so that the sequence number assigned to a
// push frame is always the same value that gets written to the wire first — see
// the Concurrency note in the file header.
//
// The readLoop method drives inbound message processing; Send is called by the
// fanout layer (tasks 89–91) from arbitrary goroutines.
type wsConn struct {
	conn   *websocket.Conn
	logger *slog.Logger

	mu  sync.Mutex // guards conn.Write and seq (see Concurrency note)
	seq uint64     // per-connection push counter; only accessed under mu
}

// Send encodes env as a JSON text frame and writes it to the WebSocket
// connection. If env.Type is [internalws.TypePush], Send assigns the next
// per-connection sequence number to env.Seq before encoding.
//
// Send is safe for concurrent use from multiple goroutines.
func (wc *wsConn) Send(ctx context.Context, env internalws.Envelope) error {
	writeCtx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()

	wc.mu.Lock()
	defer wc.mu.Unlock()

	// Increment seq inside the lock so that the counter value assigned to the
	// frame is the same value written to the wire under this lock acquisition —
	// preventing seq ordering inversions across concurrent callers.
	if env.Type == internalws.TypePush {
		wc.seq++
		env.Seq = wc.seq
	}

	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("ws: marshal envelope: %w", err)
	}

	return wc.conn.Write(writeCtx, websocket.MessageText, data)
}

// sendAck sends a [internalws.TypeAck] to the client, echoing clientSeq.
// If errMsg is non-empty the ack indicates a failed operation.
func (wc *wsConn) sendAck(ctx context.Context, clientSeq uint64, errMsg string) {
	payload, err := json.Marshal(internalws.AckPayload{
		ClientSeq: clientSeq,
		Error:     errMsg,
	})
	if err != nil {
		wc.logger.DebugContext(ctx, "ws: failed to marshal ack payload", slog.String("err", err.Error()))
		return
	}
	if err := wc.Send(ctx, internalws.Envelope{
		Type:    internalws.TypeAck,
		Payload: payload,
	}); err != nil {
		wc.logger.DebugContext(ctx, "ws: failed to send ack", slog.String("err", err.Error()))
	}
}

// sendError sends a [internalws.TypeError] to the client.
func (wc *wsConn) sendError(ctx context.Context, code, message string) {
	payload, err := json.Marshal(internalws.ErrorPayload{
		Code:    code,
		Message: message,
	})
	if err != nil {
		wc.logger.DebugContext(ctx, "ws: failed to marshal error payload", slog.String("err", err.Error()))
		return
	}
	if err := wc.Send(ctx, internalws.Envelope{
		Type:    internalws.TypeError,
		Payload: payload,
	}); err != nil {
		wc.logger.DebugContext(
			ctx, "ws: failed to send error",
			slog.String("code", code),
			slog.String("err", err.Error()),
		)
	}
}

// readLoop runs until the connection closes or ctx is canceled. It reads
// inbound [internalws.Envelope] messages from the WebSocket and dispatches them
// to the appropriate handler.
//
// readLoop also drives the server-side ping ticker so idle connections stay
// alive through proxies and load balancers.
//
// This method blocks and owns the connection for reading; only one goroutine
// should call it. It waits for the internal ping goroutine to exit before
// returning, so no goroutines are leaked after readLoop returns.
func (wc *wsConn) readLoop(ctx context.Context) {
	pingTicker := time.NewTicker(wsPingInterval)
	defer pingTicker.Stop()

	// pingDone signals the ping goroutine to stop; pingWg lets readLoop wait
	// for it to finish before returning so no goroutine outlives the connection.
	// Defers execute LIFO: pingWg.Wait() is deferred first so it runs last,
	// after close(pingDone) has already signaled the goroutine to exit.
	var pingWg sync.WaitGroup
	pingWg.Add(1)
	defer pingWg.Wait()

	pingDone := make(chan struct{})
	defer close(pingDone)

	go func() {
		defer pingWg.Done()
		for {
			select {
			case <-pingDone:
				return
			case <-pingTicker.C:
				writeCtx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
				wc.mu.Lock()
				err := wc.conn.Ping(writeCtx)
				wc.mu.Unlock()
				cancel()
				if err != nil {
					// Connection is gone; the main read loop will discover the
					// same error on the next Read and return.
					return
				}
			}
		}
	}()

	for {
		msgType, data, err := wc.conn.Read(ctx)
		if err != nil {
			var closeErr websocket.CloseError
			if errors.As(err, &closeErr) {
				wc.logger.DebugContext(
					ctx, "ws: client closed connection",
					slog.Int("code", int(closeErr.Code)),
					slog.String("reason", closeErr.Reason),
				)
			} else if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				wc.logger.WarnContext(ctx, "ws: read error", slog.String("err", err.Error()))
			}
			return
		}

		if msgType != websocket.MessageText {
			// RFC 6455 §7: a protocol error should close the connection with
			// status 1003 (unsupported data). Continuing to read after a binary
			// frame would let a misbehaving client keep the loop alive forever.
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

// dispatch routes a decoded client [internalws.Envelope] to the appropriate
// handler. Subscription and unsubscription logic will be added in tasks 89–91.
func (wc *wsConn) dispatch(ctx context.Context, env internalws.Envelope) {
	switch env.Type {
	case internalws.TypePing:
		if err := wc.Send(ctx, internalws.Envelope{Type: internalws.TypePong}); err != nil {
			wc.logger.DebugContext(ctx, "ws: failed to send pong", slog.String("err", err.Error()))
		}

	case internalws.TypeSubscribe:
		// TODO(task 89): implement subscription registration.
		wc.sendAck(ctx, env.Seq, "")

	case internalws.TypeUnsubscribe:
		// TODO(task 89): implement subscription removal.
		wc.sendAck(ctx, env.Seq, "")

	default:
		wc.sendError(ctx, "unknown_type",
			fmt.Sprintf("unknown message type %q", env.Type))
	}
}
