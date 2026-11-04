// SPDX-License-Identifier: AGPL-3.0-or-later

package simworker

import (
	"cmp"
	"slices"
	"sync"
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
}

// Events returns a copy of every event in record order.
func (l *Ledger) Events() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

// LogLines returns how many log lines the simulated workers emitted in the
// attempts that have ended so far (by a terminal, canceled or crashed event):
// the sum of those events' LogLines.
func (l *Ledger) LogLines() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var n int64
	for i := range l.events {
		n += int64(l.events[i].LogLines)
	}
	return n
}

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

// ConcurrentPeak returns, per key, the most attempts that were in flight at
// once. An attempt is in flight from its leased event to its terminal,
// canceled or crashed event (or to the end of the ledger when it has none), and
// is counted under key(its leased event). An attempt that ends at the instant
// another starts does not overlap it.
//
// The window is the worker's view, which sits inside the server's: the server
// commits the lease before the worker records leased, and a dispatcher stamps
// an attempt's end event with a time taken before it publishes the terminal
// status, so before the server can free the slot. A peak above a
// server-enforced cap is therefore a real violation, not an artifact of
// measuring. That holds for events the dispatcher records; an event recorded
// with At left zero is stamped when Record runs.
func (l *Ledger) ConcurrentPeak(key func(Event) string) map[string]int {
	type edge struct {
		at    time.Time
		key   string
		delta int
	}
	starts := map[string]Event{}
	ended := map[string]bool{}
	var edges []edge
	for _, e := range l.Events() {
		switch e.Kind {
		case KindLeased:
			if _, dup := starts[e.AttemptID]; !dup {
				starts[e.AttemptID] = e
				edges = append(edges, edge{at: e.At, key: key(e), delta: 1})
			}
		case KindTerminal, KindCanceled, KindCrashed:
			s, ok := starts[e.AttemptID]
			if ok && !ended[e.AttemptID] {
				ended[e.AttemptID] = true
				edges = append(edges, edge{at: e.At, key: key(s), delta: -1})
			}
		}
	}
	// Ends sort before starts at the same instant.
	slices.SortStableFunc(edges, func(a, b edge) int {
		return cmp.Or(a.at.Compare(b.at), cmp.Compare(a.delta, b.delta))
	})
	cur := map[string]int{}
	peak := map[string]int{}
	for _, e := range edges {
		cur[e.key] += e.delta
		peak[e.key] = max(peak[e.key], cur[e.key])
	}
	return peak
}
