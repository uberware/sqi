// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"testing"
	"time"
)

func TestWaiterRegistry_WakeOnNotify(t *testing.T) {
	r := newWaiterRegistry()
	woke := make(chan bool, 1)
	go func() { woke <- r.wait(context.Background(), "q1", time.Second) }()

	time.Sleep(20 * time.Millisecond) // let the goroutine park
	r.notify("q1")

	select {
	case got := <-woke:
		if !got {
			t.Error("wait returned false, want true (woken by notify)")
		}
	case <-time.After(time.Second):
		t.Fatal("wait did not return after notify")
	}
}

func TestWaiterRegistry_Timeout(t *testing.T) {
	r := newWaiterRegistry()
	if r.wait(context.Background(), "q1", 30*time.Millisecond) {
		t.Error("wait returned true, want false (timeout)")
	}
}

func TestWaiterRegistry_NotifyOtherQueueDoesNotWake(t *testing.T) {
	r := newWaiterRegistry()
	woke := make(chan bool, 1)
	go func() { woke <- r.wait(context.Background(), "q1", 60*time.Millisecond) }()

	time.Sleep(20 * time.Millisecond) // let the goroutine park on q1
	r.notify("q2")                    // wake a DIFFERENT queue — q1 must stay parked

	select {
	case got := <-woke:
		if got {
			t.Error("q1 waiter woke on notify(q2); want false (cross-queue isolation)")
		}
		// got == false means it returned via timeout, which is correct.
	case <-time.After(time.Second):
		t.Fatal("wait did not return")
	}
}

func TestWaiterRegistry_NotifyAllWakesEveryQueue(t *testing.T) {
	r := newWaiterRegistry()
	q1 := make(chan bool, 1)
	q2 := make(chan bool, 1)
	go func() { q1 <- r.wait(context.Background(), "q1", time.Second) }()
	go func() { q2 <- r.wait(context.Background(), "q2", time.Second) }()

	time.Sleep(20 * time.Millisecond) // let both goroutines park
	r.notifyAll()

	for name, ch := range map[string]chan bool{"q1": q1, "q2": q2} {
		select {
		case got := <-ch:
			if !got {
				t.Errorf("%s waiter returned false, want true (woken by notifyAll)", name)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s waiter did not return after notifyAll", name)
		}
	}
}
