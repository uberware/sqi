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
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	internalws "github.com/uberware/sqi/internal/ws"
)

// ── test server helpers ───────────────────────────────────────────────────────

// newWSTestServer starts an httptest.Server serving wsHandler with origin
// checking disabled (wsOriginConfig{}, the pre-A1 / auth-off default: the
// zero value has Enabled == false).
// hub may be nil when the test does not need fan-out.
func newWSTestServer(t *testing.T, hub *internalws.Hub) *httptest.Server {
	t.Helper()
	h := newWSHandler(newTestLogger(), hub, fake.New(), auth.Anonymous(), wsOriginConfig{})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// newWSTestServerWithOrigin starts an httptest.Server serving wsHandler with
// the given origin config, for tests exercising OriginPatterns enforcement.
func newWSTestServerWithOrigin(t *testing.T, hub *internalws.Hub, origin wsOriginConfig) *httptest.Server {
	t.Helper()
	h := newWSHandler(newTestLogger(), hub, fake.New(), auth.Anonymous(), origin)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// stubWSAuthenticator is a test [auth.Authenticator] that always returns err.
type stubWSAuthenticator struct{ err error }

func (s stubWSAuthenticator) Authenticate(*http.Request) (auth.Principal, error) {
	return auth.Principal{}, s.err
}

// stubPrincipalAuthenticator is a test [auth.Authenticator] that always
// succeeds, returning principal unmodified. Used to exercise subject-level
// permission gates (e.g. SubjectDiagnostics) with a specific role set.
type stubPrincipalAuthenticator struct{ principal auth.Principal }

func (s stubPrincipalAuthenticator) Authenticate(*http.Request) (auth.Principal, error) {
	return s.principal, nil
}

// newWSTestServerWithPrincipal starts an httptest.Server serving wsHandler
// authenticated as principal, for tests exercising subject-level permission
// gates.
func newWSTestServerWithPrincipal(t *testing.T, hub *internalws.Hub, principal auth.Principal) *httptest.Server {
	t.Helper()
	h := newWSHandler(newTestLogger(), hub, fake.New(), stubPrincipalAuthenticator{principal: principal}, wsOriginConfig{})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// newWSTestServerScoped starts a ws server authenticated as principal and
// backed by st, for tests exercising job-ownership scoping.
func newWSTestServerScoped(
	t *testing.T, hub *internalws.Hub, st store.Store, principal auth.Principal,
) *httptest.Server {
	t.Helper()
	h := newWSHandler(newTestLogger(), hub, st,
		stubPrincipalAuthenticator{principal: principal}, wsOriginConfig{})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
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

// ackError decodes env's AckPayload and returns its Error field.  Callers that
// already know env is a TypeAck (via wsRead) use this instead of mustAck when
// the expected ClientSeq is not the point of the assertion.
func ackError(t *testing.T, env internalws.Envelope) string {
	t.Helper()
	var ack internalws.AckPayload
	if err := json.Unmarshal(env.Payload, &ack); err != nil {
		t.Fatalf("unmarshal AckPayload: %v", err)
	}
	return ack.Error
}

// ── tests ─────────────────────────────────────────────────────────────────────

func TestWS_FailingAuthenticator_Returns401BeforeUpgrade(t *testing.T) {
	h := newWSHandler(newTestLogger(), nil, fake.New(), stubWSAuthenticator{err: errors.New("bad token")}, wsOriginConfig{})
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

// TestWS_FailingAuthenticator_WritesProblemDetails pins the WS 401 to the same
// RFC-7807 shape the REST 401 uses, so a client hitting an expired session sees
// one error contract across both surfaces rather than plain text here and JSON
// there.
func TestWS_FailingAuthenticator_WritesProblemDetails(t *testing.T) {
	h := newWSHandler(newTestLogger(), nil, fake.New(), stubWSAuthenticator{err: errors.New("bad token")}, wsOriginConfig{})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL) //nolint:noctx // simple synchronous test request
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	var body struct {
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode problem body: %v", err)
	}
	if body.Status != http.StatusUnauthorized || body.Title != "Unauthorized" {
		t.Errorf("problem = %+v, want status 401 / title Unauthorized", body)
	}
	if body.Detail != "authentication required" {
		t.Errorf("detail = %q, want %q (same wording as the REST 401)", body.Detail, "authentication required")
	}
	// The authenticator's internal reason must not reach the client.
	if strings.Contains(body.Detail, "bad token") {
		t.Errorf("detail leaks the authenticator's internal error: %q", body.Detail)
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
	hub := internalws.NewHub(newTestLogger(), internalws.HubOptions{})
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
	hub := internalws.NewHub(newTestLogger(), internalws.HubOptions{})
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

// TestWSSubscribe_DiagnosticsRequiresPermission pins the diagnostics.read gate
// on the SubjectDiagnostics WebSocket subject: operator (and superuser/
// anonymous) principals may subscribe, read-only principals are nacked and
// never registered with the hub, and the gate is diagnostics-specific (a
// read-only principal still subscribes fine to an unrelated subject).
func TestWSSubscribe_DiagnosticsRequiresPermission(t *testing.T) {
	operator := auth.Principal{Subject: "op1", Roles: []string{"operator"}, Kind: auth.KindUser}
	readOnly := auth.Principal{Subject: "ro1", Roles: []string{"read-only"}, Kind: auth.KindUser}

	t.Run("operator subscribes ok", func(t *testing.T) {
		hub := internalws.NewHub(newTestLogger(), internalws.HubOptions{})
		srv := newWSTestServerWithPrincipal(t, hub, operator)
		conn := dialTestWS(t, srv)
		ctx := context.Background()

		wsWrite(t, ctx, conn, internalws.Envelope{
			Type:    internalws.TypeSubscribe,
			Subject: internalws.SubjectDiagnostics,
			Seq:     1,
		})
		ack := mustAck(t, ctx, conn, 1)
		if ack.Error != "" {
			t.Fatalf("operator: unexpected ack error: %q", ack.Error)
		}
	})

	t.Run("read-only forbidden and not registered", func(t *testing.T) {
		hub := internalws.NewHub(newTestLogger(), internalws.HubOptions{})
		srv := newWSTestServerWithPrincipal(t, hub, readOnly)
		conn := dialTestWS(t, srv)
		ctx := context.Background()

		wsWrite(t, ctx, conn, internalws.Envelope{
			Type:    internalws.TypeSubscribe,
			Subject: internalws.SubjectDiagnostics,
			Seq:     1,
		})
		ack := mustAck(t, ctx, conn, 1)
		if ack.Error == "" {
			t.Fatal("read-only: expected non-empty ack error for diagnostics subscribe")
		}
		if !strings.Contains(ack.Error, "forbidden") {
			t.Fatalf("read-only: ack error = %q, want it to mention forbidden", ack.Error)
		}

		// Not registered: a diagnostics event must not be delivered. Confirm the
		// connection is otherwise alive via ping/pong rather than a push.
		hub.NotifyDiag(internalws.DiagEvent{Component: "server", Level: "INFO", Msg: "should not be delivered"})
		wsWrite(t, ctx, conn, internalws.Envelope{Type: internalws.TypePing})
		env := wsRead(t, ctx, conn)
		if env.Type != internalws.TypePong {
			t.Fatalf("expected TypePong (connection alive, no diagnostics push), got %q", env.Type)
		}
	})

	t.Run("read-only still allowed on a non-diagnostics subject", func(t *testing.T) {
		hub := internalws.NewHub(newTestLogger(), internalws.HubOptions{})
		srv := newWSTestServerWithPrincipal(t, hub, readOnly)
		conn := dialTestWS(t, srv)
		ctx := context.Background()

		wsWrite(t, ctx, conn, internalws.Envelope{
			Type:    internalws.TypeSubscribe,
			Subject: internalws.SubjectJobs,
			Seq:     1,
		})
		ack := mustAck(t, ctx, conn, 1)
		if ack.Error != "" {
			t.Fatalf("read-only: unexpected ack error for non-diagnostics subject: %q", ack.Error)
		}
	})

	t.Run("anonymous superuser subscribes ok", func(t *testing.T) {
		hub := internalws.NewHub(newTestLogger(), internalws.HubOptions{})
		srv := newWSTestServer(t, hub) // uses auth.Anonymous(), which is Superuser
		conn := dialTestWS(t, srv)
		ctx := context.Background()

		wsWrite(t, ctx, conn, internalws.Envelope{
			Type:    internalws.TypeSubscribe,
			Subject: internalws.SubjectDiagnostics,
			Seq:     1,
		})
		ack := mustAck(t, ctx, conn, 1)
		if ack.Error != "" {
			t.Fatalf("anonymous/superuser: unexpected ack error: %q", ack.Error)
		}
	})
}

func TestWSHandler_Unsubscribe_ReturnsAck(t *testing.T) {
	hub := internalws.NewHub(newTestLogger(), internalws.HubOptions{})
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
	hub := internalws.NewHub(newTestLogger(), internalws.HubOptions{})
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
	hub := internalws.NewHub(newTestLogger(), internalws.HubOptions{})
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
	hub := internalws.NewHub(newTestLogger(), internalws.HubOptions{})
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
	hub := internalws.NewHub(newTestLogger(), internalws.HubOptions{})
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
	hub := internalws.NewHub(newTestLogger(), internalws.HubOptions{})
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
	hub := internalws.NewHub(newTestLogger(), internalws.HubOptions{})
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
	hub := internalws.NewHub(newTestLogger(), internalws.HubOptions{})
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

// ── Origin hardening (CSWSH) ──────────────────────────────────────────────────
//
// These three tests exercise wsOriginConfig end to end through the real
// websocket.Accept/Dial handshake (not just the originPatterns helper in
// isolation), because the security property that matters is what the library
// actually does with OriginPatterns vs InsecureSkipVerify.

func TestWSHandler_OriginHardening_AuthOn_ForeignOriginRejected(t *testing.T) {
	srv := newWSTestServerWithOrigin(t, nil, wsOriginConfig{
		Enabled:        true,
		AllowedOrigins: []string{"http://localhost"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, wsTestURL(srv), &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"http://evil.example"}},
	})
	if err == nil {
		conn.CloseNow() //nolint:errcheck // test cleanup on unexpected success
		t.Fatal("expected the handshake to fail for a foreign, non-allow-listed Origin")
	}
	if resp != nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusSwitchingProtocols {
			t.Fatalf("expected a non-101 status for a rejected origin, got %d", resp.StatusCode)
		}
	}
}

func TestWSHandler_OriginHardening_AuthOn_SameOriginAllowed(t *testing.T) {
	srv := newWSTestServerWithOrigin(t, nil, wsOriginConfig{
		Enabled:        true,
		AllowedOrigins: []string{"http://localhost"}, // irrelevant here: same-origin is always allowed
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The Origin equals srv.URL itself, i.e. same-origin with the request Host —
	// coder/websocket authorizes this unconditionally, even with an
	// OriginPatterns list that doesn't mention it.
	conn, resp, err := websocket.Dial(ctx, wsTestURL(srv), &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{srv.URL}},
	})
	if err != nil {
		t.Fatalf("expected same-origin handshake to succeed, got: %v", err)
	}
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	t.Cleanup(func() {
		if err := conn.CloseNow(); err != nil {
			t.Logf("ws cleanup: CloseNow: %v", err)
		}
	})
}

func TestWSHandler_OriginHardening_AuthOff_AnyOriginAllowed(t *testing.T) {
	// Auth off: InsecureSkipVerify stays true regardless of Origin — the
	// byte-for-byte pre-A1 regression guarantee.
	srv := newWSTestServerWithOrigin(t, nil, wsOriginConfig{Enabled: false})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, wsTestURL(srv), &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"http://evil.example"}},
	})
	if err != nil {
		t.Fatalf("auth-off must allow any Origin (unchanged pre-A1 behavior), got: %v", err)
	}
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	t.Cleanup(func() {
		if err := conn.CloseNow(); err != nil {
			t.Logf("ws cleanup: CloseNow: %v", err)
		}
	})
}

func TestWSHandler_Subscribe_TaskLogs_PushDelivered(t *testing.T) {
	// Subscribing to "tasks/{id}/logs" (SubjectTaskLogsFmt) and receiving a
	// NotifyLog event covers the log-channel fan-out path in ws.go.
	hub := internalws.NewHub(newTestLogger(), internalws.HubOptions{})
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

// A scoped client must not be able to subscribe to another owner's per-job
// task stream or task log stream.
func TestWSSubscribeJobSubjectsEnforceOwnership(t *testing.T) {
	tests := []struct {
		name      string
		subject   string
		principal auth.Principal
		wantErr   string
	}{
		{
			name:      "per-job task subject for another owner is refused",
			subject:   "jobs/job-bob/tasks",
			principal: auth.Principal{Username: "alice", Roles: []string{"user"}},
			wantErr:   "forbidden: not your job",
		},
		{
			name:      "task log subject for another owner is refused",
			subject:   "tasks/task-bob/logs",
			principal: auth.Principal{Username: "alice", Roles: []string{"user"}},
			wantErr:   "forbidden: not your job",
		},
		{
			name:      "own job is allowed",
			subject:   "jobs/job-alice/tasks",
			principal: auth.Principal{Username: "alice", Roles: []string{"user"}},
			wantErr:   "",
		},
		{
			name:      "operator reaches any job",
			subject:   "jobs/job-bob/tasks",
			principal: auth.Principal{Username: "carol", Roles: []string{"operator"}},
			wantErr:   "",
		},
		{
			name:      "anonymous superuser is unrestricted (auth-off regression)",
			subject:   "jobs/job-bob/tasks",
			principal: auth.Principal{Superuser: true, Kind: auth.KindAnonymous},
			wantErr:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := fake.New()
			seedOwnedJob(t, st, "job-bob", "bob")
			seedOwnedJob(t, st, "job-alice", "alice")
			if _, err := st.CreateTask(t.Context(), store.Task{
				ID: "task-bob", JobID: "job-bob", Status: store.TaskStatusReady,
			}); err != nil {
				t.Fatalf("CreateTask: %v", err)
			}

			hub := internalws.NewHub(newTestLogger(), internalws.HubOptions{})
			srv := newWSTestServerScoped(t, hub, st, tt.principal)
			conn := dialTestWS(t, srv)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			wsWrite(t, ctx, conn, internalws.Envelope{
				Type:    internalws.TypeSubscribe,
				Subject: tt.subject,
				Seq:     1,
			})

			ack := wsRead(t, ctx, conn)
			if ack.Type != internalws.TypeAck {
				t.Fatalf("type = %q, want %q", ack.Type, internalws.TypeAck)
			}
			gotErr := ackError(t, ack)
			if gotErr != tt.wantErr {
				t.Errorf("ack error = %q, want %q", gotErr, tt.wantErr)
			}
		})
	}
}

// TestWSSubscribeJobSubjects_NoStoreFailsClosed pins the fail-closed
// requirement that a scoped client whose connection has no store wired
// (wc.store == nil) must be refused a per-job subscription rather than
// granted it — the ownership check has nothing to consult, so it must not
// default to allow.
func TestWSSubscribeJobSubjects_NoStoreFailsClosed(t *testing.T) {
	hub := internalws.NewHub(newTestLogger(), internalws.HubOptions{})
	h := newWSHandler(newTestLogger(), hub, nil,
		stubPrincipalAuthenticator{principal: auth.Principal{Username: "alice", Roles: []string{"user"}}},
		wsOriginConfig{})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	conn := dialTestWS(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsWrite(t, ctx, conn, internalws.Envelope{
		Type:    internalws.TypeSubscribe,
		Subject: "jobs/job-alice/tasks",
		Seq:     1,
	})

	ack := wsRead(t, ctx, conn)
	gotErr := ackError(t, ack)
	if gotErr != "forbidden: not your job" {
		t.Errorf("ack error = %q, want %q (nil store must fail closed)", gotErr, "forbidden: not your job")
	}
}

// TestWSSubscribeJobSubjects_UnresolvableJobFailsClosed pins the fail-closed
// requirement that a scoped client subscribing to a subject whose job/task
// does not exist (owner cannot be resolved) is refused, not granted access.
func TestWSSubscribeJobSubjects_UnresolvableJobFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		subject string
	}{
		{name: "missing job", subject: "jobs/does-not-exist/tasks"},
		{name: "missing task", subject: "tasks/does-not-exist/logs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := fake.New()
			hub := internalws.NewHub(newTestLogger(), internalws.HubOptions{})
			srv := newWSTestServerScoped(t, hub, st, auth.Principal{Username: "alice", Roles: []string{"user"}})
			conn := dialTestWS(t, srv)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			wsWrite(t, ctx, conn, internalws.Envelope{
				Type:    internalws.TypeSubscribe,
				Subject: tt.subject,
				Seq:     1,
			})

			ack := wsRead(t, ctx, conn)
			gotErr := ackError(t, ack)
			if gotErr != "forbidden: not your job" {
				t.Errorf("ack error = %q, want %q (unresolvable job/task must fail closed)", gotErr, "forbidden: not your job")
			}
		})
	}
}

// TestWSSubscribeJobSubjects_StoreErrorFailsClosed pins the fail-closed
// requirement that a genuine (non-ErrNotFound) store error on the ownership
// gate denies the subscription rather than granting it. Uses storeErr
// (jobs_error_test.go), the same package's existing error-injection wrapper
// built for exactly this — contrary to an earlier version of this task's
// report, no new error-injection hook was needed. Covers both routes into
// subjectAllowed: GetJob (via "jobs/{id}/tasks") and GetTask (via
// "tasks/{id}/logs").
func TestWSSubscribeJobSubjects_StoreErrorFailsClosed(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name    string
		subject string
		st      store.Store
	}{
		{
			name:    "GetJob error on jobs/{id}/tasks",
			subject: "jobs/job-1/tasks",
			st:      &storeErr{Store: fake.New(), getJobErr: boom},
		},
		{
			name:    "GetTask error on tasks/{id}/logs",
			subject: "tasks/task-1/logs",
			st:      &storeErr{Store: fake.New(), getTaskErr: boom},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := internalws.NewHub(newTestLogger(), internalws.HubOptions{})
			srv := newWSTestServerScoped(t, hub, tt.st, auth.Principal{Username: "alice", Roles: []string{"user"}})
			conn := dialTestWS(t, srv)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			wsWrite(t, ctx, conn, internalws.Envelope{
				Type:    internalws.TypeSubscribe,
				Subject: tt.subject,
				Seq:     1,
			})

			ack := wsRead(t, ctx, conn)
			gotErr := ackError(t, ack)
			if gotErr != "failed to resolve job" {
				t.Errorf("ack error = %q, want %q (a store error must deny, not grant)", gotErr, "failed to resolve job")
			}
		})
	}
}

// TestWSSubscribeJobs_ScopedClientSeesOnlyOwnJobEvents pins the two things the
// Task-8 review found had zero coverage:
//
//  1. readLoop's Register call (ws.go) must actually pass the connection's
//     real scope (internalws.Scope{Owner: owner, All: !scoped}) derived from
//     scopeFilter, not the pre-Task-8 Scope{All: true} placeholder — this is
//     exercised end to end via a real WebSocket connection over
//     newWSTestServerScoped, not by calling hub.Register directly.
//  2. NotifyJob's owner resolution: JobEvent.Owner is left empty here on
//     purpose, matching the real production call sites (Finding 1) — the hub
//     must resolve ownership via the injected owner-cache resolver.
//
// A scoped client subscribed to the global "jobs" subject must receive the
// push for its own job and must never receive the push for another owner's
// job.
func TestWSSubscribeJobs_ScopedClientSeesOnlyOwnJobEvents(t *testing.T) {
	st := fake.New()
	seedOwnedJob(t, st, "job-alice", "alice")
	seedOwnedJob(t, st, "job-bob", "bob")

	hub := internalws.NewHub(newTestLogger(), internalws.HubOptions{
		OwnerScoping: true,
		JobOwner: func(jobID string) (string, error) {
			job, err := st.GetJob(context.Background(), jobID)
			if err != nil {
				return "", err
			}
			return job.Owner, nil
		},
	})

	srv := newWSTestServerScoped(t, hub, st, auth.Principal{Username: "alice", Roles: []string{"user"}})
	conn := dialTestWS(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsWrite(t, ctx, conn, internalws.Envelope{Type: internalws.TypeSubscribe, Subject: internalws.SubjectJobs, Seq: 1})
	mustAck(t, ctx, conn, 1)

	// Own job's status transition — JobEvent.Owner deliberately left empty.
	hub.NotifyJob(internalws.JobEvent{JobID: "job-alice", Status: "running"})
	// Another owner's job transition — must never reach alice's connection.
	hub.NotifyJob(internalws.JobEvent{JobID: "job-bob", Status: "running"})

	env := wsRead(t, ctx, conn)
	if env.Type != internalws.TypePush || env.Subject != internalws.SubjectJobs {
		t.Fatalf("expected jobs push, got type=%q subject=%q", env.Type, env.Subject)
	}
	var push internalws.JobSummaryPush
	if err := json.Unmarshal(env.Payload, &push); err != nil {
		t.Fatalf("unmarshal push: %v", err)
	}
	if push.JobID != "job-alice" {
		t.Fatalf("job_id = %q, want %q (own job)", push.JobID, "job-alice")
	}

	// Confirm job-bob's event never arrives: a ping/pong round-trip must be
	// the very next frame, not a second push.
	wsWrite(t, ctx, conn, internalws.Envelope{Type: internalws.TypePing})
	pong := wsRead(t, ctx, conn)
	if pong.Type != internalws.TypePong {
		// Name the job in the unexpected frame. The two ways this can fail are
		// not equally serious and the frame type alone cannot tell them apart:
		// a push for job-bob is a cross-owner leak — the security property this
		// test exists to protect — whereas a second push for job-alice is a
		// duplicate-delivery bug. The latter is what made this assertion flaky
		// in CI until the hub stopped replaying events it had already fanned
		// out live (see TestHub_Subscribe_NoDuplicateAcrossRegisterAndReplay).
		// Say which one happened rather than making the next reader guess.
		detail := ""
		if pong.Type == internalws.TypePush && pong.Subject == internalws.SubjectJobs {
			var unexpected internalws.JobSummaryPush
			if err := json.Unmarshal(pong.Payload, &unexpected); err == nil {
				switch unexpected.JobID {
				case "job-bob":
					detail = " — CROSS-OWNER LEAK: alice received a push for job-bob"
				case "job-alice":
					detail = " — duplicate push for job-alice (not a leak; the hub delivered it twice)"
				default:
					detail = fmt.Sprintf(" — unexpected job_id %q", unexpected.JobID)
				}
			}
		}
		t.Fatalf("expected TypePong (no cross-owner push queued), got type=%q subject=%q%s",
			pong.Type, pong.Subject, detail)
	}
}
