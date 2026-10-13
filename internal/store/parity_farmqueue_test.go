// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
)

func mustCreateFarm(t *testing.T, st store.Store, name string) store.Farm {
	t.Helper()
	f, err := st.CreateFarm(t.Context(), store.Farm{ID: uuid.NewString(), Name: name})
	if err != nil {
		t.Fatalf("CreateFarm %s: %v", name, err)
	}
	return f
}

func mustGetFarm(t *testing.T, st store.Store, id string) store.Farm {
	t.Helper()
	f, err := st.GetFarm(t.Context(), id)
	if err != nil {
		t.Fatalf("GetFarm %s: %v", id, err)
	}
	return f
}

func mustCreateQueue(t *testing.T, st store.Store, q store.Queue) store.Queue {
	t.Helper()
	if q.ID == "" {
		q.ID = uuid.NewString()
	}
	out, err := st.CreateQueue(t.Context(), q)
	if err != nil {
		t.Fatalf("CreateQueue %s: %v", q.Name, err)
	}
	return out
}

func TestParity_CreateFarm(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			delay := 30
			want := store.Farm{ID: uuid.NewString(), Name: "main", Description: "the farm", MaxConcurrentTasks: 7, MaxAttempts: new(int), RetryDelaySeconds: &delay}
			*want.MaxAttempts = 5
			got, err := st.CreateFarm(ctx, want)
			if err != nil {
				t.Fatalf("CreateFarm: %v", err)
			}
			if got.ID != want.ID || got.Name != want.Name || got.Description != want.Description ||
				got.MaxConcurrentTasks != 7 || got.MaxAttempts == nil || *got.MaxAttempts != 5 ||
				got.RetryDelaySeconds == nil || *got.RetryDelaySeconds != 30 || got.FailureLimit != nil {
				t.Fatalf("CreateFarm returned (%+v), want fields persisted", got)
			}
			// Read it back.
			stored, err := st.GetFarm(ctx, want.ID)
			if err != nil || stored.Name != "main" {
				t.Fatalf("GetFarm after CreateFarm: %v / %s, want success and name main", err, stored.Name)
			}
			// Duplicate name: ErrConflict.
			if _, err := st.CreateFarm(ctx, store.Farm{ID: uuid.NewString(), Name: "main"}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("duplicate name: err = %v, want ErrConflict", err)
			}
		})
	}
}

func TestParity_UpdateFarm(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			orig := mustCreateFarm(t, st, "main")
			_, later := subSecond() // so UpdatedAt can advance
			time.Sleep(time.Millisecond)

			newAttempts := 9
			upd := orig
			upd.Name = "main-2"
			upd.Description = "updated"
			upd.MaxConcurrentTasks = 99
			upd.MaxAttempts = &newAttempts
			upd.CreatedAt = later // ignored: CreatedAt keeps its stored value
			upd.UpdatedAt = later // ignored: the store stamps UpdatedAt itself

			got, err := st.UpdateFarm(ctx, upd)
			if err != nil {
				t.Fatalf("UpdateFarm: %v", err)
			}
			if got.Name != "main-2" || got.Description != "updated" || got.MaxConcurrentTasks != 99 ||
				got.MaxAttempts == nil || *got.MaxAttempts != 9 {
				t.Fatalf("UpdateFarm returned (%+v), want mutable fields updated", got)
			}
			if !got.CreatedAt.Equal(orig.CreatedAt) || got.UpdatedAt.Before(orig.UpdatedAt) {
				t.Fatalf("UpdateFarm: CreatedAt changed or UpdatedAt not advanced")
			}
			// Read it back.
			stored := mustGetFarm(t, st, orig.ID)
			if stored.Name != "main-2" || stored.Description != "updated" {
				t.Fatalf("GetFarm after UpdateFarm: %+v, want updates persisted", stored)
			}
			// Unknown ID: ErrNotFound.
			if _, err := st.UpdateFarm(ctx, store.Farm{ID: uuid.NewString(), Name: "foo"}); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown id: err = %v, want ErrNotFound", err)
			}
			// Rename to conflicting name: ErrConflict.
			mustCreateFarm(t, st, "taken")
			if _, err := st.UpdateFarm(ctx, store.Farm{ID: orig.ID, Name: "taken", Description: "conflict"}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("rename to taken name: err = %v, want ErrConflict", err)
			}
		})
	}
}

func TestParity_DeleteFarm(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			farm := mustCreateFarm(t, st, "main")
			keep := mustCreateFarm(t, st, "kept")
			if err := st.DeleteFarm(ctx, farm.ID); err != nil {
				t.Fatalf("DeleteFarm: %v", err)
			}
			if _, err := st.GetFarm(ctx, farm.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetFarm after delete: err = %v, want ErrNotFound", err)
			}
			if err := st.DeleteFarm(ctx, farm.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("second delete: err = %v, want ErrNotFound", err)
			}
			if _, err := st.GetFarm(ctx, keep.ID); err != nil {
				t.Fatalf("other farm: %v, want it kept", err)
			}
		})
	}
}

func TestParity_GetFarm(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			attempts := 3
			want := store.Farm{ID: uuid.NewString(), Name: "main", Description: "the farm", MaxConcurrentTasks: 7, MaxAttempts: &attempts}
			if _, err := st.CreateFarm(t.Context(), want); err != nil {
				t.Fatalf("CreateFarm: %v", err)
			}
			got, err := st.GetFarm(t.Context(), want.ID)
			if err != nil || got.ID != want.ID || got.Name != "main" || got.Description != "the farm" ||
				got.MaxConcurrentTasks != 7 || got.MaxAttempts == nil || *got.MaxAttempts != 3 ||
				got.RetryDelaySeconds != nil || got.FailureLimit != nil {
				t.Fatalf("GetFarm = (%+v, %v), want %+v", got, err, want)
			}
			if _, err := st.GetFarm(t.Context(), uuid.NewString()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown id: err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestParity_ListFarms(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			if got, err := st.ListFarms(t.Context()); err != nil || len(got) != 0 {
				t.Fatalf("empty store: ListFarms = (%+v, %v), want none", got, err)
			}
			for _, n := range []string{"tokyo", "berlin", "lima"} {
				mustCreateFarm(t, st, n)
			}
			got, err := st.ListFarms(t.Context())
			if err != nil {
				t.Fatalf("ListFarms: %v", err)
			}
			names := project(got, func(f store.Farm) string { return f.Name })
			if want := []string{"berlin", "lima", "tokyo"}; !slices.Equal(names, want) {
				t.Fatalf("names = %v, want %v (ordered by name)", names, want)
			}
		})
	}
}

func TestParity_CreateQueue(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			farm := mustCreateFarm(t, st, "main")
			user, limit := "render", 5
			want := store.Queue{
				ID: uuid.NewString(), FarmID: farm.ID, Name: "q", Description: "d", Priority: 40,
				MaxConcurrentTasks: 3, Paused: true, FailureLimit: &limit, RunAsUser: &user,
			}
			got, err := st.CreateQueue(ctx, want)
			if err != nil {
				t.Fatalf("CreateQueue: %v", err)
			}
			if got.ID != want.ID || got.FarmID != farm.ID || got.Name != "q" || got.Description != "d" ||
				got.Priority != 40 || got.MaxConcurrentTasks != 3 || !got.Paused ||
				got.FailureLimit == nil || *got.FailureLimit != 5 || got.MaxAttempts != nil ||
				got.RunAsUser == nil || *got.RunAsUser != "render" || got.RunAsGroup != nil {
				t.Fatalf("CreateQueue returned (%+v), want fields persisted", got)
			}
			// Read it back.
			stored, err := st.GetQueue(ctx, want.ID)
			if err != nil || stored.Name != "q" {
				t.Fatalf("GetQueue after CreateQueue: %v / %s, want success and name q", err, stored.Name)
			}
			// Duplicate name in same farm: ErrConflict.
			if _, err := st.CreateQueue(ctx, store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: "q"}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("duplicate name in farm: err = %v, want ErrConflict", err)
			}
			// Duplicate name in different farm: allowed.
			farm2 := mustCreateFarm(t, st, "other")
			if _, err := st.CreateQueue(ctx, store.Queue{ID: uuid.NewString(), FarmID: farm2.ID, Name: "q"}); err != nil {
				t.Fatalf("same name in different farm: %v, want success", err)
			}
		})
	}
}

func TestParity_UpdateQueue(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			farm := mustCreateFarm(t, st, "main")
			orig := mustCreateQueue(t, st, store.Queue{FarmID: farm.ID, Name: "q"})
			_, later := subSecond()
			time.Sleep(time.Millisecond)

			newLimit := 9
			upd := orig
			upd.Name = "q-updated"
			upd.Description = "new desc"
			upd.Priority = 99
			upd.MaxConcurrentTasks = 10
			upd.Paused = true
			upd.FailureLimit = &newLimit
			upd.UpdatedAt = later // ignored: the store stamps UpdatedAt itself

			got, err := st.UpdateQueue(ctx, upd)
			if err != nil {
				t.Fatalf("UpdateQueue: %v", err)
			}
			if got.Name != "q-updated" || got.Description != "new desc" || got.Priority != 99 ||
				got.MaxConcurrentTasks != 10 || !got.Paused ||
				got.FailureLimit == nil || *got.FailureLimit != 9 {
				t.Fatalf("UpdateQueue returned (%+v), want mutable fields updated", got)
			}
			if !got.CreatedAt.Equal(orig.CreatedAt) || got.UpdatedAt.Before(orig.UpdatedAt) {
				t.Fatalf("UpdateQueue: CreatedAt changed or UpdatedAt not advanced")
			}
			// Read it back.
			stored, err := st.GetQueue(ctx, orig.ID)
			if err != nil || stored.Name != "q-updated" {
				t.Fatalf("GetQueue after UpdateQueue: %v / %s, want success and name q-updated", err, stored.Name)
			}
			// Unknown ID: ErrNotFound.
			if _, err := st.UpdateQueue(ctx, store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: "foo"}); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown id: err = %v, want ErrNotFound", err)
			}
			// Rename to conflicting name in same farm: ErrConflict.
			mustCreateQueue(t, st, store.Queue{FarmID: farm.ID, Name: "taken"})
			if _, err := st.UpdateQueue(ctx, store.Queue{ID: orig.ID, FarmID: farm.ID, Name: "taken"}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("rename to taken name: err = %v, want ErrConflict", err)
			}
		})
	}
}

func TestParity_GetQueue(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			farm := mustCreateFarm(t, st, "main")
			user, limit := "render", 5
			want := mustCreateQueue(t, st, store.Queue{
				FarmID: farm.ID, Name: "q", Description: "d", Priority: 40, MaxConcurrentTasks: 3,
				Paused: true, FailureLimit: &limit, RunAsUser: &user,
			})
			got, err := st.GetQueue(t.Context(), want.ID)
			if err != nil || got.ID != want.ID || got.FarmID != farm.ID || got.Name != "q" || got.Description != "d" ||
				got.Priority != 40 || got.MaxConcurrentTasks != 3 || !got.Paused ||
				got.FailureLimit == nil || *got.FailureLimit != 5 || got.MaxAttempts != nil ||
				got.RunAsUser == nil || *got.RunAsUser != "render" || got.RunAsGroup != nil {
				t.Fatalf("GetQueue = (%+v, %v), want %+v", got, err, want)
			}
			if _, err := st.GetQueue(t.Context(), uuid.NewString()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown id: err = %v, want ErrNotFound", err)
			}
		})
	}
}

// waitClockPast blocks until the wall clock reads later than ts, so a row the
// store stamps next gets a strictly later created_at. The store stamps with
// time.Now and takes no clock, and a coarse clock (Windows) gives two
// consecutive creations the same stamp; ordering by created_at then falls to
// the id tiebreak, a random UUID.
func waitClockPast(ts time.Time) {
	for !time.Now().After(ts) {
		time.Sleep(time.Millisecond)
	}
}

// seedQueues creates two farms: farm A with queues q-b (priority 10, paused),
// q-a (30) and q-c (20), created in that order with strictly increasing
// created_at, and farm B with q-d.
func seedQueues(t *testing.T, st store.Store) (farmA, farmB store.Farm) {
	t.Helper()
	farmA, farmB = mustCreateFarm(t, st, "a"), mustCreateFarm(t, st, "b")
	b := mustCreateQueue(t, st, store.Queue{FarmID: farmA.ID, Name: "q-b", Priority: 10, Paused: true})
	waitClockPast(b.CreatedAt)
	a := mustCreateQueue(t, st, store.Queue{FarmID: farmA.ID, Name: "q-a", Priority: 30})
	waitClockPast(a.CreatedAt)
	c := mustCreateQueue(t, st, store.Queue{FarmID: farmA.ID, Name: "q-c", Priority: 20})
	waitClockPast(c.CreatedAt)
	mustCreateQueue(t, st, store.Queue{FarmID: farmB.ID, Name: "q-d", Priority: 50})
	return farmA, farmB
}

func TestParity_ListQueues(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name      string
		opts      func(farmA store.Farm) store.ListQueuesOptions
		want      []string
		wantTotal int
	}{
		{
			"all farms by name", func(store.Farm) store.ListQueuesOptions { return store.ListQueuesOptions{} },
			[]string{"q-a", "q-b", "q-c", "q-d"},
			4,
		},
		{
			"farm filter", func(f store.Farm) store.ListQueuesOptions { return store.ListQueuesOptions{FarmID: f.ID} },
			[]string{"q-a", "q-b", "q-c"},
			3,
		},
		{"paused only", func(f store.Farm) store.ListQueuesOptions {
			return store.ListQueuesOptions{FarmID: f.ID, Paused: &yes}
		}, []string{"q-b"}, 1},
		{"active only", func(f store.Farm) store.ListQueuesOptions {
			return store.ListQueuesOptions{FarmID: f.ID, Paused: &no}
		}, []string{"q-a", "q-c"}, 2},
		{"name desc", func(f store.Farm) store.ListQueuesOptions {
			return store.ListQueuesOptions{FarmID: f.ID, SortBy: store.QueueSortByName, SortDir: store.SortDesc}
		}, []string{"q-c", "q-b", "q-a"}, 3},
		{"priority desc", func(f store.Farm) store.ListQueuesOptions {
			return store.ListQueuesOptions{FarmID: f.ID, SortBy: store.QueueSortByPriority, SortDir: store.SortDesc}
		}, []string{"q-a", "q-c", "q-b"}, 3},
		{"priority asc", func(f store.Farm) store.ListQueuesOptions {
			return store.ListQueuesOptions{FarmID: f.ID, SortBy: store.QueueSortByPriority}
		}, []string{"q-b", "q-c", "q-a"}, 3},
		{
			"created_at asc", func(f store.Farm) store.ListQueuesOptions {
				return store.ListQueuesOptions{FarmID: f.ID, SortBy: store.QueueSortByCreatedAt}
			},
			[]string{"q-b", "q-a", "q-c"},
			3,
		},
		{"page", func(f store.Farm) store.ListQueuesOptions {
			return store.ListQueuesOptions{FarmID: f.ID, Pagination: store.Pagination{Limit: 2, Offset: 1}}
		}, []string{"q-b", "q-c"}, 3},
		{"offset past the end", func(f store.Farm) store.ListQueuesOptions {
			return store.ListQueuesOptions{FarmID: f.ID, Pagination: store.Pagination{Limit: 2, Offset: 5}}
		}, []string{}, 3},
	}
	for name, st := range newStores(t) {
		farmA, _ := seedQueues(t, st)
		for _, tc := range cases {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				opts := tc.opts(farmA)
				if err := opts.Pagination.Validate(); err != nil {
					t.Fatalf("Validate: %v", err)
				}
				page, err := st.ListQueues(t.Context(), opts)
				if err != nil {
					t.Fatalf("ListQueues: %v", err)
				}
				if got := project(page.Items, func(q store.Queue) string { return q.Name }); !slices.Equal(got, tc.want) || page.Total != tc.wantTotal {
					t.Fatalf("ListQueues = %v (total %d), want %v (total %d)", got, page.Total, tc.want, tc.wantTotal)
				}
				if page.Items == nil || page.Limit != opts.Pagination.Limit || page.Offset != opts.Pagination.Offset {
					t.Fatalf("page window = items %v limit %d offset %d, want non-nil items, limit %d offset %d",
						page.Items, page.Limit, page.Offset, opts.Pagination.Limit, opts.Pagination.Offset)
				}
			})
		}
	}
}

func TestParity_DeleteQueue(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			farm := mustCreateFarm(t, st, "main")
			q := mustCreateQueue(t, st, store.Queue{FarmID: farm.ID, Name: "gone"})
			keep := mustCreateQueue(t, st, store.Queue{FarmID: farm.ID, Name: "kept"})
			if err := st.DeleteQueue(ctx, q.ID); err != nil {
				t.Fatalf("DeleteQueue: %v", err)
			}
			if _, err := st.GetQueue(ctx, q.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetQueue after delete: err = %v, want ErrNotFound", err)
			}
			if err := st.DeleteQueue(ctx, q.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("second delete: err = %v, want ErrNotFound", err)
			}
			if _, err := st.GetQueue(ctx, keep.ID); err != nil {
				t.Fatalf("other queue: %v, want it kept", err)
			}
			// The name is free again once its queue is gone.
			mustCreateQueue(t, st, store.Queue{FarmID: farm.ID, Name: "gone"})
		})
	}
}

// TestParity_DeleteFarmStillReferenced pins that a farm a queue still names is
// not deleted: the SQL stores refuse on the foreign key, and the fake must
// agree.
func TestParity_DeleteFarmStillReferenced(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			farm, _ := seedFarmQueue(t, st)
			if err := st.DeleteFarm(t.Context(), farm.ID); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("DeleteFarm with a queue = %v, want ErrConflict", err)
			}
			if _, err := st.GetFarm(t.Context(), farm.ID); err != nil {
				t.Fatalf("farm gone after a refused delete: %v", err)
			}
		})
	}
}
