// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for the "waiting for capacity group" unschedulable reason
// (unschedulable.go, capacity.go): a ready task that no online worker can run
// is reported as waiting for the first capacity group that acts and could run
// it, from the capacity sweep's last tick and memory alone.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

const (
	waitWakeLan    = "waiting for capacity group lan to wake a machine"
	waitLaunchCld  = "waiting for capacity group cloud to launch a machine"
	reasonNoOnline = "no online workers"
)

// ListReadyTasks and SetTaskUnschedulableReason are the unschedulable sweep's
// own calls, counted along with the ones spyStore already counts.
func (s *spyStore) ListReadyTasks(ctx context.Context, farmID string, now time.Time, limit int) ([]store.Task, error) {
	s.count("ListReadyTasks")
	return s.Store.ListReadyTasks(ctx, farmID, now, limit)
}

func (s *spyStore) SetTaskUnschedulableReason(ctx context.Context, taskID, reason string) (bool, error) {
	s.count("SetTaskUnschedulableReason")
	return s.Store.SetTaskUnschedulableReason(ctx, taskID, reason)
}

// A named wake_on_lan group with a member it is waking (or can wake) that could
// run the step is what the task waits for.
func TestUnschedulable_WaitingForWake(t *testing.T) {
	f := newActFixture(t, nil)
	g := createGroup(t, f.st, wolGroup("lan", nil))
	addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), false)
	tasks := submitReady(t, f.st, 1, store.Step{})
	f.sweep(t, time.Now().UTC())
	if got := f.worker(t, "m1").PowerState; got != store.PowerWaking {
		t.Fatalf("m1 = %s, want waking", got)
	}

	if got := f.s.evaluateSchedulability(t.Context(), tasks[0], nil); got != waitWakeLan {
		t.Errorf("reason with no workers = %q, want %q", got, waitWakeLan)
	}

	// It replaces the old reason when workers are online but none can run it.
	registerOnline(t, f.st, store.Worker{ID: "busy", FarmID: capFarm, CPUCount: 4, QueueID: "elsewhere"})
	online, err := f.s.onlineWorkers(t.Context())
	if err != nil {
		t.Fatalf("onlineWorkers: %v", err)
	}
	if got := f.s.evaluateSchedulability(t.Context(), tasks[0], online); got != waitWakeLan {
		t.Errorf("reason with an ineligible worker = %q, want %q", got, waitWakeLan)
	}
}

// The reason is the same for a member that is only wakeable, and a member the
// step cannot run on, or one needing more cores than it has, is not waited for.
func TestUnschedulable_WaitableMembers(t *testing.T) {
	asleep := store.Worker{
		ID: "m1", CPUCount: 4, OS: "linux", Arch: "amd64",
		Status: store.WorkerStatusOffline, PowerState: store.PowerAsleep,
	}
	tests := []struct {
		name  string
		g     waitableGroup
		step  store.Step
		cores int
		want  string
	}{
		{"waking", waitableGroup{name: "lan", kind: waitWake, waking: []store.Worker{asleep}}, store.Step{}, 0, waitWakeLan},
		{"wakeable", waitableGroup{name: "lan", kind: waitWake, wakeable: []store.Worker{asleep}}, store.Step{}, 0, waitWakeLan},
		{
			"step needs a tag the member lacks",
			waitableGroup{name: "lan", kind: waitWake, wakeable: []store.Worker{asleep}},
			houdiniStep(), 0, "",
		},
		{
			"step needs more cores than the member has",
			waitableGroup{name: "lan", kind: waitWake, wakeable: []store.Worker{asleep}},
			store.Step{},
			8, "",
		},
		{"no machines", waitableGroup{name: "lan", kind: waitWake}, store.Step{}, 0, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, s, _ := newCapacityFixture(t, nil)
			s.capState.waitable = []waitableGroup{tc.g}
			job := store.Job{ID: "j", FarmID: capFarm, QueueID: capQueue}
			if got := s.capacityWaitReason(job, tc.step, tc.cores, nil); got != tc.want {
				t.Errorf("reason = %q, want %q", got, tc.want)
			}
		})
	}
}

// The first group, in the sweep's order, that could run the step wins.
func TestUnschedulable_FirstWaitableGroupWins(t *testing.T) {
	_, s, _ := newCapacityFixture(t, nil)
	m := store.Worker{ID: "m1", CPUCount: 4, OS: "linux", Arch: "amd64", PowerState: store.PowerAsleep}
	tagged := m
	tagged.Tags = map[string]string{"houdini": "20.5"}
	s.capState.waitable = []waitableGroup{
		{name: "first", kind: waitWake, wakeable: []store.Worker{m}},
		{name: "second", kind: waitLaunch, profile: tagged},
		{name: "third", kind: waitLaunch, profile: tagged},
	}
	job := store.Job{ID: "j", FarmID: capFarm, QueueID: capQueue}

	if got, want := s.capacityWaitReason(job, store.Step{}, 0, nil), "waiting for capacity group first to wake a machine"; got != want {
		t.Errorf("plain step = %q, want %q", got, want)
	}
	if got, want := s.capacityWaitReason(job, houdiniStep(), 0, nil), "waiting for capacity group second to launch a machine"; got != want {
		t.Errorf("houdini step = %q, want %q", got, want)
	}
}

// A sized script group is what the task waits for, to launch a machine, unless
// it is degraded and backing off.
func TestUnschedulable_WaitingForLaunch(t *testing.T) {
	f := newScriptFixture(t, func(g *store.CapacityGroup) { g.Name = "cloud" })
	f.prov.set("list", actionResult{Result: resultOK})
	tasks := submitReady(t, f.st, 2, store.Step{})
	t0 := time.Now().UTC()

	f.sweep(t, t0)
	if got := f.s.evaluateSchedulability(t.Context(), tasks[0], nil); got != waitLaunchCld {
		t.Errorf("reason = %q, want %q", got, waitLaunchCld)
	}

	// The machines never register: the group degrades and backs off, so
	// nothing is coming.
	f.sweep(t, t0.Add(5*time.Minute+time.Second))
	if got := groupState(t, f.st, f.g.ID); got.DegradedReason == "" {
		t.Fatalf("group is not degraded: %+v", got)
	}
	if got := f.s.evaluateSchedulability(t.Context(), tasks[0], nil); got != reasonNoOnline {
		t.Errorf("reason while degraded = %q, want %q", got, reasonNoOnline)
	}

	// Once the backoff ends, it is launching again.
	f.sweep(t, t0.Add(5*time.Minute+time.Second+2*5*time.Minute+time.Second))
	if got := f.s.evaluateSchedulability(t.Context(), tasks[0], nil); got != waitLaunchCld {
		t.Errorf("reason after the backoff = %q, want %q", got, waitLaunchCld)
	}
}

// A group whose profile cannot run the step is not waited for.
func TestUnschedulable_IncapableGroupKeepsOldReason(t *testing.T) {
	f := newScriptFixture(t, func(g *store.CapacityGroup) { g.Name = "cloud" })
	tasks := submitReady(t, f.st, 1, houdiniStep())
	f.sweep(t, time.Now().UTC())

	if got := f.s.evaluateSchedulability(t.Context(), tasks[0], nil); got != reasonNoOnline {
		t.Errorf("reason with no workers = %q, want %q", got, reasonNoOnline)
	}

	registerOnline(t, f.st, store.Worker{ID: "w1", FarmID: capFarm, CPUCount: 4, OS: "linux", Arch: "amd64"})
	online, err := f.s.onlineWorkers(t.Context())
	if err != nil {
		t.Fatalf("onlineWorkers: %v", err)
	}
	got := f.s.evaluateSchedulability(t.Context(), tasks[0], online)
	if !strings.HasPrefix(got, "no eligible online worker: ") {
		t.Errorf("reason with an ineligible worker = %q, want the no eligible online worker reason", got)
	}
}

// With no online worker, a read the capacity wait needs that fails keeps the
// reason the task would have had without acting groups, rather than reading
// as schedulable and clearing it, so a passing store error does not flip it.
func TestUnschedulable_NoOnlineWorkersWhenAReadFails(t *testing.T) {
	failing := false
	f := newActFixture(t, func(st *fake.Store) store.Store {
		return readSwitch{Store: st, failing: &failing}
	})
	g := createGroup(t, f.st, wolGroup("lan", nil))
	addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), false)
	tasks := submitReady(t, f.st, 1, store.Step{})
	f.sweep(t, time.Now().UTC())

	failing = true
	if got := f.s.evaluateSchedulability(t.Context(), tasks[0], nil); got != reasonNoOnline {
		t.Errorf("reason with a failed read = %q, want %q", got, reasonNoOnline)
	}
}

// readSwitch fails job reads while *failing is set, as a store that cannot be
// reached does.
type readSwitch struct {
	store.Store

	failing *bool
}

func (r readSwitch) GetJob(ctx context.Context, id string) (store.Job, error) {
	if *r.failing {
		return store.Job{}, errors.New("store unavailable")
	}
	return r.Store.GetJob(ctx, id)
}

// A group whose provider does not act (external, or a script this server does
// not configure) brings nothing, so the old reason stays.
func TestUnschedulable_ExternalGroupKeepsOldReason(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*store.CapacityGroup)
	}{
		{"external", nil},
		{"script not configured", func(g *store.CapacityGroup) { g.Provider = "script:missing" }},
		{"disabled", func(g *store.CapacityGroup) { g.Provider = "script:cloud"; g.Enabled = false }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newActFixture(t, nil)
			createGroup(t, f.st, sizedGroup("cloud", tc.mut))
			tasks := submitReady(t, f.st, 1, store.Step{})
			f.sweep(t, time.Now().UTC())

			if got := f.s.evaluateSchedulability(t.Context(), tasks[0], nil); got != reasonNoOnline {
				t.Errorf("reason = %q, want %q", got, reasonNoOnline)
			}
		})
	}
}

// Disabling the only group empties what the task waits for.
func TestUnschedulable_WaitableClearedWithGroups(t *testing.T) {
	f := newActFixture(t, nil)
	g := createGroup(t, f.st, wolGroup("lan", nil))
	addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), false)
	tasks := submitReady(t, f.st, 1, store.Step{})
	f.sweep(t, time.Now().UTC())
	if got := f.s.evaluateSchedulability(t.Context(), tasks[0], nil); got != waitWakeLan {
		t.Fatalf("reason = %q, want %q", got, waitWakeLan)
	}

	g.Enabled = false
	if _, err := f.st.UpdateCapacityGroup(t.Context(), g); err != nil {
		t.Fatalf("UpdateCapacityGroup: %v", err)
	}
	f.sweep(t, time.Now().UTC())

	if got := f.s.evaluateSchedulability(t.Context(), tasks[0], nil); got != reasonNoOnline {
		t.Errorf("reason after the group was disabled = %q, want %q", got, reasonNoOnline)
	}
}

// With no capacity group the unschedulable sweep makes exactly the calls it
// always has: the capacity view is memory only.
func TestUnschedulable_NoGroupsUnchanged(t *testing.T) {
	tests := []struct {
		name   string
		online bool
		want   map[string]int
	}{
		{"no online workers", false, map[string]int{
			"ListReadyTasks": 1, "ListWorkers": 1, "SetTaskUnschedulableReason": 1,
		}},
		{"an ineligible worker", true, map[string]int{
			"ListReadyTasks": 1, "ListWorkers": 1, "GetJob": 1, "GetStep": 1, "SetTaskUnschedulableReason": 1,
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var spy *spyStore
			st, s, _ := newCapacityFixture(t, func(st *fake.Store) store.Store {
				spy = &spyStore{Store: st, calls: map[string]int{}}
				return spy
			})
			s.cfg.UnschedulableGrace = time.Millisecond
			tasks := submitReady(t, st, 1, houdiniStep())
			backdateTaskReady(t, st, tasks[0].ID, -time.Minute)
			if tc.online {
				registerOnline(t, st, store.Worker{ID: "w1", FarmID: capFarm, CPUCount: 4})
			}
			s.sweepCapacityAt(t.Context(), time.Now().UTC())
			spy.mu.Lock()
			spy.calls = map[string]int{}
			spy.mu.Unlock()

			s.sweepUnschedulable(t.Context())

			spy.mu.Lock()
			defer spy.mu.Unlock()
			for name, want := range tc.want {
				if got := spy.calls[name]; got != want {
					t.Errorf("%s calls = %d, want %d", name, got, want)
				}
			}
			for name, got := range spy.calls {
				if _, ok := tc.want[name]; !ok {
					t.Errorf("unexpected %s calls = %d", name, got)
				}
			}
			got, err := st.GetTask(t.Context(), tasks[0].ID)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if got.UnschedulableReason == "" {
				t.Error("the task was not flagged")
			}
		})
	}
}
