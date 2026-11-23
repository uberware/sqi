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
// space and an echo command. The mock worker runs no process and publishes
// status messages directly, so the command only has to be valid.
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

// httpClient is the REST helpers' default client. Its 10-second timeout stops
// a test hanging on a wedged server.
var httpClient = &http.Client{Timeout: 10 * time.Second}

// ── Wire types for REST assertions ────────────────────────────────────────────

// These mirror the api package's unexported response types, so REST responses
// decode without test/ importing internal/api.

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
	scheme := ts.Scheme
	if scheme == "" {
		scheme = "http"
	}
	return scheme + "://" + ts.HTTPAddr + path
}

// mustDoJSON performs an HTTP request and decodes the JSON response body into
// dst. It fails the test on a transport error, a status other than
// expectStatus, or a body that does not decode.
func mustDoJSON(t *testing.T, method, url string, body []byte, contentType string, expectStatus int, dst any) {
	t.Helper()
	mustDoJSONClient(t, httpClient, method, url, body, contentType, expectStatus, dst)
}

// mustDoJSONClient is [mustDoJSON] with a caller-supplied client, for a server
// whose certificate the package-default client cannot verify. Same shape as
// waitForReadyzClient in harness_test.go.
func mustDoJSONClient(t *testing.T, client *http.Client, method, url string, body []byte, contentType string, expectStatus int, dst any) {
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

	resp, err := client.Do(req)
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
	mustDoJSONClient(t, clientFor(ts), http.MethodPost, url, []byte(minimalJobYAML), "application/x-yaml", http.StatusCreated, &resp)
	if resp.ID == "" {
		t.Fatal("submitJob: server returned empty job ID")
	}
	return resp.ID
}

// pollJobStatus polls GET /api/v1/jobs/{id} until the job reaches one of the
// target statuses, and returns that status. It fails the test after timeout.
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
		mustDoJSONClient(t, clientFor(ts), http.MethodGet, apiURL(ts, "/api/v1/jobs/"+jobID), nil, "", http.StatusOK, &resp)
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

// pollWorkerOnline polls GET /api/v1/workers until workerID is listed online,
// failing the test after timeout.
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

	// The server generates the ID; only name is required.
	farmBody := []byte(`{"name":"Integration Farm"}`)
	var farmResp struct {
		ID string `json:"id"`
	}
	mustDoJSONClient(t, clientFor(ts), http.MethodPost, apiURL(ts, "/api/v1/farms"), farmBody, "application/json", http.StatusCreated, &farmResp)
	if farmResp.ID == "" {
		t.Fatal("seedFarmAndQueue: server returned empty farm ID")
	}

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
	mustDoJSONClient(t, clientFor(ts), http.MethodPost, apiURL(ts, "/api/v1/queues"), queueBody, "application/json", http.StatusCreated, &queueResp)
	if queueResp.ID == "" {
		t.Fatal("seedFarmAndQueue: server returned empty queue ID")
	}

	return farmResp.ID, queueResp.ID
}

// pollTaskLogs polls GET /api/v1/tasks/{id}/logs until the concatenated log
// output contains wantSubstr, failing the test after timeout. A job can
// complete before its log chunks are stored, because status and logs go
// through different consumers (SQI_TASK and SQI_LOGS).
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

// TestEndToEnd runs a job through its whole lifecycle:
//
//  1. Boot a full sqi-server (SQLite or PostgreSQL + embedded NATS + scheduler + HTTP).
//  2. Seed a farm and queue via REST.
//  3. Connect a mock worker and publish a RegisterMsg.
//  4. Verify the worker appears online via GET /api/v1/workers.
//  5. Submit a minimal OpenJD job via POST /api/v1/jobs.
//  6. Mock worker requests work on work.lease.<worker>.<queue> and receives the assignment.
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

	// Heartbeats keep the server's heartbeat sweep from marking the worker
	// offline on a slow host.
	worker.register()
	worker.startHeartbeat(5 * time.Second)
	pollWorkerOnline(t, ts, workerID, 10*time.Second)

	jobID := submitJob(t, ts, farmID, queueID)
	t.Logf("submitted job %s", jobID)

	assign := worker.pullAssignment(15 * time.Second)
	t.Logf("pulled assignment: task=%s attempt=%s", assign.TaskID, assign.AttemptID)

	if assign.JobID != jobID {
		t.Errorf("assignment job ID: got %q, want %q", assign.JobID, jobID)
	}

	worker.publishStatus(assign, "running", nil)

	const logLine = "hello from sqi integration test\n"
	worker.publishLogChunk(assign, 1, logLine)

	exitCode := 0
	worker.publishStatus(assign, "succeeded", &exitCode)

	finalStatus := pollJobStatus(t, ts, jobID, []string{"completed", "failed", "canceled"}, 20*time.Second)
	if finalStatus != "completed" {
		t.Errorf("job final status: got %q, want %q", finalStatus, "completed")
	}

	// The job can complete before the log chunk is stored (see pollTaskLogs).
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
// template (an empty steps list) returns a 4xx status, not a 5xx or a 201.
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

	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Errorf("POST /api/v1/jobs with empty steps: got status %d, want 4xx", resp.StatusCode)
	}
}
