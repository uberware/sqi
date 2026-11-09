// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"reflect"
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

					// A later registration reporting none replaces what was stored.
					cleared := registerWith(t, st, "w-1", farm.ID, nil, "")
					if len(cleared.Interfaces) != 0 || cleared.CloudInstanceID != "" {
						t.Fatalf("after a registration with none, interfaces %+v cloud %q, want both cleared",
							cleared.Interfaces, cleared.CloudInstanceID)
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
