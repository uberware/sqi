// SPDX-License-Identifier: AGPL-3.0-or-later

package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/ws"
)

// TestClient_SubmitJobHasNoDeadlineOfItsOwn answers a submission after longer
// than the per-request deadline: SubmitJob must still succeed, while any other
// call times out.
func TestClient_SubmitJobHasNoDeadlineOfItsOwn(t *testing.T) {
	defer client.SetRequestTimeout(50 * time.Millisecond)()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(300 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"job-1"}`)) //nolint:errcheck // test handler
	}))
	defer ts.Close()
	c := client.New(ts.URL)

	id, status, err := c.SubmitJob(t.Context(), client.SubmitRequest{FarmID: "f", QueueID: "q", Template: []byte("x")})
	if err != nil || status != http.StatusCreated || id != "job-1" {
		t.Fatalf("SubmitJob = %q, %d, %v; want job-1, 201, nil after a slow answer", id, status, err)
	}

	if _, err := c.GetJob(t.Context(), "job-1"); err == nil {
		t.Error("GetJob succeeded past the per-request deadline")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, status, err := c.SubmitJob(ctx, client.SubmitRequest{FarmID: "f", QueueID: "q", Template: []byte("x")}); err == nil || status != 0 {
		t.Errorf("SubmitJob with an expiring ctx = %d, %v; want a transport error", status, err)
	}
}

// wsAckServer is a TLS WebSocket server that acks the first subscribe.
func wsAckServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow() //nolint:errcheck // test handler
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var env ws.Envelope
		if json.Unmarshal(data, &env) != nil {
			return
		}
		payload, _ := json.Marshal(ws.AckPayload{ClientSeq: env.Seq})           //nolint:errcheck // test data
		out, _ := json.Marshal(ws.Envelope{Type: ws.TypeAck, Payload: payload}) //nolint:errcheck // test data
		_ = conn.Write(r.Context(), websocket.MessageText, out)                 //nolint:errcheck // test handler
		<-r.Context().Done()
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestDialWS_TrustsACustomCA dials a wss:// server whose certificate only the
// supplied http.Client trusts.
func TestDialWS_TrustsACustomCA(t *testing.T) {
	ts := wsAckServer(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	if conn, err := client.DialWS(ctx, ts.URL, ""); err == nil {
		_ = conn.Close()
		t.Fatal("DialWS trusted an unknown CA")
	}

	conn, err := client.DialWS(ctx, ts.URL, "", client.WithWSHTTPClient(ts.Client()))
	if err != nil {
		t.Fatalf("DialWS with the server's CA: %v", err)
	}
	defer conn.Close()
	if err := conn.Subscribe(ctx, ws.SubjectJobs, 0); err != nil {
		t.Fatalf("Subscribe over TLS: %v", err)
	}
}

func TestClient_HTTPClientIsTheOneConfigured(t *testing.T) {
	hc := &http.Client{}
	if got := client.New("http://x", client.WithHTTPClient(hc)).HTTPClient(); got != hc {
		t.Error("HTTPClient did not return the configured client")
	}
}
