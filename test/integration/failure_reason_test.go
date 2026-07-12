// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

// End-to-end coverage for the failure-reason-visibility feature: a
// worker-reported failure Message is persisted as the task's
// [store.Task.FailureReason] and rolled up into the job detail's
// failure_summary, all reachable via the REST API exactly as a UI client
// would observe it. Unit/handler-level coverage for the plumbing itself
// lives in internal/scheduler/taskstatus_test.go, internal/store, and
// internal/api/{tasks_test.go,jobs_test.go}; this test drives the same
// behavior through the real REST + NATS harness (server, scheduler, store,
// bus all wired up) the way a real worker would.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// taskFailureResp is the subset of GET /api/v1/tasks/{id}'s response used by
// this test (mirrors internal/api/tasks.go's taskResponse).
type taskFailureResp struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	FailureReason string `json:"failure_reason,omitempty"`
}

// failureSummaryResp mirrors internal/api/jobs.go's failureSummaryResponse.
type failureSummaryResp struct {
	FailedCount     int    `json:"failed_count"`
	DominantReason  string `json:"dominant_reason"`
	DistinctReasons int    `json:"distinct_reasons"`
}

// jobFailureDetailResp is the subset of GET /api/v1/jobs/{id}'s response used
// by this test (mirrors internal/api/jobs.go's jobResponse).
type jobFailureDetailResp struct {
	ID             string              `json:"id"`
	Status         string              `json:"status"`
	FailureSummary *failureSummaryResp `json:"failure_summary,omitempty"`
}

// getTaskFailure fetches GET /api/v1/tasks/{id} and decodes the fields this
// test needs.
func getTaskFailure(t *testing.T, ts *testServer, taskID string) taskFailureResp {
	t.Helper()
	var resp taskFailureResp
	mustDoJSON(t, http.MethodGet, apiURL(ts, "/api/v1/tasks/"+taskID), nil, "", http.StatusOK, &resp)
	return resp
}

// getJobFailureDetail fetches GET /api/v1/jobs/{id} and decodes the fields
// this test needs.
func getJobFailureDetail(t *testing.T, ts *testServer, jobID string) jobFailureDetailResp {
	t.Helper()
	var resp jobFailureDetailResp
	mustDoJSON(t, http.MethodGet, apiURL(ts, "/api/v1/jobs/"+jobID), nil, "", http.StatusOK, &resp)
	return resp
}

// taskAttemptResp is the subset of one item in GET /api/v1/tasks/{id}/attempts'
// response used by tests (mirrors internal/api/tasks.go's taskAttemptResponse).
type taskAttemptResp struct {
	AttemptNumber int    `json:"attempt_number"`
	Status        string `json:"status"`
	Message       string `json:"message,omitempty"`
}

// taskAttemptsResp mirrors internal/api/tasks.go's taskAttemptsResponse.
type taskAttemptsResp struct {
	Items []taskAttemptResp `json:"items"`
}

// getTaskAttempts fetches GET /api/v1/tasks/{id}/attempts and decodes the
// fields this test needs.
func getTaskAttempts(t *testing.T, ts *testServer, taskID string) taskAttemptsResp {
	t.Helper()
	var resp taskAttemptsResp
	mustDoJSON(t, http.MethodGet, apiURL(ts, "/api/v1/tasks/"+taskID+"/attempts"), nil, "", http.StatusOK, &resp)
	return resp
}

// publishStatusWithMessage publishes a [protocol.TaskStatusMsg] to
// task.status.<jobID> carrying a Message, exercising the same field a real
// worker's failure report populates (see internal/worker's failPreExec /
// process-exit reporting). The harness's publishStatus helper (used by the
// auto-retry tests) never sets Message, so this test publishes directly
// rather than widening that shared helper's signature.
func publishStatusWithMessage(t *testing.T, w *mockWorker, assign protocol.AssignMsg, status, message string, exitCode *int) {
	t.Helper()

	msg := protocol.TaskStatusMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeTaskStatus,
		TaskID:    assign.TaskID,
		AttemptID: assign.AttemptID,
		JobID:     assign.JobID,
		Status:    status,
		ExitCode:  exitCode,
		SessionID: "test-session-" + assign.TaskID,
		At:        time.Now().UTC(),
		Message:   message,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("publishStatusWithMessage(%s): marshal: %v", status, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := w.js.Publish(ctx, bus.TaskStatusSubject(assign.JobID), data); err != nil {
		t.Fatalf("publishStatusWithMessage(%s): publish: %v", status, err)
	}
}

// TestTaskFailureReason_VisibleEndToEnd submits a job with max_attempts=1 (so
// a single genuine failure is immediately terminal — no retry backoff to
// wait out) whose task the mock worker reports "failed" with a known,
// worker-supplied Message. It asserts the failure reason is threaded all the
// way through: the task's REST failure_reason equals the worker's message,
// and the job detail's failure_summary reflects it as the dominant_reason
// with failed_count >= 1.
func TestTaskFailureReason_VisibleEndToEnd(t *testing.T) {
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)

	const workerID = "worker-failreason-001"
	natsURL := "nats://" + ts.NATSAddr
	worker := newMockWorker(t, natsURL, workerID, farmID, queueID)
	worker.register()
	worker.startHeartbeat(5 * time.Second)
	pollWorkerOnline(t, ts, workerID, 10*time.Second)

	const wantReason = "simulated deterministic worker failure: license check failed"

	jobID := submitJobWithParams(t, ts, farmID, queueID, minimalJobYAML, map[string]string{
		"max_attempts":        "1",
		"retry_delay_seconds": "0",
	})
	t.Logf("submitted failure-reason job %s", jobID)

	assign := worker.pullAssignment(15 * time.Second)
	worker.publishStatus(assign, "running", nil)

	failCode := 1
	publishStatusWithMessage(t, worker, assign, "failed", wantReason, &failCode)

	finalStatus := pollJobStatus(t, ts, jobID, []string{"completed", "failed", "canceled", "paused"}, 20*time.Second)
	if finalStatus != "failed" {
		t.Fatalf("job final status: got %q, want %q", finalStatus, "failed")
	}

	task := getTaskFailure(t, ts, assign.TaskID)
	if task.Status != "failed" {
		t.Errorf("task status: got %q, want %q", task.Status, "failed")
	}
	if task.FailureReason != wantReason {
		t.Errorf("task failure_reason: got %q, want %q", task.FailureReason, wantReason)
	}

	job := getJobFailureDetail(t, ts, jobID)
	if job.FailureSummary == nil {
		t.Fatal("job failure_summary: got nil, want non-nil")
	}
	if job.FailureSummary.FailedCount < 1 {
		t.Errorf("job failure_summary.failed_count: got %d, want >= 1", job.FailureSummary.FailedCount)
	}
	if job.FailureSummary.DominantReason != wantReason {
		t.Errorf("job failure_summary.dominant_reason: got %q, want %q", job.FailureSummary.DominantReason, wantReason)
	}
	if !strings.Contains(job.FailureSummary.DominantReason, "license check failed") {
		t.Errorf("job failure_summary.dominant_reason: got %q, want it to mention the failure", job.FailureSummary.DominantReason)
	}
}
