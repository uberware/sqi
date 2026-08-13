// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
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
// whatever the client asked for rather than erroring — a scoped caller
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
// principal in the context end-to-end — the misordered-middleware threat
// model scopeFilter's fail-closed return is meant to cover. Jobs with real,
// non-empty owners are seeded first so that an unfiltered query would
// obviously return them; the assertion is that it returns none.
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
// NOTE the name: internal/api/jobs_test.go already defines
// seedJob(t, *fake.Store, store.JobStatus) store.Job in this same package.
// Reusing that name would not compile.
func seedOwnedJob(t *testing.T, st *fake.Store, id, owner string) {
	t.Helper()
	if _, err := st.CreateJob(t.Context(), store.Job{
		ID:        id,
		Name:      id,
		Owner:     owner,
		Status:    store.JobStatusPending,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateJob(%s): %v", id, err)
	}
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
	seedOwnedJob(t, st, "job-1", "bob")
	if _, err := st.CreateTask(context.Background(), store.Task{
		ID: "task-1", JobID: "job-1", Status: store.TaskStatusReady,
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
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

// A task route's /logs, /attempts, /retry, /cancel children must also
// resolve via the task's parent job — not just the bare /tasks/{id} route —
// since resolveJob matches on the "/tasks/{id}" segment appearing anywhere in
// the accumulated pattern. Regression test for the bug where the check was a
// HasPrefix("/tasks/") against RoutePattern(), which is always
// "/api/v1/tasks/{id}/..." under the real router and therefore never
// matched, silently routing every task-child request into the job branch
// instead (GetJob(taskID) -> 404).
func TestRequireJobAccessViaTaskChildRoute(t *testing.T) {
	st := fake.New()
	seedOwnedJob(t, st, "job-1", "bob")
	if _, err := st.CreateTask(context.Background(), store.Task{
		ID: "task-1", JobID: "job-1", Status: store.TaskStatusReady,
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
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

// /api/v1/jobs/{id}/tasks carries a JOB id in {id} despite the "tasks" in its
// path. It used to be the one near-collision for the old pattern-matching
// classifier; the middleware is now chosen explicitly at mount time, so this
// pins that the router mounts the job-id variant for it. Kept because the
// path shape is still the one most likely to be mis-wired by hand.
//
// Historical note: /api/v1/jobs/{id}/tasks has no
// "{id}" segment after "tasks", so it must be resolved as a JOB route (the
// URL param is a job id), never mistaken for a /tasks/{id} task route (which
// would wrongly call GetTask on a job id and 404/misclassify ownership).
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
			"resolveJob must treat the {id} as a job id here, not attempt GetTask(job-1))", rec.Code, reached)
	}
}

// A missing job must 404, not 403 — leaking "this id exists but isn't yours"
// vs "this id doesn't exist" is a distinction the caller is entitled to when
// the id doesn't exist at all.
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
// them. pattern MUST be the full accumulated pattern chi.Walk/RoutePattern()
// would report for the route (e.g. "/api/v1/tasks/{id}"), not a bare
// "/tasks/{id}" — every route in this repo is mounted under
// r.Route("/api/v1", …) (router.go), so RoutePattern() always carries that
// prefix in production. A bare pattern here would let a HasPrefix-style bug
// in resolveJob pass silently, exactly as it did before this test helper was
// fixed to match reality (see TestRequireJobAccessViaTaskChildRoute).
func jobRequest(t *testing.T, pattern, id string, principal auth.Principal) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", nil)
	rctx := chi.NewRouteContext()
	rctx.RoutePatterns = []string{pattern}
	rctx.URLParams.Add("id", id)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	return req.WithContext(auth.NewContext(ctx, principal))
}
