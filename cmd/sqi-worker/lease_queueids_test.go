// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"testing"

	"github.com/uberware/sqi/internal/bus"
)

// TestLeaseConfig_CarriesTheProcessInstanceID pins the wiring between the
// registration and the lease loop: the lease configuration carries the instance
// ID it is given (start.go passes the Registrar's InstanceID, which is what
// every registration sends), so the lease requests and the registration name
// the same process.
func TestLeaseConfig_CarriesTheProcessInstanceID(t *testing.T) {
	got := leaseConfig([]string{"q1"}, "w-1", "inst-1")
	if got.InstanceID != "inst-1" || got.WorkerID != "w-1" || len(got.QueueIDs) != 1 || got.QueueIDs[0] != "q1" {
		t.Fatalf("leaseConfig = %+v, want worker w-1, instance inst-1, queues [q1]", got)
	}
	if wild := leaseConfig(nil, "w-1", "inst-1"); len(wild.QueueIDs) != 1 || wild.QueueIDs[0] != bus.WildcardQueueToken {
		t.Fatalf("leaseConfig(nil queues).QueueIDs = %v, want the wildcard token", wild.QueueIDs)
	}
}
