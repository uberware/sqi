// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"slices"
	"testing"

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

func queueNames(qs []store.Queue) []string {
	names := make([]string, 0, len(qs))
	for _, q := range qs {
		names = append(names, q.Name)
	}
	return names
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
			var names []string
			for _, f := range got {
				names = append(names, f.Name)
			}
			if want := []string{"berlin", "lima", "tokyo"}; !slices.Equal(names, want) {
				t.Fatalf("names = %v, want %v (ordered by name)", names, want)
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

// seedQueues creates two farms: farm A with queues q-b (priority 10, paused),
// q-a (30) and q-c (20), created in that order, and farm B with q-d.
func seedQueues(t *testing.T, st store.Store) (farmA, farmB store.Farm) {
	t.Helper()
	farmA, farmB = mustCreateFarm(t, st, "a"), mustCreateFarm(t, st, "b")
	mustCreateQueue(t, st, store.Queue{FarmID: farmA.ID, Name: "q-b", Priority: 10, Paused: true})
	mustCreateQueue(t, st, store.Queue{FarmID: farmA.ID, Name: "q-a", Priority: 30})
	mustCreateQueue(t, st, store.Queue{FarmID: farmA.ID, Name: "q-c", Priority: 20})
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
		// fakeSkip, when set, is a divergence the fake does not match.
		fakeSkip string
	}{
		{
			"all farms by name", func(store.Farm) store.ListQueuesOptions { return store.ListQueuesOptions{} },
			[]string{"q-a", "q-b", "q-c", "q-d"},
			4, "",
		},
		{
			"farm filter", func(f store.Farm) store.ListQueuesOptions { return store.ListQueuesOptions{FarmID: f.ID} },
			[]string{"q-a", "q-b", "q-c"},
			3, "",
		},
		{"paused only", func(f store.Farm) store.ListQueuesOptions {
			return store.ListQueuesOptions{FarmID: f.ID, Paused: &yes}
		}, []string{"q-b"}, 1, ""},
		{"active only", func(f store.Farm) store.ListQueuesOptions {
			return store.ListQueuesOptions{FarmID: f.ID, Paused: &no}
		}, []string{"q-a", "q-c"}, 2, ""},
		{"name desc", func(f store.Farm) store.ListQueuesOptions {
			return store.ListQueuesOptions{FarmID: f.ID, SortBy: store.QueueSortByName, SortDir: store.SortDesc}
		}, []string{"q-c", "q-b", "q-a"}, 3, ""},
		{"priority desc", func(f store.Farm) store.ListQueuesOptions {
			return store.ListQueuesOptions{FarmID: f.ID, SortBy: store.QueueSortByPriority, SortDir: store.SortDesc}
		}, []string{"q-a", "q-c", "q-b"}, 3, ""},
		{"priority asc", func(f store.Farm) store.ListQueuesOptions {
			return store.ListQueuesOptions{FarmID: f.ID, SortBy: store.QueueSortByPriority}
		}, []string{"q-b", "q-c", "q-a"}, 3, ""},
		{
			"created_at asc", func(f store.Farm) store.ListQueuesOptions {
				return store.ListQueuesOptions{FarmID: f.ID, SortBy: store.QueueSortByCreatedAt}
			},
			[]string{"q-b", "q-a", "q-c"},
			3,
			"divergence: fake CreateQueue keeps the caller's (zero) CreatedAt, so created_at order is map order",
		},
		{"page", func(f store.Farm) store.ListQueuesOptions {
			return store.ListQueuesOptions{FarmID: f.ID, Pagination: store.Pagination{Limit: 2, Offset: 1}}
		}, []string{"q-b", "q-c"}, 3, ""},
		{"offset past the end", func(f store.Farm) store.ListQueuesOptions {
			return store.ListQueuesOptions{FarmID: f.ID, Pagination: store.Pagination{Limit: 2, Offset: 5}}
		}, []string{}, 3, ""},
	}
	for name, st := range newStores(t) {
		farmA, _ := seedQueues(t, st)
		for _, tc := range cases {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				if name == "fake" && tc.fakeSkip != "" {
					t.Skip(tc.fakeSkip)
				}
				opts := tc.opts(farmA)
				if err := opts.Pagination.Validate(); err != nil {
					t.Fatalf("Validate: %v", err)
				}
				page, err := st.ListQueues(t.Context(), opts)
				if err != nil {
					t.Fatalf("ListQueues: %v", err)
				}
				if got := queueNames(page.Items); !slices.Equal(got, tc.want) || page.Total != tc.wantTotal {
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
