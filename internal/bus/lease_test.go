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

	sub, err := server.SubscribeLease(func(queueID string, data []byte) []byte {
		if queueID != "q1" {
			t.Errorf("queueID = %q, want q1", queueID)
		}
		return append([]byte("reply-to-"), data...)
	})
	if err != nil {
		t.Fatalf("SubscribeLease: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() }) //nolint:errcheck // test cleanup

	reply, err := worker.RequestLease(context.Background(), "q1", []byte("w1"), 2*time.Second)
	if err != nil {
		t.Fatalf("RequestLease: %v", err)
	}
	if string(reply) != "reply-to-w1" {
		t.Errorf("reply = %q, want reply-to-w1", reply)
	}
}

// TestLeaseRequestReply_WildcardTokenRoutes guards the queueless-worker
// regression: a request on the wildcard token reaches the server's work.lease.>
// subscription, whereas an empty leaf ("work.lease.") routes to no responder.
func TestLeaseRequestReply_WildcardTokenRoutes(t *testing.T) {
	b := startBroker(t)
	server := newClient(t, b)
	worker := newClient(t, b)

	sub, err := server.SubscribeLease(func(queueID string, _ []byte) []byte {
		return []byte("got:" + queueID)
	})
	if err != nil {
		t.Fatalf("SubscribeLease: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() }) //nolint:errcheck // test cleanup

	// Wildcard token routes to the server.
	reply, err := worker.RequestLease(context.Background(), WildcardQueueToken, []byte("w1"), 2*time.Second)
	if err != nil {
		t.Fatalf("RequestLease(wildcard): %v", err)
	}
	if string(reply) != "got:"+WildcardQueueToken {
		t.Errorf("wildcard reply = %q, want got:%s", reply, WildcardQueueToken)
	}

	// Empty leaf does NOT route — this is the bug the wildcard token fixes.
	if _, err := worker.RequestLease(context.Background(), "", []byte("w1"), 300*time.Millisecond); err == nil {
		t.Error("empty-queue RequestLease unexpectedly succeeded; an empty leaf must not route to the server")
	}
}
