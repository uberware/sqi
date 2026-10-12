// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
)

// world is one farm, queue, pending job, ready step and n ready tasks, with
// the named workers registered online.
type world struct {
	FarmID, QueueID, JobID, StepID string
	Tasks, Workers                 []string
}

func seedWorld(t *testing.T, st store.Store, nTasks int, workers ...string) world {
	t.Helper()
	ctx := t.Context()
	farm, err := st.CreateFarm(ctx, store.Farm{ID: "farm-" + uuid.NewString()[:8], Name: "f-" + uuid.NewString()[:8]})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err := st.CreateQueue(ctx, store.Queue{ID: "queue-" + uuid.NewString()[:8], FarmID: farm.ID, Name: "q"})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	w := world{FarmID: farm.ID, QueueID: queue.ID, JobID: "job-" + uuid.NewString()[:8], StepID: "step-" + uuid.NewString()[:8]}
	seed := storetest.NewJob(w.JobID, farm.ID, queue.ID).Step(w.StepID, store.StepStatusReady)
	for i := range nTasks {
		id := fmt.Sprintf("%s-t%d", w.JobID, i)
		seed.Task(id, w.StepID, store.TaskStatusReady)
		w.Tasks = append(w.Tasks, id)
	}
	seed.Submit(t, st)
	now := time.Now().UTC()
	for _, id := range workers {
		if _, _, err := st.RegisterWorker(ctx, store.Worker{
			ID: id, FarmID: farm.ID, Hostname: id, Status: store.WorkerStatusOnline, LastHeartbeatAt: &now,
		}); err != nil {
			t.Fatalf("RegisterWorker %s: %v", id, err)
		}
		w.Workers = append(w.Workers, id)
	}
	return w
}

func leaseReq(taskID, workerID string) store.LeaseRequest {
	return store.LeaseRequest{TaskID: taskID, WorkerID: workerID, AttemptID: uuid.NewString(), Now: time.Now().UTC()}
}
