// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
)

// registerWith registers worker id on farmID with the given interfaces and
// cloud instance ID.
func registerWith(t *testing.T, st store.Store, id, farmID string, ifaces []store.NetInterface, cloudID string) store.Worker {
	t.Helper()
	now := time.Now().UTC()
	w, _, err := st.RegisterWorker(t.Context(), store.Worker{
		ID: id, FarmID: farmID, Hostname: id, Status: store.WorkerStatusOnline, LastHeartbeatAt: &now,
		Interfaces: ifaces, CloudInstanceID: cloudID,
	})
	if err != nil {
		t.Fatalf("RegisterWorker %s: %v", id, err)
	}
	return w
}

func TestParity_RegisterWorkerPowerTransitions(t *testing.T) {
	ifaces := []store.NetInterface{
		{Name: "eth0", MAC: "aa:bb:cc:dd:ee:01", Addrs: []string{"192.168.1.20/24", "fe80::1/64"}},
		{Name: "eth1", MAC: "aa:bb:cc:dd:ee:02"},
	}
	for _, tc := range []struct {
		name         string
		before, want store.PowerState
		// changes is whether the registration moves the state, and so stamps
		// its time and clears its reason.
		changes bool
	}{
		{"awake", store.PowerAwake, store.PowerAwake, false},
		{"waking", store.PowerWaking, store.PowerAwake, true},
		{"asleep", store.PowerAsleep, store.PowerAwake, true},
		{"wake_failed", store.PowerWakeFailed, store.PowerAwake, true},
		{"retiring", store.PowerRetiring, store.PowerRetiring, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for name, st := range newStores(t) {
				t.Run(name, func(t *testing.T) {
					farm := mustCreateFarm(t, st, "main")
					first := registerWith(t, st, "w-1", farm.ID, nil, "")
					if first.PowerState != store.PowerAwake || first.PowerStateAt != nil || first.PowerReason != "" {
						t.Fatalf("new worker power = (%q, %v, %q), want awake with no time or reason",
							first.PowerState, first.PowerStateAt, first.PowerReason)
					}

					set := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
					if tc.before != store.PowerAwake {
						if err := storetest.InjectorFor(t, st).InjectWorkerPowerState(t.Context(), "w-1", tc.before, set, "because"); err != nil {
							t.Fatalf("InjectWorkerPowerState: %v", err)
						}
					}

					start := time.Now().UTC().Add(-time.Second)
					got := registerWith(t, st, "w-1", farm.ID, ifaces, "i-0abc123")
					if got.PowerState != tc.want {
						t.Fatalf("power state after registering from %s = %q, want %q", tc.before, got.PowerState, tc.want)
					}
					switch {
					case tc.changes:
						if got.PowerStateAt == nil || got.PowerStateAt.Before(start) || got.PowerReason != "" {
							t.Fatalf("power (at %v, reason %q), want a time since %v and no reason", got.PowerStateAt, got.PowerReason, start)
						}
					case tc.before == store.PowerAwake:
						if got.PowerStateAt != nil || got.PowerReason != "" {
							t.Fatalf("power (at %v, reason %q), want an awake worker's left alone", got.PowerStateAt, got.PowerReason)
						}
					default:
						if got.PowerStateAt == nil || !got.PowerStateAt.Equal(set) || got.PowerReason != "because" {
							t.Fatalf("power (at %v, reason %q), want (%v, \"because\") kept", got.PowerStateAt, got.PowerReason, set)
						}
					}
					if !reflect.DeepEqual(got.Interfaces, ifaces) || got.CloudInstanceID != "i-0abc123" {
						t.Fatalf("interfaces %+v cloud %q, want %+v and i-0abc123", got.Interfaces, got.CloudInstanceID, ifaces)
					}
					if stored := mustWorker(t, st, "w-1"); !reflect.DeepEqual(stored, got) {
						t.Fatalf("stored worker = %+v, want what RegisterWorker returned, %+v", stored, got)
					}

					// A later registration reporting none keeps what was stored:
					// one failed interface or metadata read must not erase the
					// only record of the machine's wake address and instance.
					kept := registerWith(t, st, "w-1", farm.ID, nil, "")
					if !reflect.DeepEqual(kept.Interfaces, ifaces) || kept.CloudInstanceID != "i-0abc123" {
						t.Fatalf("after a registration with none, interfaces %+v cloud %q, want %+v and i-0abc123 kept",
							kept.Interfaces, kept.CloudInstanceID, ifaces)
					}
					kept = registerWith(t, st, "w-1", farm.ID, []store.NetInterface{}, "")
					if !reflect.DeepEqual(kept.Interfaces, ifaces) {
						t.Fatalf("after a registration with an empty list, interfaces %+v, want %+v kept", kept.Interfaces, ifaces)
					}

					// A non-empty report replaces what was stored.
					other := []store.NetInterface{{Name: "en0", MAC: "aa:bb:cc:dd:ee:09", Addrs: []string{"10.0.0.9/8"}}}
					replaced := registerWith(t, st, "w-1", farm.ID, other, "i-0def456")
					if !reflect.DeepEqual(replaced.Interfaces, other) || replaced.CloudInstanceID != "i-0def456" {
						t.Fatalf("after a registration with new values, interfaces %+v cloud %q, want %+v and i-0def456",
							replaced.Interfaces, replaced.CloudInstanceID, other)
					}
				})
			}
		})
	}
}

// TestParity_LeaseRefusesRetiringAndAsleep: a retiring or asleep worker is
// refused a lease, which writes nothing; every other power state leases.
func TestParity_LeaseRefusesRetiringAndAsleep(t *testing.T) {
	for _, tc := range []struct {
		state   store.PowerState
		refused bool
	}{
		{store.PowerAwake, false},
		{store.PowerWaking, false},
		{store.PowerRetiring, true},
		{store.PowerAsleep, true},
		{store.PowerWakeFailed, false},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			for name, st := range newStores(t) {
				t.Run(name, func(t *testing.T) {
					g := seedStepA(t, st, graphOpts{}, store.TaskStatusReady)
					registerWith(t, st, "w-p", g.Farm.ID, nil, "")
					if tc.state != store.PowerAwake {
						storetest.InjectWorkerPower(t, st, "w-p", tc.state, "")
					}
					task := g.Tasks["a"][0]
					req := leaseReq(task)
					req.WorkerID = "w-p"
					if tc.refused {
						mustLease(t, st, req, store.LeaseLost)
						assertNothingWritten(t, st, task)
						return
					}
					mustLease(t, st, req, store.LeaseLeased)
					if got := mustTask(t, st, task.ID); got.Status != store.TaskStatusAssigned || got.AssignedWorkerID != "w-p" {
						t.Fatalf("task = %s on %q, want assigned on w-p", got.Status, got.AssignedWorkerID)
					}
				})
			}
		})
	}
}

// TestParity_LeavingGroupResetsPowerState: a worker leaving its capacity group
// is awake again, with no reason; joining or staying leaves the state alone.
func TestParity_LeavingGroupResetsPowerState(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			farm := mustCreateFarm(t, st, "main")
			group := mustCreateCapacityGroup(t, st, mkCapacityGroup(farm.ID, "burst"))
			registerWith(t, st, "w-1", farm.ID, nil, "")

			if _, err := st.SetWorkerCapacityGroup(t.Context(), "w-1", group.ID, true); err != nil {
				t.Fatalf("join: %v", err)
			}
			at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
			if err := storetest.InjectorFor(t, st).InjectWorkerPowerState(t.Context(), "w-1", store.PowerAsleep, at, "idle"); err != nil {
				t.Fatalf("InjectWorkerPowerState: %v", err)
			}

			rejoined, err := st.SetWorkerCapacityGroup(t.Context(), "w-1", group.ID, true)
			if err != nil {
				t.Fatalf("stay: %v", err)
			}
			if rejoined.PowerState != store.PowerAsleep || rejoined.PowerReason != "idle" {
				t.Fatalf("staying in the group: power (%q, %q), want (asleep, idle) unchanged", rejoined.PowerState, rejoined.PowerReason)
			}

			start := time.Now().UTC().Add(-time.Second)
			left, err := st.SetWorkerCapacityGroup(t.Context(), "w-1", "", false)
			if err != nil {
				t.Fatalf("leave: %v", err)
			}
			if left.CapacityGroupID != "" || left.Ephemeral || left.PowerState != store.PowerAwake || left.PowerReason != "" ||
				left.PowerStateAt == nil || left.PowerStateAt.Before(start) {
				t.Fatalf("after leaving: %+v, want no group, awake, no reason, a time since %v", left, start)
			}

			// Leaving when already awake does not move the state's time.
			again, err := st.SetWorkerCapacityGroup(t.Context(), "w-1", "", false)
			if err != nil {
				t.Fatalf("leave again: %v", err)
			}
			if !sameTime(again.PowerStateAt, left.PowerStateAt) {
				t.Fatalf("power time after leaving again = %v, want %v unchanged", again.PowerStateAt, left.PowerStateAt)
			}
		})
	}
}

// powerMember registers worker id in farmID with the given liveness and a
// fresh heartbeat, and joins it to groupID, ephemeral or named.
func powerMember(t *testing.T, st store.Store, farmID, groupID, id string, status store.WorkerStatus, ephemeral bool) {
	t.Helper()
	capacityWorker(t, st, farmID, id, id, status, time.Now())
	if _, err := st.SetWorkerCapacityGroup(t.Context(), id, groupID, ephemeral); err != nil {
		t.Fatalf("SetWorkerCapacityGroup %s: %v", id, err)
	}
}

// injectPower sets worker id's power state as of at, with no reason.
func injectPower(t *testing.T, st store.Store, id string, state store.PowerState, at time.Time) {
	t.Helper()
	if err := storetest.InjectorFor(t, st).InjectWorkerPowerState(t.Context(), id, state, at, ""); err != nil {
		t.Fatalf("InjectWorkerPowerState %s: %v", id, err)
	}
}

// wantPower fails unless w's power state, its time and its reason are as given.
func wantPower(t *testing.T, w store.Worker, state store.PowerState, at time.Time, reason string) {
	t.Helper()
	if w.PowerState != state || w.PowerStateAt == nil || !w.PowerStateAt.Equal(at) || w.PowerReason != reason {
		t.Fatalf("worker %s power = (%q, %v, %q), want (%q, %v, %q)",
			w.ID, w.PowerState, w.PowerStateAt, w.PowerReason, state, at, reason)
	}
}

// powerOf describes worker id's stored power state, its time and its reason.
func powerOf(t *testing.T, st store.Store, id string) string {
	t.Helper()
	w := mustWorker(t, st, id)
	return fmt.Sprintf("%s at %v (%q)", w.PowerState, w.PowerStateAt, w.PowerReason)
}

// powerFixture is a farm and two capacity groups in it.
type powerFixture struct {
	farm         store.Farm
	group, other store.CapacityGroup
}

func newPowerFixture(t *testing.T, st store.Store) powerFixture {
	t.Helper()
	farm := mustCreateFarm(t, st, "main")
	return powerFixture{
		farm:  farm,
		group: mustCreateCapacityGroup(t, st, mkCapacityGroup(farm.ID, "alpha")),
		other: mustCreateCapacityGroup(t, st, mkCapacityGroup(farm.ID, "beta")),
	}
}

// powerSeed is one worker a power test seeds: its liveness, its group and the
// power state it is put in, as of at.
type powerSeed struct {
	id     string
	status store.WorkerStatus
	group  string
	state  store.PowerState
	at     time.Time
}

// seedPower registers and joins each seed, and puts it in its power state.
func seedPower(t *testing.T, st store.Store, farmID string, seeds ...powerSeed) {
	t.Helper()
	for _, m := range seeds {
		powerMember(t, st, farmID, m.group, m.id, m.status, false)
		if m.state != store.PowerAwake {
			injectPower(t, st, m.id, m.state, m.at)
		}
	}
}

// powerSnapshot records the power of each named worker, for unchangedPower.
func powerSnapshot(t *testing.T, st store.Store, ids ...string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		out[id] = powerOf(t, st, id)
	}
	return out
}

// unchangedPower fails for each worker whose power changed since before.
func unchangedPower(t *testing.T, st store.Store, before map[string]string) {
	t.Helper()
	for id, was := range before {
		if now := powerOf(t, st, id); now != was {
			t.Errorf("%s changed: %s -> %s", id, was, now)
		}
	}
}

// TestParity_MarkWaking: only offline, enabled members of the group that are
// awake, asleep or wake-failed move to waking; every other worker named is
// left as it was.
func TestParity_MarkWaking(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			f := newPowerFixture(t, st)
			old := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
			off := store.WorkerStatusOffline
			seedPower(
				t, st, f.farm.ID,
				powerSeed{"w-asleep", off, f.group.ID, store.PowerAsleep, old},
				powerSeed{"w-awake-offline", off, f.group.ID, store.PowerAwake, old},
				powerSeed{"w-wake-failed", off, f.group.ID, store.PowerWakeFailed, old},
				powerSeed{"w-online", store.WorkerStatusOnline, f.group.ID, store.PowerAwake, old},
				powerSeed{"w-disabled", off, f.group.ID, store.PowerAsleep, old},
				powerSeed{"w-retiring", off, f.group.ID, store.PowerRetiring, old},
				powerSeed{"w-waking", off, f.group.ID, store.PowerWaking, old},
				powerSeed{"w-other-group", off, f.other.ID, store.PowerAsleep, old},
			)
			capacityWorker(t, st, f.farm.ID, "w-loose", "w-loose", off, time.Now())
			if _, err := st.SetWorkerDisabled(t.Context(), "w-disabled", true); err != nil {
				t.Fatalf("SetWorkerDisabled: %v", err)
			}
			before := powerSnapshot(t, st, "w-online", "w-disabled", "w-retiring", "w-waking", "w-other-group", "w-loose")

			now := time.Now().UTC().Truncate(time.Microsecond)
			got, err := st.MarkWaking(t.Context(), f.group.ID, []string{
				"w-online", "w-wake-failed", "ghost", "w-disabled", "w-asleep", "w-retiring",
				"w-other-group", "w-awake-offline", "w-waking", "w-loose", "w-asleep",
			}, now)
			if err != nil {
				t.Fatalf("MarkWaking: %v", err)
			}
			want := []string{"w-asleep", "w-awake-offline", "w-wake-failed"}
			if !slices.Equal(got, want) {
				t.Fatalf("MarkWaking moved %v, want %v", got, want)
			}
			for _, id := range want {
				wantPower(t, mustWorker(t, st, id), store.PowerWaking, now, "")
			}
			unchangedPower(t, st, before)

			if got, err := st.MarkWaking(t.Context(), f.group.ID, nil, now); err != nil || len(got) != 0 {
				t.Fatalf("MarkWaking with no IDs = (%v, %v), want nothing", got, err)
			}
		})
	}
}

// TestParity_EmptyGroupIDMatchesNoWorker: an empty group ID names no group, so
// none of the group-scoped transitions touches a worker that is in no group,
// whatever its state.
func TestParity_EmptyGroupIDMatchesNoWorker(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			f := newPowerFixture(t, st)
			old := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
			capacityWorker(t, st, f.farm.ID, "w-off", "w-off", store.WorkerStatusOffline, time.Now())
			capacityWorker(t, st, f.farm.ID, "w-on", "w-on", store.WorkerStatusOnline, time.Now())
			capacityWorker(t, st, f.farm.ID, "w-waking", "w-waking", store.WorkerStatusOffline, time.Now())
			capacityWorker(t, st, f.farm.ID, "w-retiring", "w-retiring", store.WorkerStatusOnline, time.Now())
			injectPower(t, st, "w-waking", store.PowerWaking, old)
			injectPower(t, st, "w-retiring", store.PowerRetiring, old)
			before := powerSnapshot(t, st, "w-off", "w-on", "w-waking", "w-retiring")
			now := time.Now().UTC().Truncate(time.Microsecond)

			if got, err := st.MarkWaking(ctx, "", []string{"w-off"}, now); err != nil || len(got) != 0 {
				t.Errorf("MarkWaking = (%v, %v), want nothing moved", got, err)
			}
			if got, err := st.FailWakes(ctx, "", now, now, "x"); err != nil || len(got) != 0 {
				t.Errorf("FailWakes = (%v, %v), want nothing failed", got, err)
			}
			if got, err := st.ExpireRetirements(ctx, "", now, now, "x"); err != nil || len(got) != 0 {
				t.Errorf("ExpireRetirements = (%v, %v), want nothing expired", got, err)
			}
			if w, ok, err := st.BeginRetirement(ctx, "", "w-on", now); err != nil || ok || w.ID != "" {
				t.Errorf("BeginRetirement = (%+v, %v, %v), want (zero, false, nil)", w, ok, err)
			}
			unchangedPower(t, st, before)
		})
	}
}

// TestParity_FailWakes: a waking member of the group whose wake began before
// the cutoff becomes wake-failed with the reason; one that began at or after
// it, a waking member of another group and a member in another state are kept.
func TestParity_FailWakes(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			f := newPowerFixture(t, st)
			now := time.Now().UTC().Truncate(time.Microsecond)
			cutoff := now.Add(-10 * time.Minute)
			off := store.WorkerStatusOffline
			seedPower(
				t, st, f.farm.ID,
				powerSeed{"w-overdue", off, f.group.ID, store.PowerWaking, cutoff.Add(-time.Second)},
				powerSeed{"w-in-time", off, f.group.ID, store.PowerWaking, cutoff.Add(time.Second)},
				powerSeed{"w-at-cutoff", off, f.group.ID, store.PowerWaking, cutoff},
				powerSeed{"w-other-group", off, f.other.ID, store.PowerWaking, cutoff.Add(-time.Hour)},
				powerSeed{"w-asleep", off, f.group.ID, store.PowerAsleep, cutoff.Add(-time.Hour)},
			)
			before := powerSnapshot(t, st, "w-in-time", "w-at-cutoff", "w-other-group", "w-asleep")

			const reason = "did not register within boot_timeout (10m0s) after a wake request"
			got, err := st.FailWakes(t.Context(), f.group.ID, cutoff, now, reason)
			if err != nil {
				t.Fatalf("FailWakes: %v", err)
			}
			if !slices.Equal(got, []string{"w-overdue"}) {
				t.Fatalf("FailWakes = %v, want only w-overdue", got)
			}
			wantPower(t, mustWorker(t, st, "w-overdue"), store.PowerWakeFailed, now, reason)
			unchangedPower(t, st, before)
		})
	}
}

// TestParity_BeginRetirement: retirement begins only for an awake, online,
// enabled member of the group that holds no task, and a refusal writes
// nothing.
func TestParity_BeginRetirement(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			f := newPowerFixture(t, st)
			on := store.WorkerStatusOnline
			powerMember(t, st, f.farm.ID, f.group.ID, "w-busy", on, false)
			powerMember(t, st, f.farm.ID, f.group.ID, "w-disabled", on, false)
			powerMember(t, st, f.farm.ID, f.group.ID, "w-offline", store.WorkerStatusOffline, false)
			powerMember(t, st, f.farm.ID, f.other.ID, "w-other-group", on, false)
			powerMember(t, st, f.farm.ID, f.group.ID, "w-retiring", on, false)
			powerMember(t, st, f.farm.ID, f.group.ID, "w-idle", on, true)
			capacityWorker(t, st, f.farm.ID, "w-loose", "w-loose", on, time.Now())
			if _, err := st.SetWorkerDisabled(ctx, "w-disabled", true); err != nil {
				t.Fatalf("SetWorkerDisabled: %v", err)
			}
			injectPower(t, st, "w-retiring", store.PowerRetiring, time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond))
			queue := mustCreateQueue(t, st, store.Queue{FarmID: f.farm.ID, Name: "q"})
			storetest.NewJob("busy-job", f.farm.ID, queue.ID).
				Step("busy-step", store.StepStatusReady).
				Task("busy-task", "busy-step", store.TaskStatusAssigned).
				SubmitLeasing(t, st, "w-busy")

			now := time.Now().UTC().Truncate(time.Microsecond)
			refused := []struct{ id, why string }{
				{"w-busy", "holds a leased task"},
				{"w-disabled", "disabled"},
				{"w-offline", "offline"},
				{"w-other-group", "in another group"},
				{"w-loose", "in no group"},
				{"w-retiring", "already retiring"},
			}
			for _, tc := range refused {
				before := powerSnapshot(t, st, tc.id)
				w, ok, err := st.BeginRetirement(ctx, f.group.ID, tc.id, now)
				if err != nil || ok || w.ID != "" {
					t.Fatalf("%s: BeginRetirement = (%+v, %v, %v), want (zero, false, nil)", tc.why, w, ok, err)
				}
				unchangedPower(t, st, before)
			}
			if w, ok, err := st.BeginRetirement(ctx, f.group.ID, "ghost", now); err != nil || ok || w.ID != "" {
				t.Fatalf("unknown worker: BeginRetirement = (%+v, %v, %v), want (zero, false, nil)", w, ok, err)
			}

			w, ok, err := st.BeginRetirement(ctx, f.group.ID, "w-idle", now)
			if err != nil || !ok {
				t.Fatalf("idle member: BeginRetirement = (%+v, %v, %v), want it retiring", w, ok, err)
			}
			wantPower(t, w, store.PowerRetiring, now, "")
			if stored := mustWorker(t, st, "w-idle"); !reflect.DeepEqual(stored, w) {
				t.Fatalf("stored = %+v, want the row BeginRetirement returned, %+v", stored, w)
			}
			if _, ok, err := st.BeginRetirement(ctx, f.group.ID, "w-idle", now.Add(time.Second)); err != nil || ok {
				t.Fatalf("second BeginRetirement = (%v, %v), want refused", ok, err)
			}
		})
	}
}

// TestParity_FinishRetirementNamedSleeps: a retiring named member is asleep
// and offline once its retirement finishes, keeping its credential; a worker
// that is not retiring is refused.
func TestParity_FinishRetirementNamedSleeps(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			f := newPowerFixture(t, st)
			powerMember(t, st, f.farm.ID, f.group.ID, "w-named", store.WorkerStatusOnline, false)
			powerMember(t, st, f.farm.ID, f.group.ID, "w-awake", store.WorkerStatusOnline, false)
			mustCreateCredential(t, st, mkCredential("w-named"))
			begun := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
			if _, ok, err := st.BeginRetirement(ctx, f.group.ID, "w-named", begun); err != nil || !ok {
				t.Fatalf("BeginRetirement = (%v, %v)", ok, err)
			}

			awake := mustWorker(t, st, "w-awake")
			for _, id := range []string{"w-awake", "ghost"} {
				w, deleted, ok, err := st.FinishRetirement(ctx, id, time.Now().UTC())
				if err != nil || ok || deleted || w.ID != "" {
					t.Fatalf("%s: FinishRetirement = (%+v, deleted %v, ok %v, %v), want refused with no error", id, w, deleted, ok, err)
				}
			}
			if got := mustWorker(t, st, "w-awake"); !reflect.DeepEqual(got, awake) {
				t.Fatalf("refused finish changed w-awake: %+v -> %+v", awake, got)
			}

			now := time.Now().UTC().Truncate(time.Microsecond)
			w, deleted, ok, err := st.FinishRetirement(ctx, "w-named", now)
			if err != nil || !ok || deleted {
				t.Fatalf("FinishRetirement = (%+v, %v, %v, %v), want it asleep, not deleted", w, deleted, ok, err)
			}
			wantPower(t, w, store.PowerAsleep, now, "")
			if w.Status != store.WorkerStatusOffline || w.CapacityGroupID != f.group.ID {
				t.Fatalf("finished member = status %s group %q, want offline in %s", w.Status, w.CapacityGroupID, f.group.ID)
			}
			if stored := mustWorker(t, st, "w-named"); !reflect.DeepEqual(stored, w) {
				t.Fatalf("stored = %+v, want the row FinishRetirement returned, %+v", stored, w)
			}
			if _, err := storetest.ActiveWorkerCredential(ctx, st, "w-named"); err != nil {
				t.Fatalf("credential of the sleeping member: %v, want it kept", err)
			}
			if _, _, ok, err := st.FinishRetirement(ctx, "w-named", now); err != nil || ok {
				t.Fatalf("second FinishRetirement = (%v, %v), want refused: it is asleep", ok, err)
			}
		})
	}
}

// TestParity_FinishRetirementEphemeralDeletedAndRevoked: a retiring ephemeral
// member is deleted when its retirement finishes, online or already offline,
// and its active credential is revoked with it.
func TestParity_FinishRetirementEphemeralDeletedAndRevoked(t *testing.T) {
	for _, status := range []store.WorkerStatus{store.WorkerStatusOnline, store.WorkerStatusOffline} {
		t.Run(string(status), func(t *testing.T) {
			for name, st := range newStores(t) {
				t.Run(name, func(t *testing.T) {
					ctx := t.Context()
					f := newPowerFixture(t, st)
					powerMember(t, st, f.farm.ID, f.group.ID, "w-eph", store.WorkerStatusOnline, true)
					mustCreateCredential(t, st, mkCredential("w-eph"))
					if _, ok, err := st.BeginRetirement(ctx, f.group.ID, "w-eph", time.Now().UTC()); err != nil || !ok {
						t.Fatalf("BeginRetirement = (%v, %v)", ok, err)
					}
					if status == store.WorkerStatusOffline {
						// The worker deregistered before its retirement was finished.
						if _, _, err := st.OfflineWorker(ctx, "w-eph", "", time.Now().UTC()); err != nil {
							t.Fatalf("OfflineWorker: %v", err)
						}
					}

					now := time.Now().UTC().Truncate(time.Microsecond)
					w, deleted, ok, err := st.FinishRetirement(ctx, "w-eph", now)
					if err != nil || !ok || !deleted || w.ID != "w-eph" || !w.Ephemeral || w.CapacityGroupID != f.group.ID {
						t.Fatalf("FinishRetirement = (%+v, %v, %v, %v), want the deleted ephemeral member", w, deleted, ok, err)
					}
					if _, err := st.GetWorker(ctx, "w-eph"); !errors.Is(err, store.ErrNotFound) {
						t.Fatalf("GetWorker after finish: err = %v, want ErrNotFound", err)
					}
					if _, err := storetest.ActiveWorkerCredential(ctx, st, "w-eph"); !errors.Is(err, store.ErrNotFound) {
						t.Fatalf("credential after finish: err = %v, want ErrNotFound (revoked)", err)
					}
					if _, _, ok, err := st.FinishRetirement(ctx, "w-eph", now); err != nil || ok {
						t.Fatalf("FinishRetirement of the deleted member = (%v, %v), want refused", ok, err)
					}
				})
			}
		})
	}
}

// TestParity_FinishRetirementRefusesWhileHoldingTask: a retiring member that
// holds a task (BeginRetirement and the lease guard keep one from it; the
// injector puts it there) is neither put to sleep nor deleted under its task:
// the finish fails with ErrConflict and the member stays retiring.
func TestParity_FinishRetirementRefusesWhileHoldingTask(t *testing.T) {
	for _, ephemeral := range []bool{false, true} {
		t.Run(fmt.Sprintf("ephemeral=%v", ephemeral), func(t *testing.T) {
			for name, st := range newStores(t) {
				t.Run(name, func(t *testing.T) {
					f := newPowerFixture(t, st)
					powerMember(t, st, f.farm.ID, f.group.ID, "w-held", store.WorkerStatusOnline, ephemeral)
					queue := mustCreateQueue(t, st, store.Queue{FarmID: f.farm.ID, Name: "q"})
					storetest.NewJob("held-job", f.farm.ID, queue.ID).
						Step("held-step", store.StepStatusReady).
						Task("held-task", "held-step", store.TaskStatusRunning).
						SubmitLeasing(t, st, "w-held")
					injectPower(t, st, "w-held", store.PowerRetiring, time.Now().UTC().Truncate(time.Microsecond))
					before := mustWorker(t, st, "w-held")

					w, deleted, ok, err := st.FinishRetirement(t.Context(), "w-held", time.Now().UTC())
					if !errors.Is(err, store.ErrConflict) || ok || deleted || w.ID != "" {
						t.Fatalf("FinishRetirement = (%+v, %v, %v, %v), want ErrConflict", w, deleted, ok, err)
					}
					if after := mustWorker(t, st, "w-held"); !reflect.DeepEqual(after, before) {
						t.Fatalf("refused finish changed the worker: %+v -> %+v", before, after)
					}
				})
			}
		})
	}
}

// TestParity_AbortAndExpireRetirements: an aborted retirement returns the
// member to awake with the reason; expiry aborts every member of the group
// retiring since before the cutoff, and only those.
func TestParity_AbortAndExpireRetirements(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			f := newPowerFixture(t, st)
			now := time.Now().UTC().Truncate(time.Microsecond)
			cutoff := now.Add(-10 * time.Minute)
			on := store.WorkerStatusOnline
			seedPower(
				t, st, f.farm.ID,
				powerSeed{"w-abort", on, f.group.ID, store.PowerRetiring, now.Add(-time.Second)},
				powerSeed{"w-overdue", on, f.group.ID, store.PowerRetiring, cutoff.Add(-time.Second)},
				powerSeed{"w-in-time", on, f.group.ID, store.PowerRetiring, cutoff.Add(time.Second)},
				powerSeed{"w-other-group", on, f.other.ID, store.PowerRetiring, cutoff.Add(-time.Hour)},
				powerSeed{"w-waking", on, f.group.ID, store.PowerWaking, cutoff.Add(-time.Hour)},
			)

			const refused = "worker refused: remote sleep is not allowed"
			if ok, err := st.AbortRetirement(ctx, "w-abort", now, refused); err != nil || !ok {
				t.Fatalf("AbortRetirement = (%v, %v), want true", ok, err)
			}
			wantPower(t, mustWorker(t, st, "w-abort"), store.PowerAwake, now, refused)
			for _, id := range []string{"w-abort", "w-waking"} {
				before := powerSnapshot(t, st, id)
				if ok, err := st.AbortRetirement(ctx, id, now.Add(time.Second), "again"); err != nil || ok {
					t.Fatalf("%s: AbortRetirement = (%v, %v), want false: not retiring", id, ok, err)
				}
				unchangedPower(t, st, before)
			}
			if ok, err := st.AbortRetirement(ctx, "ghost", now, "again"); err != nil || ok {
				t.Fatalf("unknown worker: AbortRetirement = (%v, %v), want false", ok, err)
			}

			before := powerSnapshot(t, st, "w-in-time", "w-other-group", "w-waking")
			const expired = "retirement did not complete within boot_timeout (10m0s)"
			got, err := st.ExpireRetirements(ctx, f.group.ID, cutoff, now, expired)
			if err != nil {
				t.Fatalf("ExpireRetirements: %v", err)
			}
			if !slices.Equal(got, []string{"w-overdue"}) {
				t.Fatalf("ExpireRetirements = %v, want [w-overdue]", got)
			}
			wantPower(t, mustWorker(t, st, "w-overdue"), store.PowerAwake, now, expired)
			unchangedPower(t, st, before)
		})
	}
}

// sameGroupState fails unless got and want hold the same values, times
// compared as instants.
func sameGroupState(t *testing.T, got, want store.CapacityGroupState) {
	t.Helper()
	if got.GroupID != want.GroupID || got.Requested != want.Requested || got.DegradedReason != want.DegradedReason ||
		!sameTime(got.RequestedAt, want.RequestedAt) || !sameTime(got.DegradedAt, want.DegradedAt) ||
		!sameTime(got.BackoffUntil, want.BackoffUntil) || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Fatalf("group state = %+v, want %+v", got, want)
	}
}

// TestParity_CapacityGroupStateRoundTripAndCascade: a group's state is stored
// and replaced whole, a group with none is absent, a state for an unknown group
// is refused, and deleting a group deletes its state.
func TestParity_CapacityGroupStateRoundTripAndCascade(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			f := newPowerFixture(t, st)
			list := func() map[string]store.CapacityGroupState {
				t.Helper()
				states, err := st.ListCapacityGroupStates(ctx)
				if err != nil {
					t.Fatalf("ListCapacityGroupStates: %v", err)
				}
				return states
			}
			if states := list(); len(states) != 0 {
				t.Fatalf("states before any write = %+v, want none", states)
			}

			at := func(d time.Duration) *time.Time {
				v := time.Now().UTC().Add(d).Truncate(time.Microsecond)
				return &v
			}
			full := store.CapacityGroupState{
				GroupID: f.group.ID, Requested: 6, RequestedAt: at(-time.Hour),
				DegradedReason: "2 of 6 machines did not register within boot_timeout (5m0s)",
				DegradedAt:     at(-time.Minute), BackoffUntil: at(10 * time.Minute), UpdatedAt: *at(0),
			}
			if err := st.PutCapacityGroupState(ctx, full); err != nil {
				t.Fatalf("PutCapacityGroupState: %v", err)
			}
			start := time.Now().UTC().Add(-time.Second)
			bare := store.CapacityGroupState{GroupID: f.other.ID, Requested: 1}
			if err := st.PutCapacityGroupState(ctx, bare); err != nil {
				t.Fatalf("PutCapacityGroupState with no times: %v", err)
			}
			states := list()
			if len(states) != 2 {
				t.Fatalf("states = %+v, want two", states)
			}
			sameGroupState(t, states[f.group.ID], full)
			gotBare := states[f.other.ID]
			if gotBare.UpdatedAt.Before(start) {
				t.Fatalf("a zero UpdatedAt was stored as %v, want stamped since %v", gotBare.UpdatedAt, start)
			}
			bare.UpdatedAt = gotBare.UpdatedAt
			sameGroupState(t, gotBare, bare)

			// A second write replaces the state whole.
			replaced := store.CapacityGroupState{GroupID: f.group.ID, Requested: 2, RequestedAt: at(0), UpdatedAt: *at(0)}
			if err := st.PutCapacityGroupState(ctx, replaced); err != nil {
				t.Fatalf("PutCapacityGroupState again: %v", err)
			}
			sameGroupState(t, list()[f.group.ID], replaced)

			ghost := store.CapacityGroupState{GroupID: "ghost", UpdatedAt: *at(0)}
			if err := st.PutCapacityGroupState(ctx, ghost); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("PutCapacityGroupState for an unknown group: err = %v, want ErrNotFound", err)
			}

			if err := st.DeleteCapacityGroup(ctx, f.group.ID); err != nil {
				t.Fatalf("DeleteCapacityGroup: %v", err)
			}
			if states := list(); len(states) != 1 || states[f.group.ID].GroupID != "" {
				t.Fatalf("states after deleting the group = %+v, want only %s's", states, f.other.ID)
			}
		})
	}
}
