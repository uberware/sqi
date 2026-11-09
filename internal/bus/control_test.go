// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
)

func TestWorkerControlSubject(t *testing.T) {
	tests := []struct {
		workerID string
		want     string
	}{
		{"w1", "worker.control.w1"},
		{"7b1f6c52-3f0e-4c2a-9d57-0a1b2c3d4e5f", "worker.control.7b1f6c52-3f0e-4c2a-9d57-0a1b2c3d4e5f"},
	}
	for _, tt := range tests {
		if got := WorkerControlSubject(tt.workerID); got != tt.want {
			t.Errorf("WorkerControlSubject(%q) = %q, want %q", tt.workerID, got, tt.want)
		}
	}
	if SubjectWorkerControlPrefix != "worker.control" {
		t.Errorf("SubjectWorkerControlPrefix = %q, want worker.control", SubjectWorkerControlPrefix)
	}
}

func TestRequestWorkerControl_RoundTrip(t *testing.T) {
	b := startBroker(t)
	server := newClient(t, b)
	worker := newClient(t, b)

	sub, err := worker.nc.Subscribe(WorkerControlSubject("w1"), func(msg *nats.Msg) {
		_ = msg.Respond(append([]byte("ack:"), msg.Data...)) //nolint:errcheck // test responder
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() }) //nolint:errcheck // test cleanup
	if err := worker.nc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reply, err := server.RequestWorkerControl(ctx, "w1", []byte("sleep"))
	if err != nil {
		t.Fatalf("RequestWorkerControl: %v", err)
	}
	if string(reply) != "ack:sleep" {
		t.Errorf("reply = %q, want ack:sleep", reply)
	}

	// A worker nobody is subscribed for fails fast rather than waiting out ctx.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if _, err := server.RequestWorkerControl(ctx2, "absent", []byte("sleep")); err == nil {
		t.Error("RequestWorkerControl to an absent worker: want an error")
	}
}

// TestStreams_DoNotCaptureWorkerControl: a control request is core-NATS
// request/reply. A stream covering worker.control.> would ack it as a
// JetStream publish and answer the requester with a PubAck in place of the
// worker's reply.
func TestStreams_DoNotCaptureWorkerControl(t *testing.T) {
	subject := WorkerControlSubject("x")
	for _, def := range streamDefs(1) {
		for _, filter := range def.Subjects {
			if streamFilterMatches(filter, subject) {
				t.Errorf("stream %s subject filter %q captures %q", def.Name, filter, subject)
			}
		}
	}
}

func streamFilterMatches(filter, subject string) bool {
	ft := strings.Split(filter, ".")
	st := strings.Split(subject, ".")
	for i, f := range ft {
		if f == ">" {
			return i < len(st)
		}
		if i >= len(st) {
			return false
		}
		if f != "*" && f != st[i] {
			return false
		}
	}
	return len(ft) == len(st)
}

// TestEmbedded_WorkerAnswersControlRequestUnderItsGrants runs a control
// request through a broker with authentication on: the enrolled worker
// subscribes to its own control subject and answers through its response
// grant, with no publish permission on the reply subject, and cannot
// subscribe to another worker's control subject.
func TestEmbedded_WorkerAnswersControlRequestUnderItsGrants(t *testing.T) {
	creds := newCredentialMap()
	seedA, pubA := creds.enroll(t, "worker-a")
	b := startAuthEmbedded(t, creds)

	srv, err := b.NewClient()
	if err != nil {
		t.Fatalf("broker NewClient: %v", err)
	}
	defer srv.Close()

	ncA, err := nats.Connect(b.ClientURL(), nkeyOption(t, seedA, pubA))
	if err != nil {
		t.Fatalf("connect as worker-a: %v", err)
	}
	defer ncA.Close()
	sub, err := ncA.Subscribe(WorkerControlSubject("worker-a"), func(msg *nats.Msg) {
		_ = msg.Respond([]byte(`{"accepted":true}`)) //nolint:errcheck // test responder
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Unsubscribe() //nolint:errcheck // best-effort test cleanup
	if err := ncA.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reply, err := srv.RequestWorkerControl(ctx, "worker-a", []byte(`{"command":"sleep"}`))
	if err != nil {
		t.Fatalf("RequestWorkerControl: %v", err)
	}
	if string(reply) != `{"accepted":true}` {
		t.Errorf("reply = %q", reply)
	}

	// Another worker's control subject stays closed to this one.
	var denied atomic.Bool
	ncA.SetErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
		if errors.Is(err, nats.ErrPermissionViolation) {
			denied.Store(true)
		}
	})
	spy, err := ncA.SubscribeSync(WorkerControlSubject("worker-b"))
	if err != nil {
		t.Fatalf("spy subscribe: %v", err)
	}
	defer spy.Unsubscribe() //nolint:errcheck // best-effort test cleanup
	if err := ncA.Flush(); err != nil && !errors.Is(err, nats.ErrPermissionViolation) {
		t.Fatalf("Flush: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !denied.Load() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !denied.Load() {
		t.Error("worker-a subscribed to worker-b's control subject without a permission violation")
	}
}
