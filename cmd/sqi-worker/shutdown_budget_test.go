// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"testing"

	"github.com/uberware/sqi/internal/winsvc"
	"github.com/uberware/sqi/internal/worker/registration"
	"github.com/uberware/sqi/internal/worker/status"
)

// What a worker does after its drain bound ([workerDrainBound]) must fit in
// winsvc.ShutdownMargin, or Windows ends a service still flushing its tasks'
// final statuses: the inline try of a terminal status handed over as the
// kill window ends, the wait for the statuses still pending, and the
// deregistration's flush.
func TestShutdownAfterDrainFitsServiceMargin(t *testing.T) {
	after := status.DefaultAckTimeout + status.DefaultFlushTimeout + registration.DeregisterFlushTimeout
	if after > winsvc.ShutdownMargin {
		t.Errorf("shutdown after the drain takes up to %v (ack %v + status flush %v + deregister flush %v), "+
			"want at most winsvc.ShutdownMargin %v",
			after, status.DefaultAckTimeout, status.DefaultFlushTimeout, registration.DeregisterFlushTimeout, winsvc.ShutdownMargin)
	}
}
