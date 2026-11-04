// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
)

func TestParity_UpdateWorkerHeartbeat(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			t0, t1 := subSecond()
			farm := seedGraph(t, st, graphOpts{}).Farm
			w := seedWorker(t, st, farm.ID, store.WorkerStatusOnline, t0)

			if err := st.UpdateWorkerHeartbeat(ctx, w.ID, t1, nil); err != nil {
				t.Fatalf("UpdateWorkerHeartbeat: %v", err)
			}
			got, err := st.GetWorker(ctx, w.ID)
			if err != nil || got.LastHeartbeatAt == nil || !got.LastHeartbeatAt.Equal(t1) {
				t.Fatalf("GetWorker = (%+v, %v), want last heartbeat %v", got, err, t1)
			}
			if err := st.UpdateWorkerHeartbeat(ctx, "ghost", t1, nil); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown worker: err = %v, want ErrNotFound", err)
			}
		})
	}
}

// registerStamped registers the fixture worker as instance, the broker having stamped
// the message at stamp (nil is a message with no usable metadata).
func registerStamped(t *testing.T, st store.Store, farmID, instance string, stamp *time.Time) (store.Worker, []store.Task, error) {
	t.Helper()
	now := time.Now().UTC()
	return st.RegisterWorker(t.Context(), store.Worker{
		ID: fixtureWorkerID, FarmID: farmID, Hostname: "node", Status: store.WorkerStatusOnline,
		LastHeartbeatAt: &now, InstanceID: instance, RegistrationBrokerAt: stamp,
	})
}

func TestParity_RegisterWorker_RegistrationBrokerAtRoundTrip(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			farm := seedGraph(t, st, graphOpts{}).Farm
			t1, _ := subSecond()

			got, _, err := registerStamped(t, st, farm.ID, "B", &t1)
			if err != nil || got.RegistrationBrokerAt == nil || !got.RegistrationBrokerAt.Equal(t1) {
				t.Fatalf("RegisterWorker = (%+v, %v), want RegistrationBrokerAt %v", got, err, t1)
			}
			if stored := mustWorker(t, st, fixtureWorkerID); stored.RegistrationBrokerAt == nil || !stored.RegistrationBrokerAt.Equal(t1) {
				t.Fatalf("GetWorker RegistrationBrokerAt = %v, want %v", stored.RegistrationBrokerAt, t1)
			}

			// A registration that sends no broker stamp keeps the stored one.
			if _, _, err := registerStamped(t, st, farm.ID, "", nil); err != nil {
				t.Fatalf("RegisterWorker (no broker stamp): %v", err)
			}
			if stored := mustWorker(t, st, fixtureWorkerID); stored.RegistrationBrokerAt == nil || !stored.RegistrationBrokerAt.Equal(t1) {
				t.Fatalf("after a registration with no broker stamp: RegistrationBrokerAt = %v, want %v kept", stored.RegistrationBrokerAt, t1)
			}
		})
	}
}

// TestParity_RegisterWorker_EarlierBrokerStampIgnored pins that a late
// redelivery of an earlier-stamped registration neither rolls the stored
// instance back nor reclaims the live process's tasks, and that the live
// process's own next registration still finds nothing to reclaim.
func TestParity_RegisterWorker_EarlierBrokerStampIgnored(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning},
				stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusReady}})
			task := g.Tasks["a"][0]
			t1, t2 := subSecond()

			// Process A starts, then restarts as B.
			if _, _, err := registerStamped(t, st, g.Farm.ID, "A", &t1); err != nil {
				t.Fatalf("register A: %v", err)
			}
			if _, _, err := registerStamped(t, st, g.Farm.ID, "B", &t2); err != nil {
				t.Fatalf("register B: %v", err)
			}
			pool := seedPool(t, st, 1)
			leaseClaiming(t, st, task.ID, true, pool)
			before := mustWorker(t, st, fixtureWorkerID)

			// A's registration is redelivered late.
			got, reclaimed, err := registerStamped(t, st, g.Farm.ID, "A", &t1)
			if !errors.Is(err, store.ErrStaleRegistration) {
				t.Fatalf("stale register err = %v, want ErrStaleRegistration", err)
			}
			if len(reclaimed) != 0 {
				t.Fatalf("stale register reclaimed %d tasks", len(reclaimed))
			}
			if got.InstanceID != "B" || got.RegistrationBrokerAt == nil || !got.RegistrationBrokerAt.Equal(t2) {
				t.Fatalf("stale register returned instance %q stamped %v, want the stored B stamped %v", got.InstanceID, got.RegistrationBrokerAt, t2)
			}

			after := mustWorker(t, st, fixtureWorkerID)
			if after.InstanceID != "B" || after.RegistrationBrokerAt == nil || !after.RegistrationBrokerAt.Equal(t2) {
				t.Fatalf("stored instance = %q stamped %v, want B stamped %v", after.InstanceID, after.RegistrationBrokerAt, t2)
			}
			// The refusal returns the whole stored row, not only what the
			// staleness check read.
			if got.ID != after.ID || got.Hostname != after.Hostname || got.FarmID != after.FarmID ||
				!got.RegisteredAt.Equal(after.RegisteredAt) || !got.UpdatedAt.Equal(after.UpdatedAt) {
				t.Fatalf("stale register returned %+v, want the stored worker %+v", got, after)
			}
			if !after.UpdatedAt.Equal(before.UpdatedAt) {
				t.Fatalf("a stale registration wrote the row: UpdatedAt %v -> %v", before.UpdatedAt, after.UpdatedAt)
			}
			if got := mustTask(t, st, task.ID); got.Status != store.TaskStatusRunning || got.AssignedWorkerID != fixtureWorkerID {
				t.Fatalf("task = %q on %q, want still running on %q", got.Status, got.AssignedWorkerID, fixtureWorkerID)
			}

			// B's next (reconnect) registration reclaims nothing: the roll-back
			// that would have made it look like a restart never happened.
			if _, r, err := registerStamped(t, st, g.Farm.ID, "B", &t2); err != nil || len(r) != 0 {
				t.Fatalf("B re-register = (%d reclaimed, %v), want (0, nil)", len(r), err)
			}
			if got := mustTask(t, st, task.ID).Status; got != store.TaskStatusRunning {
				t.Fatalf("task = %q after B's re-register, want running", got)
			}
		})
	}
}

// TestParity_RegisterWorker_LaterBrokerStampRestarts pins that a registration
// whose broker stamp is newer than the stored one is a restart as before.
func TestParity_RegisterWorker_LaterBrokerStampRestarts(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning},
				stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusReady}})
			task := g.Tasks["a"][0]
			t1, t2 := subSecond()

			if _, _, err := registerStamped(t, st, g.Farm.ID, "A", &t1); err != nil {
				t.Fatalf("register A: %v", err)
			}
			leaseClaiming(t, st, task.ID, true)
			_, r, err := registerStamped(t, st, g.Farm.ID, "B", &t2)
			if err != nil || len(r) != 1 || r[0].ID != task.ID {
				t.Fatalf("newer register = (%+v, %v), want the task reclaimed", r, err)
			}
			if w := mustWorker(t, st, fixtureWorkerID); w.InstanceID != "B" || w.RegistrationBrokerAt == nil || !w.RegistrationBrokerAt.Equal(t2) {
				t.Fatalf("stored instance = %q stamped %v, want B stamped %v", w.InstanceID, w.RegistrationBrokerAt, t2)
			}
		})
	}
}

// TestParity_RegisterWorker_SameBrokerStampIsNotStale pins that equal broker stamps
// (a reconnect delivered twice) are accepted: only strictly earlier is stale.
func TestParity_RegisterWorker_SameBrokerStampIsNotStale(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			farm := seedGraph(t, st, graphOpts{}).Farm
			t1, _ := subSecond()
			for i := range 2 {
				if _, _, err := registerStamped(t, st, farm.ID, "A", &t1); err != nil {
					t.Fatalf("register #%d: %v", i+1, err)
				}
			}
		})
	}
}

// TestParity_RegisterWorker_NoBrokerStampBehavesAsBefore pins that a registration
// with no broker stamp, on either side, is decided by instance ID alone.
func TestParity_RegisterWorker_NoBrokerStampBehavesAsBefore(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning},
				stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusReady}})
			task := g.Tasks["a"][0]
			t1, _ := subSecond()

			// Neither side has a broker stamp: a changed instance reclaims.
			if _, _, err := registerStamped(t, st, g.Farm.ID, "A", nil); err != nil {
				t.Fatalf("register A: %v", err)
			}
			leaseClaiming(t, st, task.ID, true)
			if _, r, err := registerStamped(t, st, g.Farm.ID, "B", nil); err != nil || len(r) != 1 {
				t.Fatalf("B (no broker stamps) = (%d reclaimed, %v), want (1, nil)", len(r), err)
			}

			// The stored side has none, the incoming one does: not stale.
			if _, _, err := registerStamped(t, st, g.Farm.ID, "C", &t1); err != nil {
				t.Fatalf("C (stored has no broker stamp): %v", err)
			}
			// The incoming side has none, the stored one does: not stale, and a
			// changed instance still reclaims.
			leaseClaiming(t, st, task.ID, true)
			if _, r, err := registerStamped(t, st, g.Farm.ID, "D", nil); err != nil || len(r) != 1 {
				t.Fatalf("D (incoming has no broker stamp) = (%d reclaimed, %v), want (1, nil)", len(r), err)
			}
		})
	}
}

// beatStamped records a heartbeat at at, the broker having stamped it at
// stamp (nil is a message with no usable metadata).
func beatStamped(t *testing.T, st store.Store, id string, at time.Time, stamp *time.Time) {
	t.Helper()
	if err := st.UpdateWorkerHeartbeat(t.Context(), id, at, stamp); err != nil {
		t.Fatalf("UpdateWorkerHeartbeat(at %v, stamped %v): %v", at, stamp, err)
	}
}

// wantBeat fails unless the stored worker's last heartbeat is at and its
// heartbeat broker stamp is stamp (nil for none).
func wantBeat(t *testing.T, st store.Store, id string, at time.Time, stamp *time.Time) store.Worker {
	t.Helper()
	got := mustWorker(t, st, id)
	if got.LastHeartbeatAt == nil || !got.LastHeartbeatAt.Equal(at) {
		t.Fatalf("LastHeartbeatAt = %v, want %v", got.LastHeartbeatAt, at)
	}
	switch {
	case stamp == nil && got.HeartbeatBrokerAt != nil:
		t.Fatalf("HeartbeatBrokerAt = %v, want none", got.HeartbeatBrokerAt)
	case stamp != nil && (got.HeartbeatBrokerAt == nil || !got.HeartbeatBrokerAt.Equal(*stamp)):
		t.Fatalf("HeartbeatBrokerAt = %v, want %v", got.HeartbeatBrokerAt, *stamp)
	}
	return got
}

// TestParity_UpdateWorkerHeartbeat_BrokerStampNeverMovesBackwards pins that the
// broker's stream timestamp orders heartbeats: one stamped earlier than the
// recorded one (a late redelivery) is ignored whatever the worker's clock
// says, and one stamped the same or later is recorded.
func TestParity_UpdateWorkerHeartbeat_BrokerStampNeverMovesBackwards(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			b1, b2 := subSecond()
			at1, at2 := b1.Add(-time.Minute), b1.Add(-time.Minute+time.Second)
			farm := seedGraph(t, st, graphOpts{}).Farm
			w := seedWorker(t, st, farm.ID, store.WorkerStatusOnline, b1.Add(-time.Hour))

			beatStamped(t, st, w.ID, at1, &b2)
			updated := wantBeat(t, st, w.ID, at1, &b2)
			// A late redelivery the broker stamped earlier is accepted and
			// ignored, though the worker's clock calls it newer.
			beatStamped(t, st, w.ID, at2, &b1)
			if got := wantBeat(t, st, w.ID, at1, &b2); !got.UpdatedAt.Equal(updated.UpdatedAt) {
				t.Fatalf("UpdatedAt moved to %v on an ignored heartbeat, want %v", got.UpdatedAt, updated.UpdatedAt)
			}
			// The same broker stamp again is recorded, and then a later one.
			beatStamped(t, st, w.ID, at2, &b2)
			wantBeat(t, st, w.ID, at2, &b2)
			b3 := b2.Add(time.Second)
			at3 := at2.Add(time.Second)
			beatStamped(t, st, w.ID, at3, &b3)
			wantBeat(t, st, w.ID, at3, &b3)
		})
	}
}

// TestParity_UpdateWorkerHeartbeat_WorkerClockStepBackRecorded pins that a
// heartbeat carrying an earlier at but a later broker stamp is recorded, with
// that at as LastHeartbeatAt and the broker stamp as HeartbeatBrokerAt: only
// the broker stamp orders heartbeats. (The scheduler passes the broker stamp
// as at, so in a running farm the two agree; the store does not assume it.)
func TestParity_UpdateWorkerHeartbeat_WorkerClockStepBackRecorded(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			b1, b2 := subSecond()
			farm := seedGraph(t, st, graphOpts{}).Farm
			w := seedWorker(t, st, farm.ID, store.WorkerStatusOnline, b1.Add(-time.Hour))

			beatStamped(t, st, w.ID, b1, &b1)
			stepped := b1.Add(-10 * time.Minute)
			beatStamped(t, st, w.ID, stepped, &b2)
			wantBeat(t, st, w.ID, stepped, &b2)
		})
	}
}

// TestParity_UpdateWorkerHeartbeat_UnstampedRecordedUnconditionally pins that a
// heartbeat with no broker stamp is recorded whatever its time, as before
// heartbeats were ordered, and keeps the recorded broker stamp.
func TestParity_UpdateWorkerHeartbeat_UnstampedRecordedUnconditionally(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			b1, b2 := subSecond()
			farm := seedGraph(t, st, graphOpts{}).Farm
			w := seedWorker(t, st, farm.ID, store.WorkerStatusOnline, b1.Add(-time.Hour))

			beatStamped(t, st, w.ID, b2, nil)
			wantBeat(t, st, w.ID, b2, nil)
			beatStamped(t, st, w.ID, b2, &b2)
			beatStamped(t, st, w.ID, b1, nil)
			wantBeat(t, st, w.ID, b1, &b2)
		})
	}
}

// TestParity_RegisterWorker_NeverWritesHeartbeatBrokerAt pins that a
// registration leaves the heartbeat broker stamp alone: a new worker has none
// whatever the registration carries, and a re-registration keeps the stored
// one.
func TestParity_RegisterWorker_NeverWritesHeartbeatBrokerAt(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			b1, b2 := subSecond()
			farm := seedGraph(t, st, graphOpts{}).Farm
			reg := func(stamp *time.Time) store.Worker {
				t.Helper()
				now := time.Now().UTC()
				got, _, err := st.RegisterWorker(t.Context(), store.Worker{
					ID: fixtureWorkerID, FarmID: farm.ID, Hostname: "node", Status: store.WorkerStatusOnline,
					LastHeartbeatAt: &now, HeartbeatBrokerAt: stamp,
				})
				if err != nil {
					t.Fatalf("RegisterWorker: %v", err)
				}
				return got
			}
			if got := reg(&b1); got.HeartbeatBrokerAt != nil {
				t.Fatalf("new worker: HeartbeatBrokerAt = %v, want none", got.HeartbeatBrokerAt)
			}
			if got := mustWorker(t, st, fixtureWorkerID); got.HeartbeatBrokerAt != nil {
				t.Fatalf("new worker stored HeartbeatBrokerAt = %v, want none", got.HeartbeatBrokerAt)
			}
			beatStamped(t, st, fixtureWorkerID, b1, &b1)
			if got := reg(&b2); got.HeartbeatBrokerAt == nil || !got.HeartbeatBrokerAt.Equal(b1) {
				t.Fatalf("re-registration: HeartbeatBrokerAt = %v, want %v kept", got.HeartbeatBrokerAt, b1)
			}
			if got := reg(nil); got.HeartbeatBrokerAt == nil || !got.HeartbeatBrokerAt.Equal(b1) {
				t.Fatalf("re-registration with none: HeartbeatBrokerAt = %v, want %v kept", got.HeartbeatBrokerAt, b1)
			}
		})
	}
}

// TestParity_UpdateWorkerHeartbeat_NeverHeartbeatedWorker pins that a worker
// with no heartbeat on record takes the first one.
func TestParity_UpdateWorkerHeartbeat_NeverHeartbeatedWorker(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			t1, _ := subSecond()
			farm := seedGraph(t, st, graphOpts{}).Farm
			registerAt(t, st, farm.ID, "w-fresh", store.WorkerStatusOnline, nil)
			if err := st.UpdateWorkerHeartbeat(t.Context(), "w-fresh", t1, nil); err != nil {
				t.Fatalf("UpdateWorkerHeartbeat: %v", err)
			}
			if got := mustWorker(t, st, "w-fresh"); got.LastHeartbeatAt == nil || !got.LastHeartbeatAt.Equal(t1) {
				t.Fatalf("LastHeartbeatAt = %v, want %v", got.LastHeartbeatAt, t1)
			}
		})
	}
}

func TestParity_UpdateWorkerHeartbeat_UnknownWorkerNotFound(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			t1, _ := subSecond()
			if err := st.UpdateWorkerHeartbeat(t.Context(), "ghost", t1, nil); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown worker: err = %v, want ErrNotFound", err)
			}
		})
	}
}

func mkCredential(workerID string) store.WorkerCredential {
	return store.WorkerCredential{
		ID: uuid.NewString(), WorkerID: workerID, PublicKey: "U" + uuid.NewString(), Name: workerID,
		EnrolledAt: time.Now().UTC(),
	}
}

func mustCreateCredential(t *testing.T, st store.Store, c store.WorkerCredential) store.WorkerCredential {
	t.Helper()
	out, err := st.CreateWorkerCredential(t.Context(), c)
	if err != nil {
		t.Fatalf("CreateWorkerCredential %s: %v", c.WorkerID, err)
	}
	return out
}

func activeCredentialIDs(t *testing.T, st store.Store) []string {
	t.Helper()
	creds, err := st.ListActiveWorkerCredentials(t.Context())
	if err != nil {
		t.Fatalf("ListActiveWorkerCredentials: %v", err)
	}
	ids := make([]string, 0, len(creds))
	for _, c := range creds {
		if c.RevokedAt != nil {
			t.Fatalf("listed credential %+v is revoked", c)
		}
		ids = append(ids, c.ID)
	}
	slices.Sort(ids)
	return ids
}

func TestParity_CreateWorkerCredential(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			want := mkCredential("w1")
			got, err := st.CreateWorkerCredential(ctx, want)
			if err != nil || got.ID != want.ID || got.WorkerID != "w1" || got.PublicKey != want.PublicKey ||
				got.Name != want.Name || !got.EnrolledAt.Equal(want.EnrolledAt) || got.LastSeenAt != nil || got.RevokedAt != nil {
				t.Fatalf("CreateWorkerCredential = (%+v, %v), want %+v", got, err, want)
			}

			sameID := mkCredential("w9")
			sameID.ID = want.ID
			sameKey := mkCredential("w8")
			sameKey.PublicKey = want.PublicKey
			for label, c := range map[string]store.WorkerCredential{
				"same worker": mkCredential("w1"), "same id": sameID, "same public key": sameKey,
			} {
				if _, err := st.CreateWorkerCredential(ctx, c); !errors.Is(err, store.ErrConflict) {
					t.Fatalf("%s: err = %v, want ErrConflict", label, err)
				}
			}

			// A revoked credential frees its worker ID (key rotation) but never its key.
			if err := st.RevokeWorkerCredential(ctx, "w1", time.Now().UTC()); err != nil {
				t.Fatalf("RevokeWorkerCredential: %v", err)
			}
			rotated := mustCreateCredential(t, st, mkCredential("w1"))
			reused := mkCredential("w7")
			reused.PublicKey = want.PublicKey
			if _, err := st.CreateWorkerCredential(ctx, reused); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("revoked key reused: err = %v, want ErrConflict", err)
			}
			if got := activeCredentialIDs(t, st); !slices.Equal(got, []string{rotated.ID}) {
				t.Fatalf("active = %v, want only the rotated credential %s", got, rotated.ID)
			}
		})
	}
}

func TestParity_ListActiveWorkerCredentials(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			if got := activeCredentialIDs(t, st); len(got) != 0 {
				t.Fatalf("empty store: active = %v, want none", got)
			}
			a := mustCreateCredential(t, st, mkCredential("w1"))
			mustCreateCredential(t, st, mkCredential("w2"))
			c := mustCreateCredential(t, st, mkCredential("w3"))
			if err := st.RevokeWorkerCredential(t.Context(), "w2", time.Now().UTC()); err != nil {
				t.Fatalf("RevokeWorkerCredential: %v", err)
			}
			if got, want := activeCredentialIDs(t, st), sorted(a.ID, c.ID); !slices.Equal(got, want) {
				t.Fatalf("active = %v, want %v (the revoked one never listed)", got, want)
			}
		})
	}
}

func TestParity_RevokeWorkerCredential(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			now := time.Now().UTC()
			keep := mustCreateCredential(t, st, mkCredential("w2"))
			mustCreateCredential(t, st, mkCredential("w1"))
			if err := st.RevokeWorkerCredential(ctx, "w1", now); err != nil {
				t.Fatalf("RevokeWorkerCredential: %v", err)
			}
			if _, err := storetest.ActiveWorkerCredential(ctx, st, "w1"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("w1 after revoke: err = %v, want no active credential", err)
			}
			// After a rotation the worker has a revoked row and an active one:
			// revoking reaches the active one.
			mustCreateCredential(t, st, mkCredential("w1"))
			if err := st.RevokeWorkerCredential(ctx, "w1", now); err != nil {
				t.Fatalf("revoke after rotation: %v", err)
			}
			if _, err := storetest.ActiveWorkerCredential(ctx, st, "w1"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("w1 after second revoke: err = %v, want no active credential", err)
			}
			if got := activeCredentialIDs(t, st); !slices.Equal(got, []string{keep.ID}) {
				t.Fatalf("active = %v, want only %s", got, keep.ID)
			}
			if err := st.RevokeWorkerCredential(ctx, "ghost", now); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown worker: err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestParity_GetActiveWorkerCredentialByPublicKey(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			c := mustCreateCredential(t, st, mkCredential("w1"))
			got, err := st.GetActiveWorkerCredentialByPublicKey(ctx, c.PublicKey)
			if err != nil || got.ID != c.ID || got.WorkerID != "w1" || got.PublicKey != c.PublicKey || got.RevokedAt != nil {
				t.Fatalf("enrolled key = (%+v, %v), want credential %s for w1", got, err, c.ID)
			}
			if _, err := st.GetActiveWorkerCredentialByPublicKey(ctx, "U-unknown"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown key: err = %v, want ErrNotFound", err)
			}

			// Revoked: the key is still enrolled, but does not authenticate.
			if err := st.RevokeWorkerCredential(ctx, "w1", time.Now().UTC()); err != nil {
				t.Fatalf("RevokeWorkerCredential: %v", err)
			}
			if _, err := st.GetActiveWorkerCredentialByPublicKey(ctx, c.PublicKey); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("revoked key: err = %v, want ErrNotFound", err)
			}

			// Re-enrolled with a new key: the new key resolves, the old stays dead.
			next := mustCreateCredential(t, st, mkCredential("w1"))
			got, err = st.GetActiveWorkerCredentialByPublicKey(ctx, next.PublicKey)
			if err != nil || got.ID != next.ID || got.WorkerID != "w1" {
				t.Fatalf("new key = (%+v, %v), want credential %s for w1", got, err, next.ID)
			}
			if _, err := st.GetActiveWorkerCredentialByPublicKey(ctx, c.PublicKey); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("old key after re-enroll: err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestParity_TouchWorkerCredential(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			_, seen := subSecond()
			mustCreateCredential(t, st, mkCredential("w1"))
			if err := st.RevokeWorkerCredential(ctx, "w1", time.Now().UTC()); err != nil {
				t.Fatalf("RevokeWorkerCredential: %v", err)
			}
			if err := st.TouchWorkerCredential(ctx, "w1", seen); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("only a revoked credential: err = %v, want ErrNotFound", err)
			}
			active := mustCreateCredential(t, st, mkCredential("w1"))
			if err := st.TouchWorkerCredential(ctx, "w1", seen); err != nil {
				t.Fatalf("TouchWorkerCredential: %v", err)
			}
			got, err := storetest.ActiveWorkerCredential(ctx, st, "w1")
			if err != nil || got.ID != active.ID || got.LastSeenAt == nil || !got.LastSeenAt.Equal(seen) {
				t.Fatalf("active credential = (%+v, %v), want %s last seen %v", got, err, active.ID, seen)
			}
			if err := st.TouchWorkerCredential(ctx, "ghost", seen); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown worker: err = %v, want ErrNotFound", err)
			}
		})
	}
}

func mkJoinToken() store.WorkerJoinToken {
	now := time.Now().UTC()
	return store.WorkerJoinToken{
		ID: uuid.NewString(), TokenHash: "sha256-" + uuid.NewString(), Prefix: "sqi_jt_ab", Name: "rack 4",
		ExpiresAt: now.Add(time.Hour), CreatedBy: "admin", CreatedAt: now,
	}
}

func mustCreateJoinToken(t *testing.T, st store.Store) store.WorkerJoinToken {
	t.Helper()
	tok, err := st.CreateWorkerJoinToken(t.Context(), mkJoinToken())
	if err != nil {
		t.Fatalf("CreateWorkerJoinToken: %v", err)
	}
	return tok
}

func TestParity_GetWorkerJoinTokenByHash(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			tok := mustCreateJoinToken(t, st)
			mustCreateJoinToken(t, st)
			got, err := st.GetWorkerJoinTokenByHash(t.Context(), tok.TokenHash)
			if err != nil || got.ID != tok.ID || got.TokenHash != tok.TokenHash || got.Prefix != tok.Prefix ||
				got.Name != tok.Name || got.CreatedBy != tok.CreatedBy || !got.ExpiresAt.Equal(tok.ExpiresAt) ||
				!got.CreatedAt.Equal(tok.CreatedAt) || got.UsedAt != nil {
				t.Fatalf("GetWorkerJoinTokenByHash = (%+v, %v), want %+v", got, err, tok)
			}
			if _, err := st.GetWorkerJoinTokenByHash(t.Context(), "sha256-unknown"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown hash: err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestParity_MarkWorkerJoinTokenUsed(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			_, used := subSecond()
			tok := mustCreateJoinToken(t, st)
			if err := st.MarkWorkerJoinTokenUsed(ctx, tok.ID, used); err != nil {
				t.Fatalf("MarkWorkerJoinTokenUsed: %v", err)
			}
			got, err := st.GetWorkerJoinTokenByHash(ctx, tok.TokenHash)
			if err != nil || got.UsedAt == nil || !got.UsedAt.Equal(used) {
				t.Fatalf("token after mark = (%+v, %v), want used at %v", got, err, used)
			}
			// A used token can no longer be redeemed.
			if _, err := st.RedeemWorkerJoinToken(ctx, tok.TokenHash, time.Now().UTC(), mkCredential("w1")); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("redeem a used token: err = %v, want ErrNotFound", err)
			}
			if err := st.MarkWorkerJoinTokenUsed(ctx, uuid.NewString(), used); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown id: err = %v, want ErrNotFound", err)
			}
		})
	}
}
