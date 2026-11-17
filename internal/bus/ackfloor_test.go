// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// TestConsumerAckFloor_PassesOnlyWhatWillNotBeDeliveredAgain pins, against a
// real broker and the real task-status consumer, the broker behavior the
// scheduler prunes its pending messages by: the ack floor passes a message
// once it is acked, removed from the stream, or out of deliveries (the last
// only once the consumer then has nothing else unacknowledged), and stays
// below a message that will still be delivered.
func TestConsumerAckFloor_PassesOnlyWhatWillNotBeDeliveredAgain(t *testing.T) {
	b := startBroker(t)
	c := newClient(t, b)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := c.ConsumerAckFloor(ctx, IngestTaskStatus); err == nil {
		t.Fatal("ConsumerAckFloor before the consumer exists: got no error")
	}

	var mu sync.Mutex
	mode := map[string]string{} // subject -> "ack", "nak" or "hold"
	delivered := map[string]uint64{}
	cc, err := c.ConsumeTaskStatus(ctx, func(m jetstream.Msg) {
		mu.Lock()
		how := mode[m.Subject()]
		delivered[m.Subject()]++
		mu.Unlock()
		switch how {
		case "ack":
			_ = m.Ack() //nolint:errcheck // a lost ack fails the floor assertions
		case "nak":
			_ = m.Nak() //nolint:errcheck // a lost nak only slows the test
		}
	})
	if err != nil {
		t.Fatalf("ConsumeTaskStatus: %v", err)
	}
	t.Cleanup(cc.Stop)

	publish := func(worker, how string) (string, uint64) {
		t.Helper()
		subject := TaskStatusSubject(worker, "j1")
		mu.Lock()
		mode[subject] = how
		mu.Unlock()
		ack, err := c.js.Publish(ctx, subject, nil)
		if err != nil {
			t.Fatalf("publish %s: %v", subject, err)
		}
		return subject, ack.Sequence
	}
	floor := func() uint64 {
		t.Helper()
		f, err := c.ConsumerAckFloor(ctx, IngestTaskStatus)
		if err != nil {
			t.Fatalf("ConsumerAckFloor: %v", err)
		}
		return f
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s (ack floor %d)", what, floor())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// An acked message.
	_, acked := publish("acked", "ack")
	waitFor("the floor to pass an acked message", func() bool { return floor() >= acked })

	// A message nak'd on every delivery until it has none left: the floor
	// passes it once the next ack leaves nothing unacknowledged.
	exhaustedSubj, exhausted := publish("exhausted", "nak")
	waitFor("the last delivery", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return delivered[exhaustedSubj] >= TaskStatusConsumerMaxDeliver
	})
	// The broker gives up on it once the last nak lands, which leaves the
	// consumer nothing unacknowledged.
	waitFor("the broker to give up on the message", func() bool {
		info, err := c.js.Consumer(ctx, StreamTask, taskStatusConsumerName)
		if err != nil {
			t.Fatalf("Consumer: %v", err)
		}
		return info.CachedInfo().NumAckPending == 0
	})
	_, next := publish("next", "ack")
	waitFor("the floor to pass a message out of deliveries", func() bool { return floor() >= next })
	if f := floor(); f < exhausted {
		t.Fatalf("ack floor %d is below the message out of deliveries (%d)", f, exhausted)
	}

	// A message removed from the stream while delivered and unacknowledged.
	stream, err := c.js.Stream(ctx, StreamTask)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	removedSubj, removed := publish("removed", "hold")
	waitFor("the delivery", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return delivered[removedSubj] > 0
	})
	if err := stream.DeleteMsg(ctx, removed); err != nil {
		t.Fatalf("DeleteMsg: %v", err)
	}
	waitFor("the floor to pass a removed message", func() bool { return floor() >= removed })

	// A message delivered and not yet acknowledged keeps the floor below it,
	// even after a later message is acked.
	_, held := publish("held", "hold")
	_, after := publish("after", "ack")
	waitFor("the later message's ack", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return delivered[TaskStatusSubject("after", "j1")] > 0
	})
	time.Sleep(200 * time.Millisecond)
	if f := floor(); f >= held {
		t.Errorf("ack floor %d passed a message still to be delivered (%d; acked after it: %d)", f, held, after)
	}
}

// TestConsumerAckFloor_WorkerConsumer: IngestWorker reads the worker
// consumer's floor, not the task-status consumer's.
func TestConsumerAckFloor_WorkerConsumer(t *testing.T) {
	b := startBroker(t)
	c := newClient(t, b)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cc, err := c.ConsumeWorker(ctx, func(m jetstream.Msg) {
		_ = m.Ack() //nolint:errcheck // a lost ack fails the floor assertion
	})
	if err != nil {
		t.Fatalf("ConsumeWorker: %v", err)
	}
	t.Cleanup(cc.Stop)
	ack, err := c.js.Publish(ctx, WorkerHeartbeatSubject("w1"), nil)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		f, err := c.ConsumerAckFloor(ctx, IngestWorker)
		if err != nil {
			t.Fatalf("ConsumerAckFloor: %v", err)
		}
		if f >= ack.Sequence {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker ack floor %d never passed the acked heartbeat (%d)", f, ack.Sequence)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := c.ConsumerAckFloor(ctx, IngestTaskStatus); err == nil {
		t.Error("ConsumerAckFloor(IngestTaskStatus) with no task-status consumer: got no error")
	}
}
