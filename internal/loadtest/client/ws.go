// SPDX-License-Identifier: AGPL-3.0-or-later

package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"

	"github.com/uberware/sqi/internal/ws"
)

const (
	wsReadLimit  = 4 << 20
	wsInboxDepth = 4096
)

// WSConn is a connection to /api/v1/ws. A background goroutine reads every
// frame, so Subscribe and Next may be called from different goroutines.
type WSConn struct {
	conn *websocket.Conn

	wmu sync.Mutex // serializes writes
	seq atomic.Uint64

	mu   sync.Mutex
	acks map[uint64]chan ws.AckPayload

	inbox  chan ws.Envelope
	done   chan struct{} // closed when the read loop ends
	err    error         // why it ended; valid once done is closed
	cancel context.CancelFunc

	closeOnce sync.Once
	closeErr  error
}

// DialWS opens a WebSocket to the server at baseURL, authenticating with
// apiKey when it is non-empty.
func DialWS(ctx context.Context, baseURL, apiKey string) (*WSConn, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("ws: parse base URL: %w", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	u.Path += apiPrefix + "/ws"

	opts := &websocket.DialOptions{CompressionMode: websocket.CompressionDisabled}
	if apiKey != "" {
		opts.HTTPHeader = http.Header{"Authorization": {"Bearer " + apiKey}}
	}
	conn, resp, err := websocket.Dial(ctx, u.String(), opts) //nolint:bodyclose // coder/websocket owns the upgrade response body
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("ws: dial %s: HTTP %d: %w", u.Redacted(), resp.StatusCode, err)
		}
		return nil, fmt.Errorf("ws: dial %s: %w", u.Redacted(), err)
	}
	conn.SetReadLimit(wsReadLimit)

	readCtx, cancel := context.WithCancel(context.Background())
	w := &WSConn{
		conn:   conn,
		acks:   make(map[uint64]chan ws.AckPayload),
		inbox:  make(chan ws.Envelope, wsInboxDepth),
		done:   make(chan struct{}),
		cancel: cancel,
	}
	go w.readLoop(readCtx)
	return w, nil
}

func (w *WSConn) readLoop(ctx context.Context) {
	defer close(w.done)
	for {
		_, data, err := w.conn.Read(ctx)
		if err != nil {
			w.err = fmt.Errorf("ws: read: %w", err)
			return
		}
		var env ws.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			w.err = fmt.Errorf("ws: decode frame: %w", err)
			return
		}
		switch env.Type {
		case ws.TypeAck:
			w.deliverAck(env)
		case ws.TypePing:
			if err := w.write(ctx, ws.Envelope{Type: ws.TypePong}); err != nil {
				w.err = fmt.Errorf("ws: answer ping: %w", err)
				return
			}
		default:
			select {
			case w.inbox <- env:
			case <-ctx.Done():
				w.err = fmt.Errorf("ws: read: %w", ctx.Err())
				return
			}
		}
	}
}

func (w *WSConn) deliverAck(env ws.Envelope) {
	var p ws.AckPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return
	}
	w.mu.Lock()
	ch := w.acks[p.ClientSeq]
	w.mu.Unlock()
	if ch != nil {
		select {
		case ch <- p:
		default:
		}
	}
}

func (w *WSConn) write(ctx context.Context, env ws.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("ws: encode envelope: %w", err)
	}
	w.wmu.Lock()
	defer w.wmu.Unlock()
	return w.conn.Write(ctx, websocket.MessageText, data)
}

// Subscribe subscribes to subject and waits for the server's ack. A non-zero
// sinceSeq replays buffered pushes with a greater seq. A refusal (an invalid
// subject, or one the principal may not read) is returned as an error.
func (w *WSConn) Subscribe(ctx context.Context, subject string, sinceSeq uint64) error {
	seq := w.seq.Add(1)
	ch := make(chan ws.AckPayload, 1)
	w.mu.Lock()
	w.acks[seq] = ch
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.acks, seq)
		w.mu.Unlock()
	}()

	payload, err := json.Marshal(ws.SubscribePayload{SinceSeq: sinceSeq})
	if err != nil {
		return fmt.Errorf("ws: encode subscribe payload: %w", err)
	}
	if err := w.write(ctx, ws.Envelope{Type: ws.TypeSubscribe, Subject: subject, Payload: payload, Seq: seq}); err != nil {
		return fmt.Errorf("ws: subscribe %q: %w", subject, err)
	}
	select {
	case ack := <-ch:
		if ack.Error != "" {
			return fmt.Errorf("ws: subscribe %q refused: %s", subject, ack.Error)
		}
		return nil
	case <-w.done:
		return fmt.Errorf("ws: subscribe %q: %w", subject, w.err)
	case <-ctx.Done():
		return fmt.Errorf("ws: subscribe %q: %w", subject, ctx.Err())
	}
}

// Next returns the next envelope that is not an ack or a ping; pings are
// answered internally. Frames received before the connection ended are
// returned first, then the error that ended it.
func (w *WSConn) Next(ctx context.Context) (ws.Envelope, error) {
	select {
	case env := <-w.inbox:
		return env, nil
	default:
	}
	select {
	case env := <-w.inbox:
		return env, nil
	case <-w.done:
		select {
		case env := <-w.inbox:
			return env, nil
		default:
		}
		return ws.Envelope{}, w.err
	case <-ctx.Done():
		return ws.Envelope{}, ctx.Err()
	}
}

// Close closes the connection and waits for the reader to stop. It is
// idempotent.
func (w *WSConn) Close() error {
	w.closeOnce.Do(func() {
		err := w.conn.Close(websocket.StatusNormalClosure, "")
		w.cancel()
		<-w.done
		if err != nil && !errors.Is(err, net.ErrClosed) {
			w.closeErr = fmt.Errorf("ws: close: %w", err)
		}
	})
	return w.closeErr
}
