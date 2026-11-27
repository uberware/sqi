// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/server"
)

// TestResumeQueueLeasesAParkedRequestAtOnce drives a worker whose lease
// request is parked on a paused queue that has work: resuming the queue
// through POST /queues/{id}/resume must answer that request at once, not
// after the heartbeat sweep or the lease hold, which this server sets far
// beyond the test's deadline.
func TestResumeQueueLeasesAParkedRequestAtOnce(t *testing.T) {
	ts := startServerWith(t, func(c *server.Config) {
		c.Scheduler.HeartbeatSweepInterval = 10 * time.Minute
	})
	farmID, queueID := seedFarmAndQueue(t, ts)
	client := clientFor(ts)
	mustDoJSONClient(t, client, http.MethodPost, apiURL(ts, "/api/v1/queues/"+queueID+"/pause"),
		nil, "", http.StatusOK, nil)

	const workerID = "worker-queue-resume"
	worker := newMockWorker(t, "nats://"+ts.NATSAddr, workerID, farmID, queueID)
	worker.register()
	worker.startHeartbeat(5 * time.Second)
	pollWorkerOnline(t, ts, workerID, 10*time.Second)
	submitJob(t, ts, farmID, queueID)

	const parkFor = time.Second
	type resume struct {
		at  time.Time
		err error
	}
	resumed := make(chan resume, 1)
	go func() {
		time.Sleep(parkFor)
		r := resume{at: time.Now()}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			apiURL(ts, "/api/v1/queues/"+queueID+"/resume"), nil)
		var resp *http.Response
		if err == nil {
			resp, err = client.Do(req)
		}
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				err = fmt.Errorf("status %d, want %d", resp.StatusCode, http.StatusOK)
			}
		}
		r.err = err
		resumed <- r
	}()

	assign := worker.pullAssignment(parkFor + 5*time.Second)
	leasedAt := time.Now()
	r := <-resumed
	if r.err != nil {
		t.Fatalf("resume: %v", r.err)
	}
	if leasedAt.Before(r.at) {
		t.Fatalf("task %s was leased before the queue was resumed", assign.TaskID)
	}
}
