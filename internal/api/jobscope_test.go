// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
		newTestLogger(), testRetryDefaults)

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
		newTestLogger(), testRetryDefaults)

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
		newTestLogger(), testRetryDefaults)

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
