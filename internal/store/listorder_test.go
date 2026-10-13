// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"slices"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
)

// seedFarmQueue creates the farm and queue a job row references.
func seedFarmQueue(t *testing.T, st store.Store) (store.Farm, store.Queue) {
	t.Helper()
	farm := mustCreateFarm(t, st, "order-farm")
	queue := mustCreateQueue(t, st, store.Queue{FarmID: farm.ID, Name: "order-queue"})
	return farm, queue
}

// seedJobRow submits a one-job graph with no steps, named name.
func seedJobRow(t *testing.T, st store.Store, id, farmID, queueID, name string) {
	t.Helper()
	seed := storetest.NewJob(id, farmID, queueID)
	seed.Sub.Job.Name = name
	seed.Submit(t, st)
}

// TestListWorkers_NullHeartbeatOrderAndTies pins the order every backend
// returns workers in when sorted by a nullable column: a worker with no
// heartbeat sorts first ascending and last descending (SQLite's own NULL order,
// which PostgreSQL reverses unless told), and workers that tie on the sort
// column come back in id order, so pages never overlap or skip a row.
func TestListWorkers_NullHeartbeatOrderAndTies(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			beat := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			// w-c and w-a tie on heartbeat; w-b has none.
			for _, w := range []store.Worker{
				{ID: "w-c", Hostname: "h", Status: store.WorkerStatusOnline, LastHeartbeatAt: &beat},
				{ID: "w-b", Hostname: "h", Status: store.WorkerStatusOffline},
				{ID: "w-a", Hostname: "h", Status: store.WorkerStatusOnline, LastHeartbeatAt: &beat},
			} {
				if _, _, err := st.RegisterWorker(ctx, w); err != nil {
					t.Fatalf("RegisterWorker %s: %v", w.ID, err)
				}
			}
			ids := func(dir store.SortDir, limit, offset int) []string {
				t.Helper()
				page, err := st.ListWorkers(ctx, store.ListWorkersOptions{
					SortBy: store.WorkerSortByLastHeartbeatAt, SortDir: dir,
					Pagination: store.Pagination{Limit: limit, Offset: offset},
				})
				if err != nil {
					t.Fatalf("ListWorkers: %v", err)
				}
				var out []string
				for _, w := range page.Items {
					out = append(out, w.ID)
				}
				return out
			}
			if got, want := ids(store.SortAsc, 10, 0), []string{"w-b", "w-a", "w-c"}; !slices.Equal(got, want) {
				t.Errorf("ascending = %v, want %v", got, want)
			}
			if got, want := ids(store.SortDesc, 10, 0), []string{"w-a", "w-c", "w-b"}; !slices.Equal(got, want) {
				t.Errorf("descending = %v, want %v", got, want)
			}
			// Hostname ties on every row: id order decides, page by page.
			var paged []string
			for off := range 3 {
				page, err := st.ListWorkers(ctx, store.ListWorkersOptions{
					SortBy: store.WorkerSortByHostname, Pagination: store.Pagination{Limit: 1, Offset: off},
				})
				if err != nil {
					t.Fatalf("ListWorkers: %v", err)
				}
				for _, w := range page.Items {
					paged = append(paged, w.ID)
				}
			}
			if want := []string{"w-a", "w-b", "w-c"}; !slices.Equal(paged, want) {
				t.Errorf("hostname pages = %v, want %v", paged, want)
			}
		})
	}
}

// TestListJobs_TiesBreakByID pins the id tie-break on the job list: three
// jobs with one name, listed by name one page at a time.
func TestListJobs_TiesBreakByID(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			farm, queue := seedFarmQueue(t, st)
			for _, id := range []string{"job-c", "job-a", "job-b"} {
				seedJobRow(t, st, id, farm.ID, queue.ID, "same")
			}
			var got []string
			for off := range 3 {
				page, err := st.ListJobs(t.Context(), store.ListJobsOptions{
					SortBy: store.JobSortByName, Pagination: store.Pagination{Limit: 1, Offset: off},
				})
				if err != nil {
					t.Fatalf("ListJobs: %v", err)
				}
				for _, j := range page.Items {
					got = append(got, j.ID)
				}
			}
			if want := []string{"job-a", "job-b", "job-c"}; !slices.Equal(got, want) {
				t.Errorf("pages = %v, want %v", got, want)
			}
		})
	}
}
