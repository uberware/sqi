// SPDX-License-Identifier: AGPL-3.0-or-later

package simworker

import (
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Ledger event kinds.
const (
	// KindLeased is recorded when an assignment reaches the dispatcher.
	KindLeased = "leased"
	// KindRunning is recorded after the running status is published.
	KindRunning = "running"
	// KindTerminal is recorded after a succeeded or failed status is
	// published. Outcome names which, or "worker_shutdown" for an attempt a
	// graceful Stop gave up on.
	KindTerminal = "terminal"
	// KindCanceled is recorded after a canceled status is published.
	KindCanceled = "canceled"
	// KindCrashed is recorded for an attempt the worker abandoned without
	// publishing a terminal status: a Crash, or a Restart.
	KindCrashed = "crashed"
)

// Event is one thing a simulated worker did with one attempt.
type Event struct {
	At         time.Time
	WorkerID   string
	InstanceID string
	TaskID     string
	AttemptID  string
	JobID      string
	// AssignedAt is the server's time for the attempt's assignment, from its
	// assignment message; zero when the message carried none.
	AssignedAt time.Time
	Kind       string
	// Outcome is set on terminal events.
	Outcome string
	// LogLines is how many lines the attempt emitted, set on terminal,
	// canceled and crashed events.
	LogLines int
}

// Ledger is the record of what every simulated worker did, shared across the
// fleet and safe for concurrent use. The verifier compares it with what the
// server reports.
type Ledger struct {
	mu     sync.Mutex
	events []Event
	// logLines is the sum of every recorded event's LogLines.
	logLines atomic.Int64
}

// NewLedger returns an empty ledger.
func NewLedger() *Ledger { return &Ledger{} }

// Record appends e, stamping At with the current time when it is zero.
func (l *Ledger) Record(e Event) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	l.mu.Lock()
	l.events = append(l.events, e)
	l.mu.Unlock()
	l.logLines.Add(int64(e.LogLines))
}

// Events returns a copy of every event in record order.
func (l *Ledger) Events() []Event { return l.EventsSince(0) }

// EventsSince returns a copy of the events recorded after the first n, in
// record order. The ledger only grows, so a reader that has seen n events
// reads only the new ones.
func (l *Ledger) EventsSince(n int) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events[min(n, len(l.events)):])
}

// LogLines returns how many log lines the simulated workers emitted in the
// attempts that have ended so far (by a terminal, canceled or crashed event):
// the sum of those events' LogLines.
func (l *Ledger) LogLines() int64 { return l.logLines.Load() }

// SetOutcome rewrites the outcome of the last terminal event recorded for
// taskID and reports whether there was one. It is a test hook: it lets a test
// make the ledger disagree with the server on purpose.
func (l *Ledger) SetOutcome(taskID, outcome string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.events) - 1; i >= 0; i-- {
		if l.events[i].TaskID == taskID && l.events[i].Kind == KindTerminal {
			l.events[i].Outcome = outcome
			return true
		}
	}
	return false
}
