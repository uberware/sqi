// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ── Minimal OpenJD template used by all e2e tests ─────────────────────────────

// minimalJobYAML is a single-step, single-task OpenJD job with no parameter
// space and a trivial echo command.  The command is never actually executed —
// the mock worker skips process execution and publishes status messages
// directly — so the command just needs to be syntactically valid.
const minimalJobYAML = `specificationVersion: "jobtemplate-2023-09"
name: Integration Test Job

steps:
  - name: Run
    script:
      actions:
        onRun:
          command: echo
          args:
            - "hello from sqi integration test"
`

// ── Shared HTTP client ────────────────────────────────────────────────────────

// httpClient is used by all REST helpers.  The 10-second timeout prevents
// tests from hanging indefinitely if the server is wedged.
var httpClient = &http.Client{Timeout: 10 * time.Second}

// ── Wire types for REST assertions ────────────────────────────────────────────

// These mirror the internal api package's unexported response types so we can
// decode REST responses without importing the api package (which would create
// a cross-package dependency from test/ into internal/).

type jobResp struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Name   string `json:"name"`
}

type workerResp struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type workerListResp struct {
	Items []workerResp `json:"items"`
	Total int          `json:"total"`
}

// ── REST helpers ──────────────────────────────────────────────────────────────

// apiURL builds a full URL for the test server.
func apiURL(ts *testServer, path string) string {
	return "http://" + ts.HTTPAddr + path
}

// mustDoJSON performs an HTTP request and decodes the JSON response body into
// dst.  It fails the test on any transport or status error outside the
// expected status code set.
func mustDoJSON(t *testing.T, method, url string, body []byte, contentType string, expectStatus int, dst any) {
	t.Helper()

	var reqBody *bytes.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	} else {
		reqBody = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(context.Background(), method, url, reqBody)
	if err != nil {
		t.Fatalf("mustDoJSON: NewRequest %s %s: %v", method, url, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("mustDoJSON: %s %s: %v", method, url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != expectStatus {
		var buf bytes.Buffer
		if _, readErr := buf.ReadFrom(resp.Body); readErr != nil {
			t.Logf("mustDoJSON: read error body: %v", readErr)
		}
		t.Fatalf("mustDoJSON: %s %s: got status %d, want %d\nbody: %s",
			method, url, resp.StatusCode, expectStatus, buf.String())
	}

	if dst != nil {
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			t.Fatalf("mustDoJSON: decode response from %s %s: %v", method, url, err)
		}
	}
}

// submitJob posts minimalJobYAML to POST /api/v1/jobs and returns the created
// job's ID.
func submitJob(t *testing.T, ts *testServer, farmID, queueID string) string {
	t.Helper()
	url := fmt.Sprintf("%s?farm_id=%s&queue_id=%s&owner=test", apiURL(ts, "/api/v1/jobs"), farmID, queueID)
	var resp jobResp
	mustDoJSON(t, http.MethodPost, url, []byte(minimalJobYAML), "application/x-yaml", http.StatusCreated, &resp)
	if resp.ID == "" {
		t.Fatal("submitJob: server returned empty job ID")
	}
	return resp.ID
}

// pollJobStatus polls GET /api/v1/jobs/{id} until the job reaches one of the
// target statuses or timeout elapses.  Returns the final status reached.
func pollJobStatus(t *testing.T, ts *testServer, jobID string, targets []string, timeout time.Duration) string {
	t.Helper()
	targetSet := make(map[string]bool, len(targets))
	for _, s := range targets {
		targetSet[s] = true
	}

	var lastStatus string
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var resp jobResp
		mustDoJSON(t, http.MethodGet, apiURL(ts, "/api/v1/jobs/"+jobID), nil, "", http.StatusOK, &resp)
		lastStatus = resp.Status
		if targetSet[resp.Status] {
			return resp.Status
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("pollJobStatus: job %s stuck at %q; wanted one of %v after %s",
		jobID, lastStatus, targets, timeout)
	return ""
}

// pollWorkerOnline polls GET /api/v1/workers until at least one online worker
// is visible, or timeout elapses.
func pollWorkerOnline(t *testing.T, ts *testServer, workerID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var resp workerListResp
		mustDoJSON(t, http.MethodGet, apiURL(ts, "/api/v1/workers"), nil, "", http.StatusOK, &resp)
		for _, w := range resp.Items {
			if w.ID == workerID && w.Status == "online" {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("pollWorkerOnline: worker %s did not appear online within %s", workerID, timeout)
}

// seedFarmAndQueue creates a farm and queue via the REST API and returns their IDs.
// Farm and queue IDs are generated by the server (UUIDs); this function reads
// them from the creation responses.
func seedFarmAndQueue(t *testing.T, ts *testServer) (farmID, queueID string) {
	t.Helper()

	// POST /api/v1/farms — ID is server-generated; only name is required.
	farmBody := []byte(`{"name":"Integration Farm"}`)
	var farmResp struct {
		ID string `json:"id"`
	}
	mustDoJSON(t, http.MethodPost, apiURL(ts, "/api/v1/farms"), farmBody, "application/json", http.StatusCreated, &farmResp)
	if farmResp.ID == "" {
		t.Fatal("seedFarmAndQueue: server returned empty farm ID")
	}

	// POST /api/v1/queues — farm_id references the farm we just created.
	queueBody, err := json.Marshal(map[string]any{
		"farm_id": farmResp.ID,
		"name":    "Integration Queue",
	})
	if err != nil {
		t.Fatalf("seedFarmAndQueue: marshal queue body: %v", err)
	}
	var queueResp struct {
		ID string `json:"id"`
	}
	mustDoJSON(t, http.MethodPost, apiURL(ts, "/api/v1/queues"), queueBody, "application/json", http.StatusCreated, &queueResp)
	if queueResp.ID == "" {
		t.Fatal("seedFarmAndQueue: server returned empty queue ID")
	}

	return farmResp.ID, queueResp.ID
}

// pollTaskLogs polls GET /api/v1/tasks/{id}/logs until the concatenated log
// output contains wantSubstr, or timeout elapses.  This avoids a fixed sleep
// between the job completing and the log-ingestion consumer persisting chunks
// (the two events flow through different NATS consumers: SQI_TASK and SQI_LOGS).
func pollTaskLogs(t *testing.T, ts *testServer, taskID, wantSubstr string, timeout time.Duration) string {
	t.Helper()
	var lastLogs string
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var resp struct {
			Items []struct {
				Data string `json:"data"`
			} `json:"items"`
		}
		mustDoJSON(t, http.MethodGet, apiURL(ts, "/api/v1/tasks/"+taskID+"/logs"), nil, "", http.StatusOK, &resp)
		var sb strings.Builder
		for _, item := range resp.Items {
			sb.WriteString(item.Data)
		}
		lastLogs = sb.String()
		if strings.Contains(lastLogs, wantSubstr) {
			return lastLogs
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("pollTaskLogs: expected %q in logs for task %s after %s; got: %q",
		wantSubstr, taskID, timeout, lastLogs)
	return ""
}

// firstTaskID fetches the task list for a job and returns the first task ID.
func firstTaskID(t *testing.T, ts *testServer, jobID string) string {
	t.Helper()
	var resp struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	mustDoJSON(t, http.MethodGet, apiURL(ts, "/api/v1/jobs/"+jobID+"/tasks"), nil, "", http.StatusOK, &resp)
	if len(resp.Items) == 0 {
		t.Fatalf("firstTaskID: no tasks found for job %s", jobID)
	}
	return resp.Items[0].ID
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestEndToEnd is the primary integration test.  It exercises the complete
// lifecycle of a job:
//
//  1. Boot a full sqi-server (SQLite + embedded NATS + scheduler + HTTP).
//  2. Seed a farm and queue via REST.
//  3. Connect a mock worker and publish a RegisterMsg.
//  4. Verify the worker appears online via GET /api/v1/workers.
//  5. Submit a minimal OpenJD job via POST /api/v1/jobs.
//  6. Mock worker pulls the task assignment from the work.assign.<queue> consumer.
//  7. Mock worker publishes a "running" TaskStatusMsg.
//  8. Mock worker publishes a log chunk.
//  9. Mock worker publishes a "succeeded" TaskStatusMsg.
//
// 10. Poll GET /api/v1/jobs/{id} until status == "completed".
// 11. Assert log output is retrievable via GET /api/v1/tasks/{id}/logs.
func TestEndToEnd(t *testing.T) {
	ts := startServer(t)

	farmID, queueID := seedFarmAndQueue(t, ts)

	const workerID = "worker-integ-001"
	natsURL := "nats://" + ts.NATSAddr
	worker := newMockWorker(t, natsURL, workerID, farmID, queueID)

	// Register the worker and start sending periodic heartbeats so the server's
	// heartbeat sweep does not mark it offline during slower CI runs.
	worker.register()
	worker.startHeartbeat(5 * time.Second)
	pollWorkerOnline(t, ts, workerID, 10*time.Second)

	// Submit the job; it should immediately enter pending/running state as
	// tasks become ready and the scheduler finds the online worker.
	jobID := submitJob(t, ts, farmID, queueID)
	t.Logf("submitted job %s", jobID)

	// Pull the task assignment — the scheduler should dispatch within a few
	// hundred milliseconds given the 100 ms AssignInterval set in startServer.
	assign := worker.pullAssignment(15 * time.Second)
	t.Logf("pulled assignment: task=%s attempt=%s", assign.TaskID, assign.AttemptID)

	if assign.JobID != jobID {
		t.Errorf("assignment job ID: got %q, want %q", assign.JobID, jobID)
	}

	// Publish "running" — lets the server record the actual start time and
	// the web UI shows the task as active.
	worker.publishStatus(assign, "running", nil)

	// Publish a log chunk before the terminal status so it arrives first in
	// the SQI_LOGS stream.
	const logLine = "hello from sqi integration test\n"
	worker.publishLogChunk(assign, 1, logLine)

	// Publish "succeeded".
	exitCode := 0
	worker.publishStatus(assign, "succeeded", &exitCode)

	// Poll until the job reaches "completed" (all tasks succeeded).
	finalStatus := pollJobStatus(t, ts, jobID, []string{"completed", "failed", "canceled"}, 20*time.Second)
	if finalStatus != "completed" {
		t.Errorf("job final status: got %q, want %q", finalStatus, "completed")
	}

	// Poll for log output using a retry loop rather than a fixed sleep.  The
	// log chunk and the task-status "succeeded" message flow through separate
	// NATS consumers (SQI_LOGS vs SQI_TASK), so the job may be marked completed
	// before the log chunk is persisted.
	taskID := firstTaskID(t, ts, jobID)
	pollTaskLogs(t, ts, taskID, "hello from sqi", 10*time.Second)
}

// TestWorkerRegistration verifies that a worker registered via NATS appears
// in the GET /api/v1/workers list with the correct metadata.
func TestWorkerRegistration(t *testing.T) {
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)

	natsURL := "nats://" + ts.NATSAddr
	const workerID = "worker-reg-test"
	worker := newMockWorker(t, natsURL, workerID, farmID, queueID)
	worker.register()

	pollWorkerOnline(t, ts, workerID, 10*time.Second)

	// Fetch the specific worker record and check key fields.
	var detail struct {
		ID     string `json:"id"`
		FarmID string `json:"farm_id"`
		Status string `json:"status"`
		OS     string `json:"os"`
	}
	mustDoJSON(t, http.MethodGet, apiURL(ts, "/api/v1/workers/"+workerID), nil, "", http.StatusOK, &detail)

	if detail.ID != workerID {
		t.Errorf("worker id: got %q, want %q", detail.ID, workerID)
	}
	if detail.FarmID != farmID {
		t.Errorf("worker farm_id: got %q, want %q", detail.FarmID, farmID)
	}
	if detail.Status != "online" {
		t.Errorf("worker status: got %q, want %q", detail.Status, "online")
	}
	if detail.OS != "linux" {
		t.Errorf("worker os: got %q, want %q", detail.OS, "linux")
	}
}

// TestJobSubmissionValidation verifies that submitting a malformed OpenJD
// template (empty steps list) returns a 4xx error, not a 5xx, confirming
// that validation fires before the job is persisted.
func TestJobSubmissionValidation(t *testing.T) {
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)

	url := fmt.Sprintf("%s?farm_id=%s&queue_id=%s&owner=test", apiURL(ts, "/api/v1/jobs"), farmID, queueID)
	const badYAML = `specificationVersion: "jobtemplate-2023-09"
name: Bad Job
steps: []
`
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewBufferString(badYAML))
	if err != nil {
		t.Fatalf("TestJobSubmissionValidation: NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-yaml")

	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("TestJobSubmissionValidation: POST /api/v1/jobs: %v", err)
	}
	defer resp.Body.Close()

	// The server must reject the job with a client-error (4xx) status code,
	// not a server error (5xx).  A 201 would indicate validation was bypassed.
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Errorf("POST /api/v1/jobs with empty steps: got status %d, want 4xx", resp.StatusCode)
	}
}
