// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"slices"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
)

func TestParity_ListOpenAttempts(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedStepA(t, st, graphOpts{}, store.TaskStatusReady, store.TaskStatusReady,
				store.TaskStatusReady, store.TaskStatusReady)
			tasks := g.Tasks["a"]
			// Each lease is stamped a millisecond after the last, so created_at
			// order is not left to a coarse clock and a random attempt-ID tiebreak.
			base := time.Now().UTC().Add(-time.Minute)
			at := func(task store.Task, i int) store.LeaseRequest {
				req := leaseReq(task)
				req.Now = base.Add(time.Duration(i) * time.Millisecond)
				return req
			}
			assigned := storetest.Lease(t, st, at(tasks[0], 0))
			running := storetest.Running(t, st, at(tasks[1], 1))
			done := storetest.Running(t, st, at(tasks[2], 2))
			storetest.Complete(t, st, done, store.TaskStatusSucceeded) // closed
			// A superseded attempt: requeued, then leased again; only the new
			// attempt is open.
			old := storetest.FailAndRequeue(t, st, at(tasks[3], 3), base)
			fresh := storetest.Running(t, st, at(tasks[3], 4))

			got, err := st.ListOpenAttempts(t.Context(), time.Now().UTC().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			ids := make([]string, 0, len(got))
			for _, a := range got {
				ids = append(ids, a.AttemptID)
				if a.WorkerID == "" || a.TaskID == "" || a.CreatedAt.IsZero() {
					t.Errorf("incomplete row %+v", a)
				}
			}
			want := []string{assigned.ID, running.ID, fresh.ID}
			if !slices.Equal(ids, want) {
				t.Fatalf("open = %v, want %v (in created_at order; not %s, %s)", ids, want, done.ID, old.ID)
			}

			none, err := st.ListOpenAttempts(t.Context(), assigned.CreatedAt)
			if err != nil || len(none) != 0 {
				t.Fatalf("cutoff at the oldest = %v, %v; want none (strictly before)", none, err)
			}
		})
	}
}
