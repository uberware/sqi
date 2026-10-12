// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
)

// TestCreateJobSubmission_ConcurrentObserverSeesWholeJobs: while submitters
// write jobs (three steps of 50 tasks, every second one blocked on the one
// before), an observer polling throughout never sees a job missing a step or a
// task, nor a blocked job without its dependency edge. SQLite gets this from its
// single writer; PostgreSQL must get it from the one transaction.
func TestCreateJobSubmission_ConcurrentObserverSeesWholeJobs(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			farm, queue := seedFarmQueue(t, st)
			const jobs, steps, tasksPerStep = 20, 3, 50
			var stop atomic.Bool
			var observed atomic.Int64 // job sightings, one per job per poll
			var wg sync.WaitGroup
			seen := map[string]bool{} // only the observer touches it until wg.Wait
			wg.Go(func() {
				for !stop.Load() {
					page, err := st.ListJobs(t.Context(), store.ListJobsOptions{Pagination: store.Pagination{Limit: 1000}})
					if err != nil {
						t.Errorf("ListJobs: %v", err)
						return
					}
					for _, j := range page.Items {
						checkWholeJob(t, st, j, steps, tasksPerStep)
						seen[j.ID] = true
						observed.Add(1)
					}
				}
			})
			// The observer calls t.Errorf, so it must be finished before this
			// subtest returns, including when a submission fails the test.
			finish := func() {
				stop.Store(true)
				wg.Wait()
			}
			defer finish()

			var prev string
			for i := range jobs {
				id := fmt.Sprintf("obs-%02d", i)
				seed := storetest.NewJob(id, farm.ID, queue.ID)
				if i%2 == 1 {
					seed.As(store.JobStatusBlocked)
					seed.Sub.DependsOn = []string{prev}
				}
				for s := range steps {
					sid := fmt.Sprintf("%s-s%d", id, s)
					seed.Step(sid, store.StepStatusPending)
					for k := range tasksPerStep {
						seed.Task(fmt.Sprintf("%s-%d", sid, k), sid, store.TaskStatusPending)
					}
				}
				seed.Submit(t, st)
				prev = id
			}
			finish()
			if observed.Load() == 0 {
				t.Fatal("the observer never saw a job; the test checked nothing")
			}
			t.Logf("the observer made %d sightings of %d distinct jobs", observed.Load(), len(seen))

			// Once quiet, every submitted job is whole, the last included.
			page, err := st.ListJobs(t.Context(), store.ListJobsOptions{Pagination: store.Pagination{Limit: 1000}})
			if err != nil {
				t.Fatalf("ListJobs: %v", err)
			}
			if len(page.Items) != jobs {
				t.Fatalf("%d jobs after the submissions, want %d", len(page.Items), jobs)
			}
			for _, j := range page.Items {
				checkWholeJob(t, st, j, steps, tasksPerStep)
			}
		})
	}
}

func checkWholeJob(t *testing.T, st store.Store, j store.Job, steps, tasksPerStep int) {
	t.Helper()
	ss, err := st.ListSteps(t.Context(), j.ID)
	if err != nil {
		t.Errorf("ListSteps %s: %v", j.ID, err)
		return
	}
	counts, err := st.CountTasksByJob(t.Context(), j.ID)
	if err != nil {
		t.Errorf("CountTasksByJob %s: %v", j.ID, err)
		return
	}
	total := 0
	for _, n := range counts {
		total += n
	}
	if len(ss) != steps || total != steps*tasksPerStep {
		t.Errorf("job %s visible with %d steps and %d tasks, want %d and %d", j.ID, len(ss), total, steps, steps*tasksPerStep)
	}
	if j.Status == store.JobStatusBlocked {
		deps, err := st.ListJobDependencyIDs(t.Context(), j.ID)
		if err != nil || len(deps) == 0 {
			t.Errorf("blocked job %s visible without its dependency edge (err %v)", j.ID, err)
		}
	}
}
