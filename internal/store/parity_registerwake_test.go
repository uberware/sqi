// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// TestParity_RegisterWorker_ReportsTheWake: a registration that moves a
// member from waking, asleep or wake-failed to awake says so, with the state
// it left and when that began, read under the same lock as the write. One
// that leaves the power state alone (an awake, a retiring or a new worker)
// reports no wake, and a stale one reports nothing.
func TestParity_RegisterWorker_ReportsTheWake(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			f := newPowerFixture(t, st)
			at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
			off := store.WorkerStatusOffline
			seedPower(
				t, st, f.farm.ID,
				powerSeed{"w-waking", off, f.group.ID, store.PowerWaking, at},
				powerSeed{"w-asleep", off, f.group.ID, store.PowerAsleep, at},
				powerSeed{"w-failed", off, f.group.ID, store.PowerWakeFailed, at},
				powerSeed{"w-retiring", store.WorkerStatusOnline, f.group.ID, store.PowerRetiring, at},
				powerSeed{"w-awake", off, f.group.ID, store.PowerAwake, at},
			)
			tests := []struct {
				id       string
				wokeFrom store.PowerState
			}{
				{"w-waking", store.PowerWaking},
				{"w-asleep", store.PowerAsleep},
				{"w-failed", store.PowerWakeFailed},
				{"w-retiring", ""},
				{"w-awake", ""},
				{"w-new", ""},
			}
			for _, tc := range tests {
				now := time.Now().UTC()
				_, reg, err := st.RegisterWorker(t.Context(), store.Worker{
					ID: tc.id, FarmID: f.farm.ID, Hostname: tc.id, Status: store.WorkerStatusOnline, LastHeartbeatAt: &now,
				})
				if err != nil {
					t.Fatalf("RegisterWorker %s: %v", tc.id, err)
				}
				if reg.WokeFrom != tc.wokeFrom {
					t.Errorf("%s WokeFrom = %q, want %q", tc.id, reg.WokeFrom, tc.wokeFrom)
				}
				switch {
				case tc.wokeFrom == "" && reg.WokeFromAt != nil:
					t.Errorf("%s WokeFromAt = %v, want nil with no wake", tc.id, reg.WokeFromAt)
				case tc.wokeFrom != "" && (reg.WokeFromAt == nil || !reg.WokeFromAt.Equal(at)):
					t.Errorf("%s WokeFromAt = %v, want %v", tc.id, reg.WokeFromAt, at)
				}
			}
			// A second registration finds the worker awake: no wake.
			now := time.Now().UTC()
			_, reg, err := st.RegisterWorker(t.Context(), store.Worker{
				ID: "w-asleep", FarmID: f.farm.ID, Hostname: "w-asleep", Status: store.WorkerStatusOnline, LastHeartbeatAt: &now,
			})
			if err != nil || reg.WokeFrom != "" {
				t.Errorf("re-registration = (%+v, %v), want no wake", reg, err)
			}
		})
	}
}
