// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"testing"
	"time"
)

func TestLeaseRequestReply(t *testing.T) {
	b := startBroker(t)
	server := newClient(t, b)
	worker := newClient(t, b)

	sub, err := server.SubscribeLease(func(workerID, queueID string, data []byte) []byte {
		if workerID != "w1" {
			t.Errorf("workerID = %q, want w1", workerID)
		}
		if queueID != "q1" {
			t.Errorf("queueID = %q, want q1", queueID)
		}
		return append([]byte("reply-to-"), data...)
	})
	if err != nil {
		t.Fatalf("SubscribeLease: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() }) //nolint:errcheck // test cleanup

	reply, err := worker.RequestLease(context.Background(), "w1", "q1", []byte("payload"), 2*time.Second)
	if err != nil {
		t.Fatalf("RequestLease: %v", err)
	}
	if string(reply) != "reply-to-payload" {
		t.Errorf("reply = %q, want reply-to-payload", reply)
	}
}

// TestLeaseRequestReply_WildcardTokenRoutes guards the queueless-worker
// regression: a request on the wildcard token reaches the server's work.lease.>
// subscription, whereas an empty queue token ("work.lease.w1.") routes to no
// responder.
func TestLeaseRequestReply_WildcardTokenRoutes(t *testing.T) {
	b := startBroker(t)
	server := newClient(t, b)
	worker := newClient(t, b)

	sub, err := server.SubscribeLease(func(_, queueID string, _ []byte) []byte {
		return []byte("got:" + queueID)
	})
	if err != nil {
		t.Fatalf("SubscribeLease: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() }) //nolint:errcheck // test cleanup

	// Wildcard token routes to the server.
	reply, err := worker.RequestLease(context.Background(), "w1", WildcardQueueToken, []byte("payload"), 2*time.Second)
	if err != nil {
		t.Fatalf("RequestLease(wildcard): %v", err)
	}
	if string(reply) != "got:"+WildcardQueueToken {
		t.Errorf("wildcard reply = %q, want got:%s", reply, WildcardQueueToken)
	}

	// Empty queue token does NOT route — this is the bug the wildcard token fixes.
	if _, err := worker.RequestLease(context.Background(), "w1", "", []byte("payload"), 300*time.Millisecond); err == nil {
		t.Error("empty-queue RequestLease unexpectedly succeeded; an empty queue token must not route to the server")
	}
}

// TestSubscribeLease_IgnoresUnparsableSubject pins the no-reply path: a request
// arriving on a subject that carries no worker identity gets no response at
// all, so the requester's own timeout is what ends the exchange.
func TestSubscribeLease_IgnoresUnparsableSubject(t *testing.T) {
	b := startBroker(t)
	server := newClient(t, b)
	worker := newClient(t, b)

	sub, err := server.SubscribeLease(func(workerID, queueID string, _ []byte) []byte {
		t.Errorf("handler called for an identity-less subject: (%q, %q)", workerID, queueID)
		return []byte("unexpected")
	})
	if err != nil {
		t.Fatalf("SubscribeLease: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() }) //nolint:errcheck // test cleanup

	// The pre-identity subject shape: a queue token with no worker before it.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := worker.nc.RequestWithContext(ctx, SubjectWorkLeasePrefix+".q1", []byte("payload")); err == nil {
		t.Fatal("request on an identity-less subject was answered; want a timeout")
	}
}
