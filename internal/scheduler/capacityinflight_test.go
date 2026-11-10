// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// While a group's provider call runs, the sweep records no new waking or
// retiring member: the request could not be sent until the call ends, and a
// recorded transition starts its boot-timeout clock at once, so a slow call
// would expire members no request ever reached. The next sweep after the call
// records them.
func TestActuator_NoTransitionsWhileACallRuns(t *testing.T) {
	f := newActFixture(t, nil)
	release := make(chan struct{})
	asked := make(chan struct{}, 1)
	f.bus.setAnswer(func(string, protocol.ControlMsg) ([]byte, error) {
		asked <- struct{}{}
		<-release
		return controlReply(false, protocol.ControlRefusalBusy), nil
	})
	g := createGroup(t, f.st, wolGroup("lan", nil))
	addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), true)
	t0 := time.Now().UTC()

	// m1 idles past the idle timeout: its retire call starts and hangs.
	f.s.sweepCapacityAt(t.Context(), t0)
	f.s.sweepCapacityAt(t.Context(), t0.Add(2*time.Minute))
	select {
	case <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("no sleep was requested")
	}

	// Demand arrives for two machines while the call runs: m1, put back to
	// awake when the call ends, and a sleeping second member.
	addNamedMember(t, f.st, g.ID, lanWorker("m2", "192.168.1.22", "aa:bb:cc:dd:ee:02"), false)
	submitReady(t, f.st, 2, store.Step{})
	f.s.sweepCapacityAt(t.Context(), t0.Add(2*time.Minute+15*time.Second))
	if got := f.worker(t, "m2").PowerState; got == store.PowerWaking {
		t.Fatalf("m2 = %s while the group's call still runs, want it left for the next sweep", got)
	}

	close(release)
	f.s.capState.act.calls.Wait()
	f.sweep(t, t0.Add(2*time.Minute+30*time.Second))
	if got := f.worker(t, "m2").PowerState; got != store.PowerWaking {
		t.Errorf("m2 = %s after the call ended, want waking", got)
	}
}
