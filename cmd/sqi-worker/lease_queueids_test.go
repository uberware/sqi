// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"testing"

	"github.com/uberware/sqi/internal/bus"
)

// TestLeaseQueueIDs guards the queueless-worker regression: a worker with no
// configured queues must request leases on the wildcard token (a valid subject),
// not on an empty leaf that routes to no responder.
func TestLeaseQueueIDs(t *testing.T) {
	t.Run("empty -> wildcard token", func(t *testing.T) {
		got := leaseQueueIDs(nil)
		if len(got) != 1 || got[0] != bus.WildcardQueueToken {
			t.Fatalf("leaseQueueIDs(nil) = %v, want [%q]", got, bus.WildcardQueueToken)
		}
		// The resulting subject must be valid: a parseable worker → server
		// lease subject with a non-empty queue token.
		subj := bus.WorkLeaseSubject("w-1", got[0])
		workerID, queueID, ok := bus.ParseWorkerSubject(subj)
		if !ok || workerID != "w-1" || queueID != bus.WildcardQueueToken {
			t.Fatalf("wildcard produced invalid subject %q", subj)
		}
	})

	t.Run("configured queues pass through unchanged", func(t *testing.T) {
		in := []string{"q1", "q2"}
		got := leaseQueueIDs(in)
		if len(got) != 2 || got[0] != "q1" || got[1] != "q2" {
			t.Fatalf("leaseQueueIDs(%v) = %v, want unchanged", in, got)
		}
	})
}
