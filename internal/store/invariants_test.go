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
				tasks: []store.TaskStatus{store.TaskStatusRunning, store.TaskStatusSucceeded},
			})
			pool := seedPool(t, st, 0)

			live := seedAttempt(t, st, g.Tasks["a"][0], store.AttemptStatusRunning)
			seedClaim(t, st, pool.ID, live.ID)
			if v := claimViolations(t, st); len(v) != 0 {
				t.Fatalf("healthy claim reported as violation: %v", v)
			}

			dead := seedAttempt(t, st, g.Tasks["a"][1], store.AttemptStatusSucceeded)
			seedClaim(t, st, pool.ID, dead.ID)
			if v := claimViolations(t, st); len(v) != 1 {
				t.Fatalf("violations = %v, want exactly the claim on the closed attempt", v)
			}
		})
	}
}
