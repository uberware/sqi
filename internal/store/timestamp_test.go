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

// subSecond returns two instants inside one second: t0 on the whole second,
// which trimmed RFC3339Nano text writes as "…:05Z", and t1 half a second later,
// "…:05.5Z". 'Z' sorts after '.', so trimmed text orders t0 after t1; every
// comparison these tests make was wrong on SQLite until timestamps were stored
// at a fixed width.
func subSecond() (t0, t1 time.Time) {
	t0 = time.Now().UTC().Truncate(time.Second)
	return t0, t0.Add(500 * time.Millisecond)
}

func subSecondFarm(t *testing.T, st store.Store) store.Farm {
	t.Helper()
	f, err := st.CreateFarm(t.Context(), store.Farm{ID: uuid.NewString(), Name: uuid.NewString()})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	return f
}

// registerAt registers an online worker with the given last heartbeat; nil
// registers one that has never heartbeated.
func registerAt(t *testing.T, st store.Store, farmID, id string, heartbeat *time.Time) {
	t.Helper()
	if _, _, err := st.RegisterWorker(t.Context(), store.Worker{
		ID: id, FarmID: farmID, Hostname: id, Status: store.WorkerStatusOnline, LastHeartbeatAt: heartbeat,
	}); err != nil {
		t.Fatalf("RegisterWorker %s: %v", id, err)
	}
}

func workerIDs(ws []store.Worker) []string {
	ids := make([]string, 0, len(ws))
	for _, w := range ws {
		ids = append(ids, w.ID)
	}
	return ids
}

func TestSubSecond_StaleHeartbeat(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			t0, t1 := subSecond()
			farm := subSecondFarm(t, st)
			registerAt(t, st, farm.ID, "w-stale", &t0)

			stale, err := st.ListStaleWorkers(t.Context(), t1)
			if err != nil {
				t.Fatalf("ListStaleWorkers: %v", err)
			}
			if !slices.Contains(workerIDs(stale), "w-stale") {
				t.Fatalf("ListStaleWorkers(%v) = %v, want w-stale (heartbeat %v)", t1, workerIDs(stale), t0)
			}
			if _, ok, err := st.OfflineStaleWorker(t.Context(), "w-stale", t1, t1); err != nil || !ok {
				t.Fatalf("OfflineStaleWorker(cutoff %v) = (%v, %v), want taken offline (heartbeat %v)", t1, ok, err, t0)
			}
		})
	}
}

func TestSubSecond_HeartbeatOrdering(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			t0, t1 := subSecond()
			farm := subSecondFarm(t, st)
			registerAt(t, st, farm.ID, "w-later", &t1)
			registerAt(t, st, farm.ID, "w-earlier", &t0)

			page, err := st.ListWorkers(t.Context(), store.ListWorkersOptions{
				FarmID: farm.ID, SortBy: store.WorkerSortByLastHeartbeatAt, SortDir: store.SortAsc,
				Pagination: store.Pagination{Limit: 10},
			})
			if err != nil {
				t.Fatalf("ListWorkers: %v", err)
			}
			if got, want := workerIDs(page.Items), []string{"w-earlier", "w-later"}; !slices.Equal(got, want) {
				t.Fatalf("ListWorkers by last_heartbeat_at asc = %v, want %v", got, want)
			}
		})
	}
}

func TestSubSecond_SessionExpiry(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			t0, t1 := subSecond()
			u, err := st.CreateUser(ctx, mkUser("user"))
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			sess := store.Session{
				ID: uuid.NewString(), TokenHash: "hash-" + uuid.NewString(),
				UserID: u.ID, ExpiresAt: t0, CreatedAt: t0.Add(-time.Hour),
			}
			if _, err := st.CreateSession(ctx, sess); err != nil {
				t.Fatalf("CreateSession: %v", err)
			}
			if _, err := st.GetSessionByTokenHash(ctx, sess.TokenHash, t1); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetSessionByTokenHash at %v for a session expiring %v = %v, want ErrNotFound", t1, t0, err)
			}
			if n, err := st.DeleteExpiredSessions(ctx, t1); err != nil || n != 1 {
				t.Fatalf("DeleteExpiredSessions(%v) = (%d, %v), want (1, nil)", t1, n, err)
			}
		})
	}
}

func TestSubSecond_APIKeyExpiry(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			t0, t1 := subSecond()
			u, err := st.CreateUser(ctx, mkUser("user"))
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			k := mkAPIKey(u.ID, "hash-"+uuid.NewString())
			k.ExpiresAt = &t0
			if _, err := st.CreateAPIKey(ctx, k); err != nil {
				t.Fatalf("CreateAPIKey: %v", err)
			}
			if _, _, err := st.GetAPIKeyUserByTokenHash(ctx, k.TokenHash, t1); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetAPIKeyUserByTokenHash at %v for a key expiring %v = %v, want ErrNotFound", t1, t0, err)
			}
		})
	}
}

func TestSubSecond_JoinTokenExpiry(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			t0, t1 := subSecond()
			tok := store.WorkerJoinToken{
				ID: uuid.NewString(), TokenHash: "jt-" + uuid.NewString(), Prefix: "sqiwjt_abcd",
				ExpiresAt: t0, CreatedAt: t0.Add(-time.Hour),
			}
			if _, err := st.CreateWorkerJoinToken(ctx, tok); err != nil {
				t.Fatalf("CreateWorkerJoinToken: %v", err)
			}
			_, err := st.RedeemWorkerJoinToken(ctx, tok.TokenHash, t1, store.WorkerCredential{
				ID: uuid.NewString(), WorkerID: "w-join", PublicKey: "pub", EnrolledAt: t1,
			})
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("RedeemWorkerJoinToken at %v for a token expiring %v = %v, want ErrNotFound", t1, t0, err)
			}
		})
	}
}

func TestSubSecond_RetryBackoffElapsed(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			t0, t1 := subSecond()
			g := seedStepA(t, st, graphOpts{}, store.TaskStatusReady)
			task := g.Tasks["a"][0]
			storetest.FailAndRequeue(t, st, leaseReq(task), t0)

			ready, err := st.ListReadyTasks(t.Context(), g.Farm.ID, t1, 10)
			if err != nil {
				t.Fatalf("ListReadyTasks: %v", err)
			}
			if !slices.ContainsFunc(ready, func(x store.Task) bool { return x.ID == task.ID }) {
				t.Fatalf("ListReadyTasks at %v omits a task whose retry_after is %v", t1, t0)
			}
		})
	}
}

func TestSubSecond_StaleAssignment(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			t0, t1 := subSecond()
			g := seedStepA(t, st, graphOpts{}, store.TaskStatusReady)
			task := g.Tasks["a"][0]
			req := leaseReq(task)
			req.Now = t0
			storetest.Lease(t, st, req)

			got, err := st.ReclaimStaleAssignedTasks(t.Context(), t1)
			if err != nil {
				t.Fatalf("ReclaimStaleAssignedTasks: %v", err)
			}
			if !slices.ContainsFunc(got, func(x store.Task) bool { return x.ID == task.ID }) {
				t.Fatalf("ReclaimStaleAssignedTasks(cutoff %v) omits a task assigned at %v", t1, t0)
			}
		})
	}
}

// TestListStaleWorkers_SkipsANeverHeartbeatedWorker pins the backends' shared
// rule: a worker with no recorded heartbeat has no age to compare, so it is not
// stale (SQLite's NULL comparison; OfflineStaleWorker already agrees on both).
// The scheduler stamps a heartbeat at registration, so production never has
// such a worker; the rule is about the backends agreeing.
func TestListStaleWorkers_SkipsANeverHeartbeatedWorker(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			t0, t1 := subSecond()
			farm := subSecondFarm(t, st)
			registerAt(t, st, farm.ID, "w-stale", &t0)
			registerAt(t, st, farm.ID, "w-silent", nil)
			fresh := t1.Add(time.Minute)
			registerAt(t, st, farm.ID, "w-fresh", &fresh)
			// An offline worker is never stale, however old its heartbeat.
			if _, _, err := st.RegisterWorker(t.Context(), store.Worker{
				ID: "w-offline", FarmID: farm.ID, Hostname: "w-offline", Status: store.WorkerStatusOffline, LastHeartbeatAt: &t0,
			}); err != nil {
				t.Fatalf("RegisterWorker w-offline: %v", err)
			}

			stale, err := st.ListStaleWorkers(t.Context(), t1)
			if err != nil {
				t.Fatalf("ListStaleWorkers: %v", err)
			}
			if got := workerIDs(stale); !slices.Equal(got, []string{"w-stale"}) {
				t.Fatalf("ListStaleWorkers(%v) = %v, want [w-stale]", t1, got)
			}
		})
	}
}

// TestSubSecond_NanosecondsRoundTrip pins that a stored time keeps every
// digit: the fake keeps times exactly, so SQLite must keep all nine fractional
// digits or the two backends disagree about equality.
func TestSubSecond_NanosecondsRoundTrip(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			t0, _ := subSecond()
			exp := t0.Add(time.Hour + 123456789*time.Nanosecond)
			u, err := st.CreateUser(ctx, mkUser("user"))
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			sess := store.Session{
				ID: uuid.NewString(), TokenHash: "hash-" + uuid.NewString(),
				UserID: u.ID, ExpiresAt: exp, CreatedAt: t0,
			}
			if _, err := st.CreateSession(ctx, sess); err != nil {
				t.Fatalf("CreateSession: %v", err)
			}
			got, err := st.GetSessionByTokenHash(ctx, sess.TokenHash, t0)
			if err != nil {
				t.Fatalf("GetSessionByTokenHash: %v", err)
			}
			if !got.ExpiresAt.Equal(exp) {
				t.Fatalf("ExpiresAt = %v, want %v", got.ExpiresAt, exp)
			}
		})
	}
}
