// SPDX-License-Identifier: AGPL-3.0-or-later

package verify

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/simworker"
)

// TestDecide_FollowsAssignmentOrder proves the decision comes from the
// attempt the server assigned last, not the lease that reached its worker
// last: after a server stall, a reclaimed attempt's reply can land after the
// next attempt's, and that stale attempt is canceled while the next succeeds.
func TestDecide_FollowsAssignmentOrder(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 7, 27, 58, 0, time.UTC)
	l := simworker.NewLedger()
	ev := func(at time.Duration, attempt string, assigned time.Duration, kind, outcome string) {
		e := simworker.Event{
			At: t0.Add(at), WorkerID: "w-" + attempt, TaskID: "task", AttemptID: attempt, JobID: "job",
			Kind: kind, Outcome: outcome,
		}
		if assigned >= 0 {
			e.AssignedAt = t0.Add(assigned)
		}
		l.Record(e)
	}
	// stale: assigned at +0.8s, reclaimed, its reply lands at +1.9s.
	// next: assigned at +0.9s, its reply lands at +1.5s; it succeeds.
	ev(1500*time.Millisecond, "next", 900*time.Millisecond, simworker.KindLeased, "")
	ev(1900*time.Millisecond, "stale", 800*time.Millisecond, simworker.KindLeased, "")
	ev(4*time.Second, "stale", 800*time.Millisecond, simworker.KindCanceled, "")
	ev(24*time.Second, "next", 900*time.Millisecond, simworker.KindTerminal, simworker.OutcomeSucceeded)

	d := indexLedger(l).decide("task")
	if !d.decided || d.status != simworker.OutcomeSucceeded || d.last.id != "next" {
		t.Fatalf("decision = %+v (%s), want the assigned-last attempt next, succeeded", d, d.why())
	}
}

// TestDecide_WithoutAssignmentTimesKeepsLeaseOrder proves an attempt whose
// message carried no assignment time keeps its place in lease order.
func TestDecide_WithoutAssignmentTimesKeepsLeaseOrder(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 7, 0, 0, 0, time.UTC)
	l := simworker.NewLedger()
	for i, a := range []string{"first", "second"} {
		at := t0.Add(time.Duration(i) * time.Second)
		l.Record(simworker.Event{At: at, TaskID: "task", AttemptID: a, JobID: "job", Kind: simworker.KindLeased})
	}
	l.Record(simworker.Event{At: t0.Add(5 * time.Second), TaskID: "task", AttemptID: "second", JobID: "job", Kind: simworker.KindTerminal, Outcome: simworker.OutcomeFailed})
	d := indexLedger(l).decide("task")
	if d.last == nil || d.last.id != "second" || d.status != simworker.OutcomeFailed {
		t.Fatalf("decision = %+v (%s), want the last leased attempt second, failed", d, d.why())
	}
}
