// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

// End-to-end coverage for the auto-retry / failure-limit feature: a
// worker-reported "failed" task auto-re-queues (with backoff) until
// max_attempts genuine failures, then goes terminal-failed; a job auto-parks
// once its cumulative genuine failures reach failure_limit. Unit coverage for
// the scheduler fork itself lives in internal/scheduler/failure_test.go
// (TestHandleTaskFailed_RetriesUntilCeiling, TestHandleTaskFailed_
// ParksJobAtFailureLimit); these tests drive the same behavior through the
// real REST + NATS harness (server, scheduler, store, bus all wired up) the
// way a real worker would.

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// twoStepJobYAML is an OpenJD job with two independent steps (no
// dependency between them), each with a single trivial task. Used by the
// failure-limit park test: with a single worker and full-machine task cost
// (no declared required_cores — see internal/scheduler/lease.go
// fullMachineCost), a single lease request only ever leases one of the two
// ready tasks, so the other stays "ready" (non-terminal) after the leased one
// trips the failure limit. That keeps the job's step/job completion cascade
// from finalizing the job out from under the park (see
// internal/scheduler/taskstatus.go checkStepCompletion/checkJobCompletion).
const twoStepJobYAML = `specificationVersion: "jobtemplate-2023-09"
name: Park Test Job

steps:
  - name: StepA
    script:
      actions:
        onRun:
          command: echo
          args:
            - "step a"
  - name: StepB
    script:
      actions:
        onRun:
          command: echo
          args:
            - "step b"
`

// taskDetailResp is the subset of GET /api/v1/tasks/{id}'s response used by
// the retry test (mirrors internal/api/tasks.go's taskResponse).
type taskDetailResp struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	FailedAttempts int    `json:"failed_attempts"`
}

// jobDetailParkResp is the subset of GET /api/v1/jobs/{id}'s response used by
// the park test (mirrors internal/api/jobs.go's jobResponse).
type jobDetailParkResp struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	ParkReason     string `json:"park_reason,omitempty"`
	FailedAttempts int    `json:"failed_attempts"`
}

// submitJobWithParams posts yamlBody to POST /api/v1/jobs with farm_id,
// queue_id, owner, plus any additional query parameters in extra (e.g.
// max_attempts, retry_delay_seconds, failure_limit). Returns the created
// job's ID.
func submitJobWithParams(t *testing.T, ts *testServer, farmID, queueID, yamlBody string, extra map[string]string) string {
	t.Helper()

	q := url.Values{}
	q.Set("farm_id", farmID)
	q.Set("queue_id", queueID)
	q.Set("owner", "test")
	for k, v := range extra {
		q.Set(k, v)
	}

	fullURL := apiURL(ts, "/api/v1/jobs") + "?" + q.Encode()
	var resp jobResp
	mustDoJSON(t, http.MethodPost, fullURL, []byte(yamlBody), "application/x-yaml", http.StatusCreated, &resp)
	if resp.ID == "" {
		t.Fatal("submitJobWithParams: server returned empty job ID")
	}
	return resp.ID
}

// getTaskDetail fetches GET /api/v1/tasks/{id} and decodes it.
func getTaskDetail(t *testing.T, ts *testServer, taskID string) taskDetailResp {
	t.Helper()
	var resp taskDetailResp
	mustDoJSON(t, http.MethodGet, apiURL(ts, "/api/v1/tasks/"+taskID), nil, "", http.StatusOK, &resp)
	return resp
}

// getJobDetailPark fetches GET /api/v1/jobs/{id} and decodes the fields the
// park test needs.
func getJobDetailPark(t *testing.T, ts *testServer, jobID string) jobDetailParkResp {
	t.Helper()
	var resp jobDetailParkResp
	mustDoJSON(t, http.MethodGet, apiURL(ts, "/api/v1/jobs/"+jobID), nil, "", http.StatusOK, &resp)
	return resp
}

// TestAutoRetry_RetryThenSucceed submits a job with max_attempts=2 and a
// zero-second retry_delay whose task fails on its first attempt and
// succeeds on the second, then asserts:
//
//  1. The retried assignment targets the SAME task with a NEW attempt ID
//     (the task, not a fresh row, is re-leased — internal/store
//     RequeueTaskForRetry).
//  2. The job reaches "completed".
//  3. The task's failed_attempts == 1 (only the genuine first-attempt
//     failure counts; the successful second attempt does not).
func TestAutoRetry_RetryThenSucceed(t *testing.T) {
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)

	const workerID = "worker-retry-001"
	natsURL := "nats://" + ts.NATSAddr
	worker := newMockWorker(t, natsURL, workerID, farmID, queueID)
	worker.register()
	worker.startHeartbeat(5 * time.Second)
	pollWorkerOnline(t, ts, workerID, 10*time.Second)

	jobID := submitJobWithParams(t, ts, farmID, queueID, minimalJobYAML, map[string]string{
		"max_attempts":        "2",
		"retry_delay_seconds": "0",
	})
	t.Logf("submitted retry job %s", jobID)

	// First attempt: the worker reports a genuine failure.
	assign1 := worker.pullAssignment(15 * time.Second)
	worker.publishStatus(assign1, "running", nil)
	failCode := 1
	worker.publishStatus(assign1, "failed", &failCode)

	// With retry_delay_seconds=0 the scheduler re-queues the task with an
	// immediate RetryAfter and wakes parked lease waiters synchronously
	// (scheduler/failure.go scheduleRetryWake), so the retried attempt should
	// become leasable without any extra delay. pullAssignment's request
	// timeout exceeds the server's long-poll hold, so it safely rides out the
	// brief window between the "failed" publish and the scheduler processing
	// it.
	assign2 := worker.pullAssignment(15 * time.Second)
	if assign2.TaskID != assign1.TaskID {
		t.Fatalf("retry assignment task ID: got %q, want %q (same task, re-leased)", assign2.TaskID, assign1.TaskID)
	}
	if assign2.AttemptID == assign1.AttemptID {
		t.Fatalf("retry assignment should open a new attempt, got the same attempt ID %q", assign2.AttemptID)
	}

	// Second attempt: succeeds.
	worker.publishStatus(assign2, "running", nil)
	okCode := 0
	worker.publishStatus(assign2, "succeeded", &okCode)

	finalStatus := pollJobStatus(t, ts, jobID, []string{"completed", "failed", "canceled"}, 20*time.Second)
	if finalStatus != "completed" {
		t.Fatalf("job final status: got %q, want %q", finalStatus, "completed")
	}

	task := getTaskDetail(t, ts, assign1.TaskID)
	if task.Status != "succeeded" {
		t.Errorf("task status: got %q, want %q", task.Status, "succeeded")
	}
	if task.FailedAttempts != 1 {
		t.Errorf("task failed_attempts: got %d, want 1", task.FailedAttempts)
	}
}

// TestAutoRetry_ParksJobAtFailureLimit submits a two-independent-step job
// with failure_limit=1 whose leased task always fails, and asserts the job
// ends "paused" with a non-empty park_reason that survives the step/job
// completion cascade because the sibling step's task is still non-terminal
// (see twoStepJobYAML's doc comment).
func TestAutoRetry_ParksJobAtFailureLimit(t *testing.T) {
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)

	const workerID = "worker-park-001"
	natsURL := "nats://" + ts.NATSAddr
	worker := newMockWorker(t, natsURL, workerID, farmID, queueID)
	worker.register()
	worker.startHeartbeat(5 * time.Second)
	pollWorkerOnline(t, ts, workerID, 10*time.Second)

	jobID := submitJobWithParams(t, ts, farmID, queueID, twoStepJobYAML, map[string]string{
		"failure_limit": "1",
	})
	t.Logf("submitted park job %s", jobID)

	// The worker's single lease request only ever leases one of the two
	// ready tasks (full-machine cost, one worker) — fail whichever one the
	// scheduler picked.
	assign := worker.pullAssignment(15 * time.Second)
	worker.publishStatus(assign, "running", nil)
	failCode := 1
	worker.publishStatus(assign, "failed", &failCode)

	// Poll across every terminal-ish status so a design regression (e.g. the
	// completion cascade overwriting the park) fails fast with a clear
	// "got failed, want paused" message instead of timing out blind.
	finalStatus := pollJobStatus(t, ts, jobID, []string{"paused", "completed", "failed", "canceled"}, 20*time.Second)
	if finalStatus != "paused" {
		t.Fatalf("job final status: got %q, want %q", finalStatus, "paused")
	}

	job := getJobDetailPark(t, ts, jobID)
	if job.Status != "paused" {
		t.Errorf("job status: got %q, want %q", job.Status, "paused")
	}
	if job.ParkReason == "" {
		t.Error("job park_reason: got empty, want non-empty")
	}
	if !strings.Contains(job.ParkReason, "failure limit") {
		t.Errorf("job park_reason: got %q, want it to mention the failure limit", job.ParkReason)
	}
}
