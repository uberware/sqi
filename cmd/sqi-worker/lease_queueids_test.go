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

// stubIdentity is a processIdentity with a fixed instance ID.
type stubIdentity string

func (s stubIdentity) InstanceID() string { return string(s) }

// TestLeaseConfig_CarriesTheProcessInstanceID pins the wiring between the
// registration and the lease loop: the lease configuration takes its instance
// ID from the process identity it is given (start.go passes the Registrar,
// whose InstanceID is what every registration sends), so the lease requests
// and the registration name the same process (H4a2 §4.5).
func TestLeaseConfig_CarriesTheProcessInstanceID(t *testing.T) {
	got := leaseConfig([]string{"q1"}, "w-1", stubIdentity("inst-1"))
	if got.InstanceID != "inst-1" || got.WorkerID != "w-1" || len(got.QueueIDs) != 1 || got.QueueIDs[0] != "q1" {
		t.Fatalf("leaseConfig = %+v, want worker w-1, instance inst-1, queues [q1]", got)
	}
	if wild := leaseConfig(nil, "w-1", stubIdentity("inst-1")); len(wild.QueueIDs) != 1 || wild.QueueIDs[0] != bus.WildcardQueueToken {
		t.Fatalf("leaseConfig(nil queues).QueueIDs = %v, want the wildcard token", wild.QueueIDs)
	}
}
