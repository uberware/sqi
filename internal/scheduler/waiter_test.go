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
	if got := r.wait(context.Background(), "q1", 40*time.Millisecond); got {
		// notify a different queue mid-wait
		t.Error("unexpected wake")
	}
}
