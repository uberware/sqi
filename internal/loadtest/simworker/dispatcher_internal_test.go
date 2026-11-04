// SPDX-License-Identifier: AGPL-3.0-or-later

package simworker

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/worker/logstreamer"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/status"
)

type countingConn struct {
	mu sync.Mutex
	n  int
}

func (c *countingConn) Publish(string, []byte) error {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return nil
}

func (c *countingConn) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func newInternalDispatcher(plan TaskPlan) (*Dispatcher, *countingConn, *Ledger) {
	conn := &countingConn{}
	logger := slog.New(slog.DiscardHandler)
	l := NewLedger()
	d := NewDispatcher("w1",
		status.New(conn, status.Config{WorkerID: "w1"}, logger),
		logstreamer.New(conn, "w1", logstreamer.DefaultConfig(), logger),
		nil,
		BehaviorFunc(func(*protocol.AssignMsg, *rand.Rand) TaskPlan { return plan }),
		l, rand.New(rand.NewPCG(1, 2)))
	return d, conn, l
}

func TestDispatcher_ShutdownLeavesNoGoroutines(t *testing.T) {
	tests := []struct {
		name     string
		reason   endReason
		wantKind string
		publish  bool // whether ending publishes a status
	}{
		{"crash", endCrashed, KindCrashed, false},
		{"graceful shutdown", endShutdown, KindTerminal, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseline := runtime.NumGoroutine()
			// A hang with lines left keeps a timer and the log publisher's
			// per-attempt flush goroutine alive.
			d, conn, l := newInternalDispatcher(TaskPlan{Duration: 10 * time.Second, Outcome: OutcomeHang, LogLines: 1000, LineBytes: 10})
			const n = 20
			for i := range n {
				id := string(rune('a' + i))
				if err := d.Dispatch(context.Background(), &protocol.AssignMsg{TaskID: "t" + id, AttemptID: "a" + id, JobID: "j"}); err != nil {
					t.Fatalf("Dispatch: %v", err)
				}
			}
			deadline := time.Now().Add(5 * time.Second)
			for countKindIn(l, KindRunning) < n && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			time.Sleep(50 * time.Millisecond) // let some lines out
			before := conn.count()

			d.shutdown(tt.reason)
			if !d.wait(t.Context()) {
				t.Fatal("wait returned before the task goroutines did")
			}
			if got := countKindIn(l, tt.wantKind); got != n {
				t.Errorf("%d %s events, want %d", got, tt.wantKind, n)
			}
			if len(d.ActiveAttemptIDs()) != 0 || d.ActiveTaskCount() != 0 {
				t.Errorf("held %v / active %d after shutdown, want none", d.ActiveAttemptIDs(), d.ActiveTaskCount())
			}
			// The crash publishes at most the lines its flush drained; the
			// shutdown adds one terminal status per task.
			if tt.publish && conn.count()-before < n {
				t.Errorf("shutdown published %d messages, want at least %d terminal statuses", conn.count()-before, n)
			}

			for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if g := runtime.NumGoroutine(); g > baseline {
				t.Errorf("%d goroutines after shutdown, baseline %d", g, baseline)
			}

			// A dispatch after shutdown is refused and recorded as abandoned.
			err := d.Dispatch(context.Background(), &protocol.AssignMsg{TaskID: "late", AttemptID: "late-a"})
			if !errors.Is(err, errClosed) {
				t.Errorf("Dispatch after shutdown = %v, want errClosed", err)
			}
			if countKindIn(l, KindCrashed) < 1 || len(d.ActiveAttemptIDs()) != 0 {
				t.Error("late dispatch was not recorded as crashed, or left an attempt held")
			}
		})
	}
}

func TestDispatcher_CancelBeatsAFinishingTask(t *testing.T) {
	d, _, _ := newInternalDispatcher(TaskPlan{})
	tk := &simTask{m: &protocol.AssignMsg{TaskID: "t1"}, stop: make(chan struct{})}
	d.active["t1"] = tk
	if got := d.settle(tk); got != endDone {
		t.Fatalf("settle = %v, want endDone", got)
	}
	if d.Cancel("t1") {
		t.Error("Cancel of a finishing task = true, want false")
	}
	tk2 := &simTask{m: &protocol.AssignMsg{TaskID: "t2"}, stop: make(chan struct{})}
	d.active["t2"] = tk2
	first, repeat := d.Cancel("t2"), d.Cancel("t2")
	if !first || !repeat {
		t.Error("Cancel, then a repeat Cancel, of a running task should both be true")
	}
	if got := d.settle(tk2); got != endCanceled {
		t.Errorf("settle after Cancel = %v, want endCanceled", got)
	}
}

func TestLinesDue(t *testing.T) {
	p := TaskPlan{Duration: time.Second, LogLines: 10}
	tests := []struct {
		name    string
		plan    TaskPlan
		elapsed time.Duration
		want    int
	}{
		{"start", p, 0, 0},
		{"half way", p, 500 * time.Millisecond, 5},
		{"end", p, time.Second, 10},
		{"past the end", p, 2 * time.Second, 10},
		{"no lines", TaskPlan{Duration: time.Second}, time.Second, 0},
		{"zero duration emits at once", TaskPlan{LogLines: 3}, 0, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := linesDue(tt.plan, tt.elapsed); got != tt.want {
				t.Errorf("linesDue = %d, want %d", got, tt.want)
			}
		})
	}
}

func countKindIn(l *Ledger, kind string) int {
	n := 0
	for _, e := range l.Events() {
		if e.Kind == kind {
			n++
		}
	}
	return n
}
