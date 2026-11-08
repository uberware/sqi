// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

// drain_heartbeat_test.go — a real sqi-worker keeps heartbeating while it
// drains, so a drain that outlasts the server's heartbeat timeout does not
// lose the task it is waiting for.

import (
	"os"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/server"
	"github.com/uberware/sqi/internal/store"
)

const drainHeartbeatJobYAML = `specificationVersion: "jobtemplate-2023-09"
name: Drain Heartbeat Test
steps:
  - name: Run
    script:
      actions:
        onRun:
          command: sleep
          args:
            - "8"
`

// TestWorkerDrainLongerThanHeartbeatTimeoutKeepsTask signals a real worker
// while it runs an 8 s task, against a server that declares a worker offline
// after 3 s without a heartbeat and a worker that drains for up to 20 s. The
// worker must keep heartbeating through the drain: the task ends succeeded,
// on its one attempt, and is never reclaimed as "worker went offline".
func TestWorkerDrainLongerThanHeartbeatTimeoutKeepsTask(t *testing.T) {
	ts := startServerWith(t, func(c *server.Config) {
		c.Scheduler.WorkerTimeout = 3 * time.Second
		c.Scheduler.HeartbeatSweepInterval = 500 * time.Millisecond
		// The server's default grace (minutes, sized for a worker's redial
		// backoff) would hold the sweep off for longer than this test runs.
		c.Scheduler.BusReconnectGrace = 2 * time.Second
	})
	farmID, queueID := seedFarmAndQueue(t, ts)

	worker := startRealWorkerNoWait(t, ts, farmID, queueID, nil, []string{
		"SQI_WORKER_SHUTDOWN_GRACE_PERIOD=20s",
	}, false)
	if findOnlineWorker(t, ts, farmID, 30*time.Second) == "" {
		t.Fatal("the worker did not come online")
	}

	jobID := submitJobWithParams(t, ts, farmID, queueID, drainHeartbeatJobYAML, map[string]string{
		"max_attempts":        "3",
		"retry_delay_seconds": "0",
	})
	taskID := firstTaskID(t, ts, jobID)

	deadline := time.Now().Add(30 * time.Second)
	for getTaskDetail(t, ts, taskID).Status != "running" {
		if time.Now().After(deadline) {
			t.Fatalf("task %s never started running", taskID)
		}
		time.Sleep(100 * time.Millisecond)
	}

	if err := worker.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal the worker: %v", err)
	}

	final := pollJobStatus(t, ts, jobID, []string{"completed", "failed", "canceled", "paused"}, 60*time.Second)
	if final != "completed" {
		t.Fatalf("job final status = %q, want completed", final)
	}

	task := getTaskFailure(t, ts, taskID)
	if task.Status != "succeeded" {
		t.Errorf("task status = %q, want succeeded", task.Status)
	}
	attempts := getTaskAttempts(t, ts, taskID)
	if len(attempts.Items) != 1 {
		t.Errorf("task has %d attempts, want exactly 1: %+v", len(attempts.Items), attempts.Items)
	}
	for _, a := range attempts.Items {
		if a.Message == store.FailureReasonWorkerOffline {
			t.Errorf("attempt %d was closed with %q: the server reclaimed the task while the worker drained", a.AttemptNumber, a.Message)
		}
	}
}
