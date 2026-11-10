// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// The sweep evaluates a step once, not once per task: tasks of one step and
// core count share the job, the step, the pools and so the answer. A sleeping
// wake-on-LAN farm with a backlog, the common case for a waitable group, makes
// one job and one step read per sweep, however many tasks wait.
func TestUnschedulable_EvaluatesEachStepOnce(t *testing.T) {
	const n = 40
	for _, online := range []bool{false, true} {
		name := "no online workers"
		if online {
			name = "an ineligible worker"
		}
		t.Run(name, func(t *testing.T) {
			var spy *spyStore
			f := newActFixture(t, func(st *fake.Store) store.Store {
				spy = &spyStore{Store: st, calls: map[string]int{}}
				return spy
			})
			f.s.cfg.UnschedulableGrace = time.Millisecond
			g := createGroup(t, f.st, wolGroup("lan", nil))
			addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), false)
			tasks := submitReady(t, f.st, n, store.Step{})
			for _, task := range tasks {
				backdateTaskReady(t, f.st, task.ID, -time.Minute)
			}
			if online {
				registerOnline(t, f.st, store.Worker{ID: "busy", FarmID: capFarm, CPUCount: 4, QueueID: "elsewhere"})
			}
			f.sweep(t, time.Now().UTC())
			spy.mu.Lock()
			spy.calls = map[string]int{}
			spy.mu.Unlock()

			f.s.sweepUnschedulable(t.Context())

			spy.mu.Lock()
			calls := spy.calls
			spy.mu.Unlock()
			for _, m := range []string{"GetJob", "GetStep"} {
				if calls[m] != 1 {
					t.Errorf("%s calls = %d for %d tasks of one step, want 1", m, calls[m], n)
				}
			}
			if calls["SetTaskUnschedulableReason"] != n {
				t.Errorf("SetTaskUnschedulableReason calls = %d, want %d", calls["SetTaskUnschedulableReason"], n)
			}
			for _, task := range tasks {
				got, err := f.st.GetTask(t.Context(), task.ID)
				if err != nil {
					t.Fatalf("GetTask: %v", err)
				}
				if got.UnschedulableReason != waitWakeLan {
					t.Errorf("task %s reason = %q, want %q", task.ID, got.UnschedulableReason, waitWakeLan)
				}
			}
		})
	}
}
