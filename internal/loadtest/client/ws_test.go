// SPDX-License-Identifier: AGPL-3.0-or-later

package client_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/internal/testserver"
	"github.com/uberware/sqi/internal/ws"
)

func TestWS_SubscribeAndReceive(t *testing.T) {
	srv := testserver.Start(t, testserver.Options{})
	c := client.New(srv.HTTPURL())
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	farmID, err := c.CreateFarm(ctx, "farm-ws")
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queueID, err := c.CreateQueue(ctx, farmID, "queue-ws", 0)
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}

	conn, err := client.DialWS(ctx, srv.HTTPURL(), "")
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.Subscribe(ctx, ws.SubjectJobs, 0); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	jobID, _, err := c.SubmitJob(ctx, client.SubmitRequest{FarmID: farmID, QueueID: queueID, Template: []byte(oneTaskTemplate)})
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}

	// The server pushes nothing when a job is created (the scheduler announces
	// status changes, and there is no worker here), so delete the job: the
	// hub announces its removal on the jobs subject.
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, srv.HTTPURL()+"/api/v1/jobs/"+jobID, nil)
	if err != nil {
		t.Fatalf("build delete: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete job: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete job: HTTP %d, want 204", resp.StatusCode)
	}

	recvCtx, recvCancel := context.WithTimeout(ctx, 5*time.Second)
	defer recvCancel()
	for {
		env, err := conn.Next(recvCtx)
		if err != nil {
			t.Fatalf("no push naming job %s within 5s: %v", jobID, err)
		}
		if env.Type == ws.TypePush && strings.Contains(string(env.Payload), jobID) {
			if env.Subject != ws.SubjectJobs {
				t.Errorf("subject = %q, want %q", env.Subject, ws.SubjectJobs)
			}
			return
		}
	}
}

func TestWS_SubscribeRefused(t *testing.T) {
	srv := testserver.Start(t, testserver.Options{})
	conn, err := client.DialWS(t.Context(), srv.HTTPURL(), "")
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.Subscribe(t.Context(), "not/a/subject", 0); err == nil {
		t.Fatal("Subscribe to an invalid subject succeeded, want the ack error")
	}
}

func TestWS_NextAfterClose(t *testing.T) {
	srv := testserver.Start(t, testserver.Options{})
	conn, err := client.DialWS(t.Context(), srv.HTTPURL(), "")
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := conn.Next(t.Context()); err == nil {
		t.Fatal("Next after Close returned no error")
	}
}
