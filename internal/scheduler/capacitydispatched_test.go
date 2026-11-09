// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// TestActuator_DispatchedIntentsArePruned: a sent intent is remembered only
// while it can still be consulted. Once its member has moved on and two boot
// timeouts have passed, the record is dropped, so a long-lived leader does not
// keep one entry per transition it ever made.
func TestActuator_DispatchedIntentsArePruned(t *testing.T) {
	f := newActFixture(t, nil)
	g := createGroup(t, f.st, wolGroup("lan", nil))
	addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), false)
	t0 := time.Now().UTC()
	if moved, err := f.st.MarkWaking(t.Context(), g.ID, []string{"m1"}, t0); err != nil || len(moved) != 1 {
		t.Fatalf("MarkWaking = %v, %v", moved, err)
	}

	f.sweep(t, t0.Add(15*time.Second))
	if n := len(f.s.capState.act.dispatched); n != 1 {
		t.Fatalf("dispatched after the wake = %d entries, want 1", n)
	}

	// Past the boot timeout the wake fails; past twice it the record goes.
	f.sweep(t, t0.Add(g.BootTimeout+time.Second))
	if got := f.worker(t, "m1").PowerState; got != store.PowerWakeFailed {
		t.Fatalf("m1 = %s, want wake_failed", got)
	}
	f.sweep(t, t0.Add(2*g.BootTimeout+time.Minute))
	if n := len(f.s.capState.act.dispatched); n != 0 {
		t.Errorf("dispatched two boot timeouts on = %d entries, want 0", n)
	}
}

// TestActuatorState_PruneDispatched keeps exactly the records whose expiry
// has not passed.
func TestActuatorState_PruneDispatched(t *testing.T) {
	now := time.Now()
	live := intentKey{id: "live", intent: "waking"}
	due := intentKey{id: "due", intent: "waking"}
	past := intentKey{id: "past", intent: "retiring"}
	a := actuatorState{dispatched: map[intentKey]time.Time{
		live: now.Add(time.Second), due: now, past: now.Add(-time.Second),
	}}
	a.pruneDispatched(now)
	if _, ok := a.dispatched[live]; !ok || len(a.dispatched) != 2 {
		t.Errorf("after prune = %v, want live and due kept", a.dispatched)
	}
	if _, ok := a.dispatched[past]; ok {
		t.Errorf("after prune = %v, want past dropped", a.dispatched)
	}
}
