// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/storetest"
)

func TestSubmitJob_Paused(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		wantCode   int
		wantStatus string
		wantWake   bool
	}{
		{"paused=true", "&paused=true", http.StatusCreated, "paused", false},
		{"paused=1", "&paused=1", http.StatusCreated, "paused", false},
		{"paused=false", "&paused=false", http.StatusCreated, "pending", true},
		{"omitted", "", http.StatusCreated, "pending", true},
		{"not a boolean", "&paused=yes", http.StatusBadRequest, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := fake.New()
			farm, queue := seedFarmQueue(t, st)
			sched := &fakeScheduler{}
			r := newJobRouter(st, sched)

			req := newReq(t, http.MethodPost,
				"/api/v1/jobs?farm_id="+farm.ID+"&queue_id="+queue.ID+tc.query,
				strings.NewReader(minimalOpenJDJSON("PausedTest")))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d — body: %s", rr.Code, tc.wantCode, rr.Body)
			}
			if tc.wantCode != http.StatusCreated {
				if !strings.Contains(rr.Body.String(), "paused must be true or false") {
					t.Errorf("body = %s, want the paused problem", rr.Body)
				}
				return
			}
			var resp jobResponse
			if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", resp.Status, tc.wantStatus)
			}
			if woke := len(sched.wokenQueue) > 0; woke != tc.wantWake {
				t.Errorf("WakeQueue called = %v, want %v", woke, tc.wantWake)
			}
		})
	}
}

func TestProducts_SubmitPaused(t *testing.T) {
	srv := newProductTestServer(t)
	farmID, queueID := seedProductSubmitPrereqs(t, srv)

	body := jsonBody(t, map[string]any{
		"farm_id": farmID, "queue_id": queueID, "paused": true,
		"parameters": map[string]string{"Command": "echo hi"},
	})
	req := newReq(t, http.MethodPost, "/api/v1/products/script/jobs", bytes.NewReader(body.Bytes()))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("submit status = %d body=%s", rec.Code, rec.Body)
	}
	var resp jobResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "paused" {
		t.Errorf("status = %q, want paused", resp.Status)
	}
}

// seedHeldJob submits a blocked job waiting on an upstream in upstreamStatus,
// in the same farm and queue; the store records its dependency hold.
func seedHeldJob(t *testing.T, st store.Store, upstreamStatus store.JobStatus) store.Job {
	t.Helper()
	up := seedJob(t, st, upstreamStatus)
	jobID, stepID := uuid.NewString(), uuid.NewString()
	return storetest.Submit(t, st, store.JobSubmission{
		Job: store.Job{
			ID: jobID, FarmID: up.FarmID, QueueID: up.QueueID, Name: "held",
			Status: store.JobStatusBlocked, TemplateFormat: store.TemplateFormatJSON,
		},
		DependsOn: []string{up.ID},
		Steps:     []store.Step{{ID: stepID, JobID: jobID, Name: "s1", Status: store.StepStatusPending}},
		Tasks:     []store.Task{{ID: uuid.NewString(), JobID: jobID, StepID: stepID, Name: "t1", Status: store.TaskStatusPending}},
	}).Job
}

// patchAction sends PATCH {"action": action} for jobID and decodes the 200.
func patchAction(t *testing.T, r http.Handler, jobID, action string) jobResponse {
	t.Helper()
	req := newReq(t, http.MethodPatch, "/api/v1/jobs/"+jobID, jsonBody(t, patchJobRequest{Action: action}))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("%s: code = %d, want 200 — body: %s", action, rr.Code, rr.Body)
	}
	var resp jobResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func TestPatchJob_PauseAndResumeHeldJob(t *testing.T) {
	st := fake.New()
	sched := &fakeScheduler{}
	r := newJobRouter(st, sched)
	held := seedHeldJob(t, st, store.JobStatusPending)

	if got := patchAction(t, r, held.ID, "pause"); got.Status != "paused" {
		t.Fatalf("pause of a blocked job: status = %q, want paused", got.Status)
	}
	if got := patchAction(t, r, held.ID, "resume"); got.Status != "blocked" {
		t.Fatalf("resume of a held job: status = %q, want blocked", got.Status)
	}
	if len(sched.reconciledBlocked) != 1 || sched.reconciledBlocked[0] != held.ID {
		t.Fatalf("ReconcileBlockedJob calls = %v, want [%s]", sched.reconciledBlocked, held.ID)
	}
}

// The resume response reports what the reconcile left: a held job whose
// upstreams already completed comes back pending, not blocked.
func TestPatchJob_ResumeReportsReconciledStatus(t *testing.T) {
	st := fake.New()
	held := seedHeldJob(t, st, store.JobStatusCompleted)
	// Stands in for the scheduler's reconcile: the upstream has completed, so
	// the release goes through.
	sched := &fakeScheduler{onReconcileBlocked: func(ctx context.Context, id string) error {
		_, err := st.ReleaseBlockedJob(ctx, id, time.Now().UTC())
		return err
	}}
	r := newJobRouter(st, sched)

	patchAction(t, r, held.ID, "pause")
	if got := patchAction(t, r, held.ID, "resume"); got.Status != "pending" {
		t.Fatalf("resume: status = %q, want pending after the reconcile released it", got.Status)
	}
}
