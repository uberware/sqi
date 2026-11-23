// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/storetest"
	"github.com/uberware/sqi/internal/ws"
)

func TestScopeFilter(t *testing.T) {
	tests := []struct {
		name       string
		principal  auth.Principal
		wantOwner  string
		wantScoped bool
	}{
		{
			name:       "user role is scoped to its own username",
			principal:  auth.Principal{Username: "alice", Roles: []string{"user"}},
			wantOwner:  "alice",
			wantScoped: true,
		},
		{
			name:       "operator sees everything",
			principal:  auth.Principal{Username: "bob", Roles: []string{"operator"}},
			wantOwner:  "",
			wantScoped: false,
		},
		{
			name:       "read-only sees everything",
			principal:  auth.Principal{Username: "carol", Roles: []string{"read-only"}},
			wantOwner:  "",
			wantScoped: false,
		},
		{
			name:       "anonymous superuser is unscoped (auth-off regression)",
			principal:  auth.Principal{Superuser: true, Kind: auth.KindAnonymous},
			wantOwner:  "",
			wantScoped: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := auth.NewContext(context.Background(), tt.principal)
			owner, scoped := scopeFilter(ctx)
			if owner != tt.wantOwner || scoped != tt.wantScoped {
				t.Errorf("scopeFilter() = (%q, %v), want (%q, %v)",
					owner, scoped, tt.wantOwner, tt.wantScoped)
			}
		})
	}
}

// A context with no principal at all must be treated as scoped-with-no-match
// rather than unscoped: failing open here would expose every job if the auth
// middleware were ever misordered.
func TestScopeFilterNoPrincipalFailsClosed(t *testing.T) {
	owner, scoped := scopeFilter(context.Background())
	if !scoped || owner != "" {
		t.Errorf("scopeFilter() = (%q, %v), want (\"\", true)", owner, scoped)
	}
}

// listJobs must pin the owner filter for a scoped principal, overriding
// whatever the client asked for rather than erroring: a scoped caller
// pointing ?owner= at someone else gets their own jobs back.
func TestListJobsScopedOverridesClientOwnerParam(t *testing.T) {
	st := fake.New()
	seedOwnedJob(t, st, "job-alice", "alice")
	seedOwnedJob(t, st, "job-bob", "bob")

	h := newJobHandler(st, openjd.NewSubmitter(st), nil, ws.NoopNotifier{},
		newTestLogger(), testRetryDefaults, false, 0)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/jobs?owner=bob", nil)
	req = req.WithContext(auth.NewContext(req.Context(), auth.Principal{
		Username: "alice", Roles: []string{"user"},
	}))
	rec := httptest.NewRecorder()

	h.listJobs(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp jobListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Items) != 1 || resp.Items[0].ID != "job-alice" {
		t.Errorf("got %d items %v, want only job-alice", len(resp.Items), resp.Items)
	}
}

// TestListJobsNoPrincipalReturnsEmpty drives a real listJobs request with no
// principal in the context end-to-end: the misordered-middleware case
// scopeFilter's fail-closed return covers. Jobs with non-empty owners are
// seeded first so that an unfiltered query would return them; the assertion
// is that it returns none.
func TestListJobsNoPrincipalReturnsEmpty(t *testing.T) {
	st := fake.New()
	seedOwnedJob(t, st, "job-alice", "alice")
	seedOwnedJob(t, st, "job-bob", "bob")

	h := newJobHandler(st, openjd.NewSubmitter(st), nil, ws.NoopNotifier{},
		newTestLogger(), testRetryDefaults, false, 0)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/jobs", nil)
	rec := httptest.NewRecorder()

	h.listJobs(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp jobListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Items) != 0 {
		t.Errorf("got %d items %v, want 0 (no principal must fail closed, not open)", len(resp.Items), resp.Items)
	}
	if resp.Total != 0 {
		t.Errorf("Total = %d, want 0", resp.Total)
	}
	if resp.Offset != 0 {
		t.Errorf("Offset = %d, want 0", resp.Offset)
	}
	if resp.Limit != store.DefaultLimit {
		t.Errorf("Limit = %d, want %d (default)", resp.Limit, store.DefaultLimit)
	}
}

func TestListJobsUnscopedSeesAll(t *testing.T) {
	st := fake.New()
	seedOwnedJob(t, st, "job-alice", "alice")
	seedOwnedJob(t, st, "job-bob", "bob")

	h := newJobHandler(st, openjd.NewSubmitter(st), nil, ws.NoopNotifier{},
		newTestLogger(), testRetryDefaults, false, 0)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/jobs", nil)
	req = req.WithContext(auth.NewContext(req.Context(), auth.Principal{
		Username: "carol", Roles: []string{"operator"},
	}))
	rec := httptest.NewRecorder()

	h.listJobs(rec, req)

	var resp jobListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Errorf("got %d items, want 2", len(resp.Items))
	}
}

// seedOwnedJob inserts a minimal job owned by owner.
//
// It is not named seedJob because internal/api/jobs_test.go defines
// seedJob(t, *fake.Store, store.JobStatus) store.Job in this package.
func seedOwnedJob(t *testing.T, st *fake.Store, id, owner string) {
	t.Helper()
	storetest.Submit(t, st, store.JobSubmission{Job: store.Job{
		ID:     id,
		Name:   id,
		Owner:  owner,
		Status: store.JobStatusPending,
	}})
}

// seedOwnedJobWithTask inserts a minimal job owned by owner, in jobStatus, with
// one task, taskID, in taskStatus under one ready step, in a single submission.
// A task cannot be added to a job that already exists, so a test that needs a
// task seeds the job through this instead of seedOwnedJob.
func seedOwnedJobWithTask(
	t *testing.T, st *fake.Store, id, owner, taskID string, jobStatus store.JobStatus, taskStatus store.TaskStatus,
) {
	t.Helper()
	step := store.Step{ID: "step-" + id, JobID: id, Name: "Step1", Status: store.StepStatusReady}
	storetest.Submit(t, st, store.JobSubmission{
		Job:   store.Job{ID: id, Name: id, Owner: owner, Status: jobStatus},
		Steps: []store.Step{step},
		Tasks: []store.Task{{ID: taskID, JobID: id, StepID: step.ID, Name: taskID, Status: taskStatus}},
	})
}

func TestRequireJobAccess(t *testing.T) {
	tests := []struct {
		name       string
		principal  auth.Principal
		jobOwner   string
		wantStatus int
	}{
		{
			name:       "scoped principal reaches its own job",
			principal:  auth.Principal{Username: "alice", Roles: []string{"user"}},
			jobOwner:   "alice",
			wantStatus: http.StatusOK,
		},
		{
			name:       "scoped principal is refused another owner's job",
			principal:  auth.Principal{Username: "alice", Roles: []string{"user"}},
			jobOwner:   "bob",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "owner match is case-insensitive",
			principal:  auth.Principal{Username: "Alice", Roles: []string{"user"}},
			jobOwner:   "alice",
			wantStatus: http.StatusOK,
		},
		{
			name:       "scoped principal cannot see a job with no owner",
			principal:  auth.Principal{Username: "alice", Roles: []string{"user"}},
			jobOwner:   "",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "operator reaches any job",
			principal:  auth.Principal{Username: "bob", Roles: []string{"operator"}},
			jobOwner:   "alice",
			wantStatus: http.StatusOK,
		},
		{
			name:       "anonymous superuser reaches any job (auth-off regression)",
			principal:  auth.Principal{Superuser: true, Kind: auth.KindAnonymous},
			jobOwner:   "alice",
			wantStatus: http.StatusOK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := fake.New()
			seedOwnedJob(t, st, "job-1", tt.jobOwner)
			az := newAuthz(st, slog.New(slog.DiscardHandler))

			reached := false
			h := az.requireJobAccessByJobID()(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					reached = true
					w.WriteHeader(http.StatusOK)
				},
			))

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, jobRequest(t, "/api/v1/jobs/{id}", "job-1", tt.principal))

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if reached != (tt.wantStatus == http.StatusOK) {
				t.Errorf("handler reached = %v, want %v", reached, tt.wantStatus == http.StatusOK)
			}
		})
	}
}

// A task route resolves ownership through the task's parent job.
func TestRequireJobAccessViaTask(t *testing.T) {
	st := fake.New()
	seedOwnedJobWithTask(t, st, "job-1", "bob", "task-1", store.JobStatusPending, store.TaskStatusReady)
	az := newAuthz(st, slog.New(slog.DiscardHandler))

	h := az.requireJobAccessByTaskID()(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
	))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, jobRequest(t, "/api/v1/tasks/{id}", "task-1",
		auth.Principal{Username: "alice", Roles: []string{"user"}}))

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (task belongs to bob's job)", rec.Code)
	}
}

// A task route's /logs, /attempts, /retry, /cancel children also resolve
// ownership through the task's parent job, like the bare /tasks/{id} route,
// rather than calling GetJob on the task id.
func TestRequireJobAccessViaTaskChildRoute(t *testing.T) {
	st := fake.New()
	seedOwnedJobWithTask(t, st, "job-1", "bob", "task-1", store.JobStatusPending, store.TaskStatusReady)
	az := newAuthz(st, slog.New(slog.DiscardHandler))

	h := az.requireJobAccessByTaskID()(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
	))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, jobRequest(t, "/api/v1/tasks/{id}/logs", "task-1",
		auth.Principal{Username: "alice", Roles: []string{"user"}}))

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (task belongs to bob's job, resolved via /tasks/{id}/logs)", rec.Code)
	}
}

// /api/v1/jobs/{id}/tasks carries a job id in {id} despite the "tasks" in its
// path, so it is resolved as a job route and never calls GetTask on a job id.
// The middleware is chosen at mount time, and this path shape is the one most
// likely to be mis-wired by hand.
func TestRequireJobAccessJobTasksRouteNotConfusedWithTaskRoute(t *testing.T) {
	st := fake.New()
	seedOwnedJob(t, st, "job-1", "alice")
	az := newAuthz(st, slog.New(slog.DiscardHandler))

	reached := false
	h := az.requireJobAccessByJobID()(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		},
	))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, jobRequest(t, "/api/v1/jobs/{id}/tasks", "job-1",
		auth.Principal{Username: "alice", Roles: []string{"user"}}))

	if rec.Code != http.StatusOK || !reached {
		t.Errorf("status = %d, reached = %v; want 200/reached (job-1 is alice's own job, "+
			"the {id} must be treated as a job id here, not passed to GetTask)", rec.Code, reached)
	}
}

// A missing job must 404, not 403: when the id does not exist at all, the
// caller is entitled to be told so.
func TestRequireJobAccessUnknownJob(t *testing.T) {
	st := fake.New()
	az := newAuthz(st, slog.New(slog.DiscardHandler))

	h := az.requireJobAccessByJobID()(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
	))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, jobRequest(t, "/api/v1/jobs/{id}", "nope",
		auth.Principal{Username: "alice", Roles: []string{"user"}}))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// jobRequest builds a request carrying principal, with chi route context set
// so RoutePattern() and URLParam("id") resolve the way the router supplies
// them. pattern is the full accumulated pattern chi.Walk/RoutePattern()
// would report for the route (e.g. "/api/v1/tasks/{id}"), not a bare
// "/tasks/{id}": every route is mounted under r.Route("/api/v1", …)
// (router.go), so RoutePattern() always carries that prefix in production.
func jobRequest(t *testing.T, pattern, id string, principal auth.Principal) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", nil)
	rctx := chi.NewRouteContext()
	rctx.RoutePatterns = []string{pattern}
	rctx.URLParams.Add("id", id)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	return req.WithContext(auth.NewContext(ctx, principal))
}
