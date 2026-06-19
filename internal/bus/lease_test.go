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
