// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"errors"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
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

	// An empty queue token does not route; the wildcard token exists for that case.
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

// TestLeaseSubscription_StopAnswersEveryRequestItTook pins a graceful stop's
// lease handover (H13f11). The stopping scheduler wakes a parked request,
// whose reply is still being sent when Stop is called, and the worker
// re-requests the moment a reply arrives. Every request the subscription took
// must be answered before Stop returns, since the server closes its
// connection right after; a request sent after the interest is gone must find
// no responder, so the worker retries elsewhere at once instead of waiting
// out its request timeout.
func TestLeaseSubscription_StopAnswersEveryRequestItTook(t *testing.T) {
	b := startBroker(t)
	server, err := b.NewClient()
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer server.Close()
	worker := newClient(t, b)

	parked := make(chan struct{}, 1)
	release := make(chan struct{})
	sub, err := server.SubscribeLease(func(string, string, []byte) []byte {
		select {
		case parked <- struct{}{}:
		default:
		}
		<-release
		time.Sleep(50 * time.Millisecond) // still answering when Stop is called
		return []byte("empty")
	})
	if err != nil {
		t.Fatalf("SubscribeLease: %v", err)
	}

	const requestTimeout = 3 * time.Second
	ended := make(chan error, 1)
	go func() {
		for {
			if _, err := worker.RequestLease(context.Background(), "w1", "q1", nil, requestTimeout); err != nil {
				ended <- err
				return
			}
		}
	}()

	<-parked
	close(release)
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sub.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	server.Close()

	select {
	case err := <-ended:
		if !errors.Is(err, nats.ErrNoResponders) {
			t.Fatalf("the worker's request ended with %v, want %v (a reply was lost)", err, nats.ErrNoResponders)
		}
	case <-time.After(2 * requestTimeout):
		t.Fatal("the worker's requests never ended")
	}
}
