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

// TestLeaseRequestReply_WildcardTokenRoutes checks that a queueless worker's
// request on the wildcard token reaches the server's work.lease.>
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

	// A queue token with no worker before it.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := worker.nc.RequestWithContext(ctx, SubjectWorkLeasePrefix+".q1", []byte("payload")); err == nil {
		t.Fatal("request on an identity-less subject was answered; want a timeout")
	}
}

// TestLeaseSubscription_StopAnswersEveryRequestItTook pins a graceful stop's
// lease handover. A parked request is answered once Stop wakes it,
// and the worker re-requests the moment a reply arrives. Every request the
// subscription took must be answered before Stop returns, since the server
// closes its connection right after; once the interest is gone a request
// must find no responder, so the worker retries after its backoff instead of
// waiting out its request timeout.
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
		time.Sleep(50 * time.Millisecond) // still answering after the wake
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
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sub.Stop(stopCtx, func() { close(release) }); err != nil {
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

// TestLeaseSubscription_StopWakesOnceTheInterestIsGone pins the order Stop
// keeps: wake runs only after the broker has stopped routing requests here,
// so a request sent from wake on (as a worker does on the reply wake
// releases) finds no responder rather than being taken and dropped.
func TestLeaseSubscription_StopWakesOnceTheInterestIsGone(t *testing.T) {
	b := startBroker(t)
	server := newClient(t, b)
	worker := newClient(t, b)

	sub, err := server.SubscribeLease(func(string, string, []byte) []byte { return []byte("empty") })
	if err != nil {
		t.Fatalf("SubscribeLease: %v", err)
	}
	for i := range 20 {
		if _, err := worker.RequestLease(context.Background(), "w1", "q1", nil, 2*time.Second); err != nil {
			t.Fatalf("request %d before Stop: %v", i, err)
		}
	}

	var wakes int
	var afterWake error
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sub.Stop(stopCtx, func() {
		wakes++
		_, afterWake = worker.RequestLease(context.Background(), "w1", "q1", nil, 2*time.Second)
	}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if wakes != 1 {
		t.Errorf("wake ran %d times, want 1", wakes)
	}
	if !errors.Is(afterWake, nats.ErrNoResponders) {
		t.Errorf("a request sent from wake ended with %v, want %v", afterWake, nats.ErrNoResponders)
	}
}

// TestLeaseSubscription_StopWakesWhenItFails pins that wake runs exactly once
// even when Stop cannot drain, so the replies a caller holds back until wake
// are never stranded.
func TestLeaseSubscription_StopWakesWhenItFails(t *testing.T) {
	b := startBroker(t)
	server, err := b.NewClient()
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	sub, err := server.SubscribeLease(func(string, string, []byte) []byte { return nil })
	if err != nil {
		t.Fatalf("SubscribeLease: %v", err)
	}
	server.Close()

	var wakes int
	if err := sub.Stop(context.Background(), func() { wakes++ }); err == nil {
		t.Error("Stop on a closed connection returned nil, want an error")
	}
	if wakes != 1 {
		t.Errorf("wake ran %d times, want 1", wakes)
	}
}
