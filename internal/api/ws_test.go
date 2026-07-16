// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Unit tests for the WebSocket upgrade handler.
//
// Each test spins up an httptest.Server backed by wsHandler directly (no chi
// router needed) and dials it with the coder/websocket client.  This keeps the
// tests fast and independent of the rest of the router.
//
// The fake bus is the real [ws.Hub] wired to the handler; the hub itself is
// fully tested in internal/ws/hub_test.go.  Here we only test the HTTP
// upgrade, subscription flow, fan-out delivery, and disconnect semantics as
// seen through the wire protocol.
//
// Coverage goals:
//
//   - Successful WebSocket upgrade.
//   - Client-initiated ping → server pong.
//   - Subscribe with a valid subject → TypeAck with no error.
//   - Subscribe with a missing subject → TypeAck with error.
//   - Subscribe with an invalid subject (hub attached) → TypeAck with error.
//   - Unsubscribe after subscribe → TypeAck with no error.
//   - Unknown message type → TypeError.
//   - Malformed JSON → TypeError (invalid_envelope).
//   - Broadcast: hub.NotifyJob delivers TypePush to a subscribed client.
//   - No push delivered after unsubscribe; connection stays alive (verified via ping).
//   - Disconnect: after client closes, subsequent hub notifications do not block.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/uberware/sqi/internal/auth"
	internalws "github.com/uberware/sqi/internal/ws"
)

// ── test server helpers ───────────────────────────────────────────────────────

// newWSTestServer starts an httptest.Server serving wsHandler.
// hub may be nil when the test does not need fan-out.
func newWSTestServer(t *testing.T, hub *internalws.Hub) *httptest.Server {
	t.Helper()
	h := newWSHandler(newTestLogger(), hub, auth.Anonymous())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// stubWSAuthenticator is a test [auth.Authenticator] that always returns err.
type stubWSAuthenticator struct{ err error }

func (s stubWSAuthenticator) Authenticate(*http.Request) (auth.Principal, error) {
	return auth.Principal{}, s.err
}

// wsTestURL converts the http://… address of srv to ws://….
func wsTestURL(srv *httptest.Server) string {
	return "ws" + srv.URL[len("http"):]
}

// dialTestWS dials srv and registers a CloseNow cleanup.
func dialTestWS(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, wsTestURL(srv), nil)
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	// The HTTP response is consumed by the WebSocket handshake; close its body
	// to satisfy the bodyclose linter.
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	t.Cleanup(func() {
		if err := conn.CloseNow(); err != nil {
			t.Logf("ws cleanup: CloseNow: %v", err) // best-effort teardown
		}
	})
	return conn
}

// wsWrite encodes env as a JSON text frame.
func wsWrite(t *testing.T, ctx context.Context, conn *websocket.Conn, env internalws.Envelope) {
	t.Helper()
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("conn.Write: %v", err)
	}
}

// wsRead reads one JSON text frame and decodes it into an Envelope.
func wsRead(t *testing.T, ctx context.Context, conn *websocket.Conn) internalws.Envelope {
	t.Helper()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("conn.Read: %v", err)
	}
	var env internalws.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	return env
}

// mustAck reads one frame, asserts it is a TypeAck for the given clientSeq,
// and returns the AckPayload.
func mustAck(t *testing.T, ctx context.Context, conn *websocket.Conn, clientSeq uint64) internalws.AckPayload {
	t.Helper()
	env := wsRead(t, ctx, conn)
	if env.Type != internalws.TypeAck {
		t.Fatalf("expected TypeAck, got %q", env.Type)
	}
	var ack internalws.AckPayload
	if err := json.Unmarshal(env.Payload, &ack); err != nil {
		t.Fatalf("unmarshal AckPayload: %v", err)
	}
	if ack.ClientSeq != clientSeq {
		t.Fatalf("expected ClientSeq=%d, got %d", clientSeq, ack.ClientSeq)
	}
	return ack
}

// ── tests ─────────────────────────────────────────────────────────────────────

func TestWS_FailingAuthenticator_Returns401BeforeUpgrade(t *testing.T) {
	h := newWSHandler(newTestLogger(), nil, stubWSAuthenticator{err: errors.New("bad token")})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL) //nolint:noctx // simple synchronous test request
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestWSHandler_Upgrade(t *testing.T) {
	srv := newWSTestServer(t, nil)
	// Successful dial confirms the 101 Switching Protocols handshake.
	// dialTestWS registers t.Cleanup to close the connection.
	_ = dialTestWS(t, srv)
}

func TestWSHandler_Ping_ReturnsPong(t *testing.T) {
	srv := newWSTestServer(t, nil)
	conn := dialTestWS(t, srv)
	ctx := context.Background()

	wsWrite(t, ctx, conn, internalws.Envelope{Type: internalws.TypePing})

	env := wsRead(t, ctx, conn)
	if env.Type != internalws.TypePong {
		t.Fatalf("expected TypePong, got %q", env.Type)
	}
}

func TestWSHandler_Subscribe_NoHub_ReturnsAck(t *testing.T) {
	// With no hub, any valid subject is accepted silently.
	srv := newWSTestServer(t, nil)
	conn := dialTestWS(t, srv)
	ctx := context.Background()

	wsWrite(t, ctx, conn, internalws.Envelope{
		Type:    internalws.TypeSubscribe,
		Subject: internalws.SubjectJobs,
		Seq:     1,
	})

	ack := mustAck(t, ctx, conn, 1)
	if ack.Error != "" {
		t.Fatalf("unexpected ack error: %q", ack.Error)
	}
}

func TestWSHandler_Subscribe_WithHub_ValidSubject(t *testing.T) {
	hub := internalws.NewHub(newTestLogger())
	srv := newWSTestServer(t, hub)
	conn := dialTestWS(t, srv)
	ctx := context.Background()

	wsWrite(t, ctx, conn, internalws.Envelope{
		Type:    internalws.TypeSubscribe,
		Subject: internalws.SubjectWorkers,
		Seq:     42,
	})

	ack := mustAck(t, ctx, conn, 42)
	if ack.Error != "" {
		t.Fatalf("unexpected ack error for valid subject: %q", ack.Error)
	}
}

func TestWSHandler_Subscribe_MissingSubject_ReturnsError(t *testing.T) {
	srv := newWSTestServer(t, nil)
	conn := dialTestWS(t, srv)
	ctx := context.Background()

	wsWrite(t, ctx, conn, internalws.Envelope{
		Type: internalws.TypeSubscribe,
		// Subject intentionally omitted.
		Seq: 3,
	})

	ack := mustAck(t, ctx, conn, 3)
	if ack.Error == "" {
		t.Fatal("expected non-empty Error for subscribe with no subject")
	}
}

func TestWSHandler_Subscribe_InvalidSubject_ReturnsError(t *testing.T) {
	hub := internalws.NewHub(newTestLogger())
	srv := newWSTestServer(t, hub)
	conn := dialTestWS(t, srv)
	ctx := context.Background()

	wsWrite(t, ctx, conn, internalws.Envelope{
		Type:    internalws.TypeSubscribe,
		Subject: "not/a/valid/subject",
		Seq:     7,
	})

	ack := mustAck(t, ctx, conn, 7)
	if ack.Error == "" {
		t.Fatal("expected non-empty Error for unrecognized subject")
	}
}

func TestWSHandler_Unsubscribe_ReturnsAck(t *testing.T) {
	hub := internalws.NewHub(newTestLogger())
	srv := newWSTestServer(t, hub)
	conn := dialTestWS(t, srv)
	ctx := context.Background()

	// Subscribe first.
	wsWrite(t, ctx, conn, internalws.Envelope{
		Type:    internalws.TypeSubscribe,
		Subject: internalws.SubjectJobs,
		Seq:     1,
	})
	mustAck(t, ctx, conn, 1) // consume subscribe ack

	// Then unsubscribe.
	wsWrite(t, ctx, conn, internalws.Envelope{
		Type:    internalws.TypeUnsubscribe,
		Subject: internalws.SubjectJobs,
		Seq:     2,
	})
	ack := mustAck(t, ctx, conn, 2)
	if ack.Error != "" {
		t.Fatalf("unexpected unsubscribe error: %q", ack.Error)
	}
}

func TestWSHandler_Unsubscribe_MissingSubject_ReturnsError(t *testing.T) {
	srv := newWSTestServer(t, nil)
	conn := dialTestWS(t, srv)
	ctx := context.Background()

	wsWrite(t, ctx, conn, internalws.Envelope{
		Type: internalws.TypeUnsubscribe,
		Seq:  5,
	})

	ack := mustAck(t, ctx, conn, 5)
	if ack.Error == "" {
		t.Fatal("expected non-empty Error for unsubscribe with no subject")
	}
}

func TestWSHandler_UnknownMessageType_ReturnsTypeError(t *testing.T) {
	srv := newWSTestServer(t, nil)
	conn := dialTestWS(t, srv)
	ctx := context.Background()

	wsWrite(t, ctx, conn, internalws.Envelope{Type: "not_real"})

	env := wsRead(t, ctx, conn)
	if env.Type != internalws.TypeError {
		t.Fatalf("expected TypeError, got %q", env.Type)
	}
	var ep internalws.ErrorPayload
	if err := json.Unmarshal(env.Payload, &ep); err != nil {
		t.Fatalf("unmarshal ErrorPayload: %v", err)
	}
	if ep.Code != "unknown_type" {
		t.Fatalf("expected code %q, got %q", "unknown_type", ep.Code)
	}
}

func TestWSHandler_InvalidJSON_ReturnsTypeError(t *testing.T) {
	srv := newWSTestServer(t, nil)
	conn := dialTestWS(t, srv)
	ctx := context.Background()

	if err := conn.Write(ctx, websocket.MessageText, []byte("{bad json")); err != nil {
		t.Fatalf("conn.Write: %v", err)
	}

	env := wsRead(t, ctx, conn)
	if env.Type != internalws.TypeError {
		t.Fatalf("expected TypeError for bad JSON, got %q", env.Type)
	}
	var ep internalws.ErrorPayload
	if err := json.Unmarshal(env.Payload, &ep); err != nil {
		t.Fatalf("unmarshal ErrorPayload: %v", err)
	}
	if ep.Code != "invalid_envelope" {
		t.Fatalf("expected code %q, got %q", "invalid_envelope", ep.Code)
	}
}

func TestWSHandler_Broadcast_DeliversPush(t *testing.T) {
	hub := internalws.NewHub(newTestLogger())
	srv := newWSTestServer(t, hub)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn := dialTestWS(t, srv)

	// Subscribe to jobs.
	wsWrite(t, ctx, conn, internalws.Envelope{
		Type:    internalws.TypeSubscribe,
		Subject: internalws.SubjectJobs,
		Seq:     1,
	})
	mustAck(t, ctx, conn, 1)

	// Trigger a notification via the hub.
	hub.NotifyJob(internalws.JobEvent{JobID: "broadcast-job", Status: "running"})

	// The next frame from the server must be a TypePush for SubjectJobs.
	env := wsRead(t, ctx, conn)
	if env.Type != internalws.TypePush {
		t.Fatalf("expected TypePush, got %q", env.Type)
	}
	if env.Subject != internalws.SubjectJobs {
		t.Fatalf("expected subject %q, got %q", internalws.SubjectJobs, env.Subject)
	}
	if env.Seq == 0 {
		t.Fatal("push envelope must have a non-zero Seq")
	}
}

func TestWSHandler_Broadcast_TaskPushDelivered(t *testing.T) {
	hub := internalws.NewHub(newTestLogger())
	srv := newWSTestServer(t, hub)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn := dialTestWS(t, srv)
	const jobID = "job-task-test"
	taskSubject := fmt.Sprintf(internalws.SubjectJobTasksFmt, jobID)

	wsWrite(t, ctx, conn, internalws.Envelope{
		Type:    internalws.TypeSubscribe,
		Subject: taskSubject,
		Seq:     1,
	})
	mustAck(t, ctx, conn, 1)

	hub.NotifyTask(internalws.TaskEvent{JobID: jobID, TaskID: "t1", Status: "succeeded"})

	// There will be a push on the task subject (NotifyTask fans to both jobs and tasks;
	// this client is only subscribed to the per-job tasks subject).
	env := wsRead(t, ctx, conn)
	if env.Type != internalws.TypePush {
		t.Fatalf("expected TypePush, got %q", env.Type)
	}
	if env.Subject != taskSubject {
		t.Fatalf("expected subject %q, got %q", taskSubject, env.Subject)
	}
}

func TestWSHandler_NoPushAfterUnsubscribe(t *testing.T) {
	hub := internalws.NewHub(newTestLogger())
	srv := newWSTestServer(t, hub)
	ctx := context.Background()
	conn := dialTestWS(t, srv)

	// Subscribe then immediately unsubscribe.
	wsWrite(t, ctx, conn, internalws.Envelope{Type: internalws.TypeSubscribe, Subject: internalws.SubjectJobs, Seq: 1})
	mustAck(t, ctx, conn, 1)
	wsWrite(t, ctx, conn, internalws.Envelope{Type: internalws.TypeUnsubscribe, Subject: internalws.SubjectJobs, Seq: 2})
	mustAck(t, ctx, conn, 2)

	hub.NotifyJob(internalws.JobEvent{JobID: "j1", Status: "running"})

	// Confirm the connection is still live via ping/pong — no push should arrive.
	wsWrite(t, ctx, conn, internalws.Envelope{Type: internalws.TypePing})
	env := wsRead(t, ctx, conn)
	if env.Type != internalws.TypePong {
		t.Fatalf("expected TypePong (connection alive, no push), got %q", env.Type)
	}
}

func TestWSHandler_Disconnect_HubNotifyDoesNotBlock(t *testing.T) {
	hub := internalws.NewHub(newTestLogger())
	srv := newWSTestServer(t, hub)
	ctx := context.Background()
	conn := dialTestWS(t, srv)

	// Subscribe so the client is in the hub's fan-out set.
	wsWrite(t, ctx, conn, internalws.Envelope{Type: internalws.TypeSubscribe, Subject: internalws.SubjectJobs, Seq: 1})
	mustAck(t, ctx, conn, 1)

	// Close the connection from the client side.
	_ = conn.Close(websocket.StatusNormalClosure, "done")

	// Allow the server read loop to detect the close and run its cleanup defers
	// (Deregister removes the client from the hub's subscription set).
	// There is no exported signal for "cleanup complete", so we use a short
	// sleep.  300 ms is generous on any CI host.
	time.Sleep(300 * time.Millisecond)

	// Post-disconnect notifications must complete without blocking.
	done := make(chan struct{})
	go func() {
		hub.NotifyJob(internalws.JobEvent{JobID: "post-disconnect", Status: "running"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("NotifyJob blocked after client disconnect — hub leak suspected")
	}
}

func TestWSHandler_MultipleClients_IndependentSubscriptions(t *testing.T) {
	hub := internalws.NewHub(newTestLogger())
	srv := newWSTestServer(t, hub)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	connA := dialTestWS(t, srv)
	connB := dialTestWS(t, srv)

	// connA subscribes to jobs; connB subscribes to workers.
	wsWrite(t, ctx, connA, internalws.Envelope{Type: internalws.TypeSubscribe, Subject: internalws.SubjectJobs, Seq: 1})
	mustAck(t, ctx, connA, 1)
	wsWrite(t, ctx, connB, internalws.Envelope{Type: internalws.TypeSubscribe, Subject: internalws.SubjectWorkers, Seq: 1})
	mustAck(t, ctx, connB, 1)

	// Notify jobs.
	hub.NotifyJob(internalws.JobEvent{JobID: "j1", Status: "running"})

	// connA must receive the jobs push.
	envA := wsRead(t, ctx, connA)
	if envA.Type != internalws.TypePush || envA.Subject != internalws.SubjectJobs {
		t.Fatalf("connA: expected jobs push, got type=%q subject=%q", envA.Type, envA.Subject)
	}

	// connB must not receive anything — confirm it is alive via ping.
	wsWrite(t, ctx, connB, internalws.Envelope{Type: internalws.TypePing})
	envB := wsRead(t, ctx, connB)
	if envB.Type != internalws.TypePong {
		t.Fatalf("connB: expected TypePong, got %q", envB.Type)
	}
}

func TestWSHandler_Subscribe_SinceSeq_ReplayDelivered(t *testing.T) {
	hub := internalws.NewHub(newTestLogger())
	srv := newWSTestServer(t, hub)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// First connection: subscribe and drive the ring with two events.
	connPrimer := dialTestWS(t, srv)
	wsWrite(t, ctx, connPrimer, internalws.Envelope{Type: internalws.TypeSubscribe, Subject: internalws.SubjectJobs, Seq: 1})
	mustAck(t, ctx, connPrimer, 1)

	hub.NotifyJob(internalws.JobEvent{JobID: "j1", Status: "pending"})
	push1 := wsRead(t, ctx, connPrimer) // Seq=1
	hub.NotifyJob(internalws.JobEvent{JobID: "j2", Status: "pending"})
	push2 := wsRead(t, ctx, connPrimer) // Seq=2

	_ = push2 // not needed beyond capturing

	// Second connection reconnects with SinceSeq = push1.Seq.
	// It should replay only push2 (Seq > push1.Seq).
	connReconnect := dialTestWS(t, srv)
	payload, err := json.Marshal(internalws.SubscribePayload{SinceSeq: push1.Seq})
	if err != nil {
		t.Fatalf("marshal SubscribePayload: %v", err)
	}
	wsWrite(t, ctx, connReconnect, internalws.Envelope{
		Type:    internalws.TypeSubscribe,
		Subject: internalws.SubjectJobs,
		Seq:     1,
		Payload: payload,
	})
	mustAck(t, ctx, connReconnect, 1)

	// Expect exactly one replayed push (Seq > push1.Seq).
	replayed := wsRead(t, ctx, connReconnect)
	if replayed.Type != internalws.TypePush {
		t.Fatalf("expected TypePush replay, got %q", replayed.Type)
	}
	if replayed.Seq <= push1.Seq {
		t.Fatalf("replayed Seq=%d should be > sinceSeq=%d", replayed.Seq, push1.Seq)
	}
}

func TestWSHandler_BinaryFrame_ClosesConnection(t *testing.T) {
	srv := newWSTestServer(t, nil)
	conn := dialTestWS(t, srv)
	ctx := context.Background()

	// Binary frames are rejected with StatusUnsupportedData (1003).
	if err := conn.Write(ctx, websocket.MessageBinary, []byte{0x00, 0x01}); err != nil {
		t.Fatalf("conn.Write binary: %v", err)
	}

	// The server closes the connection; the next read should return a close error.
	_, _, err := conn.Read(ctx)
	if err == nil {
		t.Fatal("expected connection close after binary frame, got nil error")
	}
	var closeErr websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("expected websocket.CloseError, got %T: %v", err, err)
	}
	if closeErr.Code != websocket.StatusUnsupportedData {
		t.Fatalf("expected StatusUnsupportedData (%d), got %d", websocket.StatusUnsupportedData, closeErr.Code)
	}
}

func TestWSHandler_Subscribe_MalformedPayload_ReturnsError(t *testing.T) {
	hub := internalws.NewHub(newTestLogger())
	srv := newWSTestServer(t, hub)
	conn := dialTestWS(t, srv)
	ctx := context.Background()

	// Build the frame manually: the outer envelope is valid JSON, but the payload
	// field contains a since_seq value that cannot decode into uint64. This causes
	// handleSubscribe to fail the json.Unmarshal of SubscribePayload and return
	// a TypeAck with a non-empty Error. We write raw bytes directly because
	// json.Marshal rejects invalid json.RawMessage before the frame is sent.
	raw := []byte(`{"type":"subscribe","subject":"jobs","seq":9,"payload":{"since_seq":"not-a-number"}}`)
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("conn.Write: %v", err)
	}

	// The server will unmarshal the envelope successfully, then fail when it tries
	// to decode the subscribe payload into SubscribePayload, and return an ack
	// with an error.
	ack := mustAck(t, ctx, conn, 9)
	if ack.Error == "" {
		t.Fatal("expected non-empty Error for subscribe with malformed payload JSON")
	}
}

func TestWSHandler_Subscribe_TaskLogs_PushDelivered(t *testing.T) {
	// Subscribing to "tasks/{id}/logs" (SubjectTaskLogsFmt) and receiving a
	// NotifyLog event covers the log-channel fan-out path in ws.go.
	hub := internalws.NewHub(newTestLogger())
	srv := newWSTestServer(t, hub)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn := dialTestWS(t, srv)
	const taskID = "task-log-test"
	logSubject := fmt.Sprintf(internalws.SubjectTaskLogsFmt, taskID)

	wsWrite(t, ctx, conn, internalws.Envelope{
		Type:    internalws.TypeSubscribe,
		Subject: logSubject,
		Seq:     1,
	})
	mustAck(t, ctx, conn, 1)

	hub.NotifyLog(internalws.LogEvent{
		TaskID:    taskID,
		AttemptID: "attempt-1",
		SeqNum:    7,
		Stream:    "stdout",
		Data:      "hello from worker",
	})

	env := wsRead(t, ctx, conn)
	if env.Type != internalws.TypePush {
		t.Fatalf("expected TypePush for log event, got %q", env.Type)
	}
	if env.Subject != logSubject {
		t.Fatalf("expected subject %q, got %q", logSubject, env.Subject)
	}
	if env.Seq == 0 {
		t.Error("push envelope must have a non-zero Seq")
	}
}
