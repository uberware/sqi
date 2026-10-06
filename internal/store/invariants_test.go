// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"testing"

	"github.com/uberware/sqi/internal/store"
)

// TestClaimInvariantViolations pins the I3 checker itself: an active claim on
// a running attempt of an in-flight task is fine; an active claim on a closed
// attempt is a violation.
func TestClaimInvariantViolations(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{}, stepSpec{
				name: "a", status: store.StepStatusReady,
				tasks: []store.TaskStatus{store.TaskStatusReady, store.TaskStatusRunning},
			})
			pool := seedPool(t, st, 0)

			leaseClaiming(t, st, g.Tasks["a"][0].ID, true, pool)
			if v := claimViolations(t, st); len(v) != 0 {
				t.Fatalf("healthy claim reported as violation: %v", v)
			}

			// The second task's attempt closes through production; the claim
			// left active on it cannot, so it is injected.
			done := g.Tasks["a"][1]
			dead := g.Attempts[done.ID]
			if _, err := st.CompleteTaskAttempt(t.Context(), completion(done, dead, store.TaskStatusSucceeded, store.AttemptStatusSucceeded)); err != nil {
				t.Fatalf("CompleteTaskAttempt: %v", err)
			}
			seedClaim(t, st, pool.ID, dead.ID)
			if v := claimViolations(t, st); len(v) != 1 {
				t.Fatalf("violations = %v, want exactly the claim on the closed attempt", v)
			}
		})
	}
}
