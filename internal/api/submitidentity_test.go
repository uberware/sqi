// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/ws"
)

func TestBindSubmitIdentity(t *testing.T) {
	tests := []struct {
		name          string
		principal     auth.Principal
		clientOwner   string
		clientSub     string
		wantOwner     string
		wantSubmitter string
		wantStatus    int
	}{
		{
			name:          "submitter is always the principal, client value discarded",
			principal:     auth.Principal{Username: "alice", Roles: []string{"user"}},
			clientSub:     "someone-else",
			wantOwner:     "alice",
			wantSubmitter: "alice",
		},
		{
			name:          "owner defaults to the submitter",
			principal:     auth.Principal{Username: "alice", Roles: []string{"user"}},
			wantOwner:     "alice",
			wantSubmitter: "alice",
		},
		{
			name:          "owner equal to self is accepted",
			principal:     auth.Principal{Username: "alice", Roles: []string{"user"}},
			clientOwner:   "alice",
			wantOwner:     "alice",
			wantSubmitter: "alice",
		},
		{
			name:          "owner equal to self is accepted case-insensitively",
			principal:     auth.Principal{Username: "Alice", Roles: []string{"user"}},
			clientOwner:   "alice",
			wantOwner:     "Alice",
			wantSubmitter: "Alice",
		},
		{
			name:        "owner other than self without jobs.submit_as is refused",
			principal:   auth.Principal{Username: "alice", Roles: []string{"user"}},
			clientOwner: "bob",
			wantStatus:  http.StatusForbidden,
		},
		{
			name:          "operator may submit on behalf of another user",
			principal:     auth.Principal{Username: "proxy", Roles: []string{"operator"}},
			clientOwner:   "bob",
			wantOwner:     "bob",
			wantSubmitter: "proxy",
		},
		{
			name:          "auth off keeps client values verbatim",
			principal:     auth.Principal{Superuser: true, Kind: auth.KindAnonymous},
			clientOwner:   "alice",
			clientSub:     "pipeline",
			wantOwner:     "alice",
			wantSubmitter: "pipeline",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := auth.NewContext(context.Background(), tt.principal)
			owner, submitter, problem, status := bindSubmitIdentity(ctx, nil, tt.clientOwner, tt.clientSub)

			if status != tt.wantStatus {
				t.Fatalf("status = %d (%q), want %d", status, problem, tt.wantStatus)
			}
			if tt.wantStatus != 0 {
				return
			}
			if owner != tt.wantOwner {
				t.Errorf("owner = %q, want %q", owner, tt.wantOwner)
			}
			if submitter != tt.wantSubmitter {
				t.Errorf("submitter = %q, want %q", submitter, tt.wantSubmitter)
			}
		})
	}
}

// Auth-on submit must persist the principal as submitter regardless of what
// the client put in the query string, and must refuse an owner override from
// a principal that lacks jobs.submit_as.
func TestSubmitJobBindsSubmitterFromPrincipal(t *testing.T) {
	st := fake.New()
	ctx := t.Context()
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "farm-one"}); err != nil {
		t.Fatalf("create farm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "render"}); err != nil {
		t.Fatalf("create queue: %v", err)
	}
	r := newJobRouter(st, &fakeScheduler{})

	body := strings.NewReader(minimalOpenJDJSON("BindSubmitterTest"))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/jobs?farm_id=farm-1&queue_id=queue-1&owner=bob&submitter=bob", body)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.NewContext(req.Context(), auth.Principal{
		Username: "alice", Roles: []string{"user"},
	}))
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (owner=bob without jobs.submit_as) — body: %s", rec.Code, rec.Body)
	}
}

// The owner/submitter split exists so a privileged proxy (e.g. a pipeline
// gateway) can submit on behalf of an artist: the job belongs to the artist
// (Owner) while the proxy's own identity is recorded as Submitter.
func TestSubmitJobOperatorSubmitsOnBehalfOf(t *testing.T) {
	st := fake.New()
	ctx := t.Context()
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "farm-one"}); err != nil {
		t.Fatalf("create farm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "render"}); err != nil {
		t.Fatalf("create queue: %v", err)
	}
	r := newJobRouter(st, &fakeScheduler{})

	body := strings.NewReader(minimalOpenJDJSON("OperatorOnBehalfTest"))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/jobs?farm_id=farm-1&queue_id=queue-1&owner=bob", body)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.NewContext(req.Context(), auth.Principal{
		Username: "proxy", Roles: []string{"operator"},
	}))
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — body: %s", rec.Code, rec.Body)
	}
	var resp jobResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	created, err := st.GetJob(ctx, resp.ID)
	if err != nil {
		t.Fatalf("GetJob(%q): %v", resp.ID, err)
	}
	if created.Owner != "bob" {
		t.Errorf("persisted job Owner = %q, want %q", created.Owner, "bob")
	}
	if created.Submitter != "proxy" {
		t.Errorf("persisted job Submitter = %q, want %q", created.Submitter, "proxy")
	}
}

func TestBindSubmitIdentityValidatesOwner(t *testing.T) {
	known := func(_ context.Context, username string) (string, error) {
		if strings.EqualFold(username, "bob") {
			return "bob", nil
		}
		return "", store.ErrNotFound
	}

	tests := []struct {
		name       string
		lookup     ownerLookup
		owner      string
		wantStatus int
	}{
		{name: "known owner accepted", lookup: known, owner: "bob", wantStatus: 0},
		{name: "unknown owner rejected", lookup: known, owner: "nobody", wantStatus: http.StatusBadRequest},
		{name: "validation disabled accepts anything", lookup: nil, owner: "nobody", wantStatus: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := auth.NewContext(context.Background(), auth.Principal{
				Username: "proxy", Roles: []string{"operator"},
			})
			_, _, problem, status := bindSubmitIdentity(ctx, tt.lookup, tt.owner, "")
			if status != tt.wantStatus {
				t.Errorf("status = %d (%q), want %d", status, problem, tt.wantStatus)
			}
		})
	}
}

// Validation only applies to a submit-as override; the caller's own username
// never needs looking up.
func TestBindSubmitIdentitySelfOwnerSkipsLookup(t *testing.T) {
	called := false
	lookup := func(context.Context, string) (string, error) {
		called = true
		return "", store.ErrNotFound
	}
	ctx := auth.NewContext(context.Background(), auth.Principal{
		Username: "alice", Roles: []string{"user"},
	})
	_, _, _, status := bindSubmitIdentity(ctx, lookup, "alice", "")
	if status != 0 {
		t.Errorf("status = %d, want 0", status)
	}
	if called {
		t.Error("lookup called for the caller's own username")
	}
}

// TestBindSubmitIdentityCanonicalizesOwnerCasing is the regression test for
// M-1: a submit-as override must persist the stored user's canonical casing,
// not whatever casing the client supplied, exactly like the self path already
// does. ownerLookup discarding the looked-up User and returning only nil/err
// let a case variant (e.g. "ALICE" for a user stored as "alice") through
// verbatim, which internal/config/config.go's ValidateJobOwner doc says must
// not happen: Job.Owner is meant to be a trustworthy per-user concurrency-cap
// key, and a case variant is exactly the "own silently uncapped bucket" a
// typo would create.
func TestBindSubmitIdentityCanonicalizesOwnerCasing(t *testing.T) {
	lookup := func(_ context.Context, username string) (string, error) {
		if strings.EqualFold(username, "alice") {
			return "alice", nil // the stored, canonical casing
		}
		return "", store.ErrNotFound
	}
	ctx := auth.NewContext(context.Background(), auth.Principal{
		Username: "proxy", Roles: []string{"operator"},
	})

	owner, _, problem, status := bindSubmitIdentity(ctx, lookup, "ALICE", "")
	if status != 0 {
		t.Fatalf("status = %d (%q), want 0", status, problem)
	}
	if owner != "alice" {
		t.Errorf("owner = %q, want %q (canonical casing from the store, not the client's)", owner, "alice")
	}
}

// TestSubmitJobOperatorSubmitAsCanonicalizesOwnerCasing drives the same
// regression end-to-end through the real job-submission handler with owner
// validation enabled, proving the persisted Job.Owner is canonicalized rather
// than the client-supplied casing.
func TestSubmitJobOperatorSubmitAsCanonicalizesOwnerCasing(t *testing.T) {
	st := fake.New()
	ctx := t.Context()
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "farm-one"}); err != nil {
		t.Fatalf("create farm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "render"}); err != nil {
		t.Fatalf("create queue: %v", err)
	}
	if _, err := st.CreateUser(ctx, store.User{ID: "u-alice", Username: "alice"}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	sub := openjd.NewSubmitter(st)
	h := newJobHandler(st, sub, &fakeScheduler{}, ws.NoopNotifier{}, newTestLogger(), testRetryDefaults, true)
	r := chi.NewRouter()
	r.Post("/api/v1/jobs", h.submitJob)

	body := strings.NewReader(minimalOpenJDJSON("CanonicalCasingTest"))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/jobs?farm_id=farm-1&queue_id=queue-1&owner=ALICE", body)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.NewContext(req.Context(), auth.Principal{
		Username: "proxy", Roles: []string{"operator"},
	}))
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — body: %s", rec.Code, rec.Body)
	}
	var resp jobResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	created, err := st.GetJob(ctx, resp.ID)
	if err != nil {
		t.Fatalf("GetJob(%q): %v", resp.ID, err)
	}
	if created.Owner != "alice" {
		t.Errorf("persisted job Owner = %q, want %q (canonical casing)", created.Owner, "alice")
	}
}

// Hardening (raised against Task 5): the auth-off passthrough must be keyed
// on the principal actually being the anonymous/auth-off identity, never on
// an empty Username. An authenticated (non-anonymous) principal with no
// Username is a latent possibility (a future LDAP/OIDC authenticator, or
// auth.KindService) and must still be run through the jobs.submit_as check
// rather than silently bypassing it via the old `p.Username == ""` proxy.
func TestBindSubmitIdentityAuthenticatedEmptyUsernameStillEnforcesSubmitAs(t *testing.T) {
	ctx := auth.NewContext(context.Background(), auth.Principal{
		Kind: auth.KindUser, Roles: []string{"user"}, // no jobs.submit_as
	})
	_, _, problem, status := bindSubmitIdentity(ctx, nil, "bob", "")
	if status != http.StatusForbidden {
		t.Errorf("status = %d (%q), want 403 (authenticated empty-username principal must not bypass submit_as)",
			status, problem)
	}
}
