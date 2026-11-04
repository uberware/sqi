// SPDX-License-Identifier: AGPL-3.0-or-later

package client_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/internal/testserver"
)

const oneTaskTemplate = `specificationVersion: "jobtemplate-2023-09"
name: Client Test Job
steps:
  - name: Run
    script:
      actions:
        onRun:
          command: echo
          args:
            - "hello"
`

func TestClient_SetupAndSubmit(t *testing.T) {
	srv := testserver.Start(t, testserver.Options{})
	c := client.New(srv.HTTPURL())
	ctx := t.Context()

	farmID, err := c.CreateFarm(ctx, "farm-a")
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queueID, err := c.CreateQueue(ctx, farmID, "queue-a", 10)
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	if _, err := c.CreateUsagePool(ctx, "pool-a", 3); err != nil {
		t.Fatalf("CreateUsagePool: %v", err)
	}

	jobID, status, err := c.SubmitJob(ctx, client.SubmitRequest{
		FarmID: farmID, QueueID: queueID, Template: []byte(oneTaskTemplate), Priority: 60,
	})
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	if status != http.StatusCreated {
		t.Errorf("SubmitJob status = %d, want 201", status)
	}

	job, err := c.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.ID != jobID {
		t.Errorf("GetJob ID = %q, want %q", job.ID, jobID)
	}
	if job.Status != "pending" && job.Status != "ready" {
		t.Errorf("GetJob status = %q, want pending or ready", job.Status)
	}
	if job.TaskCounts["total"] != 1 {
		t.Errorf("task_counts[total] = %d, want 1", job.TaskCounts["total"])
	}
	if job.CreatedAt.IsZero() || job.UpdatedAt.IsZero() {
		t.Errorf("timestamps not decoded: %+v", job)
	}

	page, err := c.ListJobs(ctx, farmID, 10, 0)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != jobID {
		t.Errorf("ListJobs = %+v, want the one submitted job", page)
	}

	tasks, err := c.ListTasks(ctx, jobID, 10, 0)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if tasks.Total != 1 || len(tasks.Items) != 1 {
		t.Fatalf("ListTasks = %+v, want one task", tasks)
	}
	taskID := tasks.Items[0].ID
	attempts, err := c.ListAttempts(ctx, taskID)
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	if len(attempts) != 0 {
		t.Errorf("ListAttempts = %+v, want none before any lease", attempts)
	}
	lines, next, err := c.TaskLogs(ctx, taskID, 0, 100)
	if err != nil {
		t.Fatalf("TaskLogs: %v", err)
	}
	if len(lines) != 0 || next != 0 {
		t.Errorf("TaskLogs = %v, %d; want empty and cursor 0", lines, next)
	}

	if err := c.PauseQueue(ctx, queueID, true); err != nil {
		t.Fatalf("PauseQueue(true): %v", err)
	}
	if err := c.PauseQueue(ctx, queueID, false); err != nil {
		t.Fatalf("PauseQueue(false): %v", err)
	}
	if err := c.CancelJob(ctx, jobID); err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	if err := c.RetryJob(ctx, jobID); err != nil {
		t.Fatalf("RetryJob: %v", err)
	}
	workers, err := c.ListWorkers(ctx, farmID, 10, 0)
	if err != nil {
		t.Fatalf("ListWorkers: %v", err)
	}
	if workers.Total != 0 {
		t.Errorf("ListWorkers total = %d, want 0", workers.Total)
	}
	if err := c.DeleteFarm(ctx, farmID); err == nil {
		t.Error("DeleteFarm of a farm with a queue succeeded, want a conflict")
	}
}

func TestClient_SubmitReturnsStatus(t *testing.T) {
	srv := testserver.Start(t, testserver.Options{})
	c := client.New(srv.HTTPURL())
	ctx := t.Context()

	farmID, err := c.CreateFarm(ctx, "farm-b")
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queueID, err := c.CreateQueue(ctx, farmID, "queue-b", 0)
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}

	tests := []struct {
		name       string
		queueID    string
		template   string
		wantStatus int
		wantInErr  string
	}{
		{"invalid template", queueID, "specificationVersion: nonsense\nname: x\n", http.StatusUnprocessableEntity, "specificationVersion"},
		{"unknown queue", "no-such-queue", oneTaskTemplate, 0, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			jobID, status, err := c.SubmitJob(ctx, client.SubmitRequest{
				FarmID: farmID, QueueID: tc.queueID, Template: []byte(tc.template),
			})
			if err == nil {
				t.Fatalf("SubmitJob succeeded (job %q), want an error", jobID)
			}
			if tc.wantStatus != 0 && status != tc.wantStatus {
				t.Errorf("status = %d, want %d", status, tc.wantStatus)
			}
			if status < 400 {
				t.Errorf("status = %d, want >= 400", status)
			}
			if tc.wantInErr != "" && !strings.Contains(err.Error(), tc.wantInErr) {
				t.Errorf("error %q does not include the server message (%q)", err, tc.wantInErr)
			}
			if jobID != "" {
				t.Errorf("jobID = %q on failure, want empty", jobID)
			}
		})
	}
}

func TestClient_BearerHeader(t *testing.T) {
	tests := []struct {
		name string
		opts []client.Option
		want string
	}{
		{"key set", []client.Option{client.WithAPIKey("k")}, "Bearer k"},
		{"no key", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := make(chan string, 1)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got <- r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"id":"f1"}`)) //nolint:errcheck // test handler
			}))
			defer ts.Close()

			c := client.New(ts.URL, tc.opts...)
			if _, err := c.CreateFarm(t.Context(), "f"); err != nil {
				t.Fatalf("CreateFarm: %v", err)
			}
			select {
			case h := <-got:
				if h != tc.want {
					t.Errorf("Authorization = %q, want %q", h, tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("server saw no request")
			}
		})
	}
}

func TestClient_GetRaw(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("Authorization") != "Bearer k":
			w.WriteHeader(http.StatusUnauthorized)
		case r.Header.Get("Accept") != "":
			w.WriteHeader(http.StatusNotAcceptable)
		case r.URL.Path == "/metrics" && r.URL.Query().Get("x") == "1":
			_, _ = w.Write([]byte("0123456789")) //nolint:errcheck // test handler
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	c := client.New(ts.URL, client.WithAPIKey("k"))

	body, err := c.GetRaw(t.Context(), "/metrics?x=1", 4)
	if err != nil {
		t.Fatalf("GetRaw: %v", err)
	}
	if string(body) != "0123" {
		t.Errorf("body = %q, want the first 4 bytes", body)
	}
	_, err = c.GetRaw(t.Context(), "/missing", 0)
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Errorf("GetRaw of a missing path = %v, want a 404 APIError", err)
	}
}
